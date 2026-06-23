package handler

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"teleagent2api/internal/adapter"
	"teleagent2api/internal/config"
	"teleagent2api/internal/middleware"
	"teleagent2api/internal/proxy"
)

// modelCreated is a stable timestamp for the /v1/models response.
var modelCreated = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

// credCounter is used for round-robin credential rotation.
var credCounter uint64

func Health() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}

func Models(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		data := make([]map[string]any, 0, len(cfg.Models))
		for _, m := range cfg.Models {
			entry := map[string]any{
				"id":       m,
				"object":   "model",
				"created":  modelCreated,
				"owned_by": "TeleAgent",
				"name":     m,
			}
			if meta, ok := cfg.ModelMeta[m]; ok {
				entry["context_length"] = meta.ContextLen
				entry["max_output_tokens"] = meta.MaxOutput
				entry["tool_call"] = meta.ToolCall
				entry["tool_stream"] = meta.ToolStream
				entry["reasoning"] = meta.Reasoning
				entry["temperature"] = meta.Temperature
			}
			data = append(data, entry)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

func ChatCompletions(up *proxy.UpstreamProxy, client *http.Client, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to read request body",
				slog.String("error", err.Error()),
			)
			http.Error(w, "read body error", http.StatusBadRequest)
			return
		}

		// Sanitize request: strip unsupported params that cause upstream errors
		// and cap max_tokens to model limits
		body = adapter.SanitizeRequest(body, cfg.ModelMeta)

		// Detect if client requested streaming
		var reqStruct struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &reqStruct)

		// Total attempts = normal retries + 1 extra attempt for empty response retry
		maxAttempts := cfg.RetryCount + 2 // +1 for initial, +1 for empty-response retry

		// Round-robin credential selection
		credIdx := atomic.AddUint64(&credCounter, 1) % uint64(len(cfg.Credentials))
		cred := cfg.Credentials[credIdx]

		for attempt := 0; attempt < maxAttempts; attempt++ {
			if attempt > 0 {
				credIdx = atomic.AddUint64(&credCounter, 1) % uint64(len(cfg.Credentials))
				cred = cfg.Credentials[credIdx]
			}

			upstreamReq, err := up.BuildRequest(r, body, cred)
			if err != nil {
				slog.ErrorContext(r.Context(), "failed to build upstream request",
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			resp, err := client.Do(upstreamReq)
			if err != nil {
				if attempt < maxAttempts-1 {
					continue
				}
				slog.ErrorContext(r.Context(), "upstream request failed after retries",
					slog.String("error", err.Error()),
					slog.Int("attempts", attempt+1),
				)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}

			if resp.StatusCode >= 500 && attempt < maxAttempts-1 {
				resp.Body.Close()
				continue
			}

			slog.InfoContext(r.Context(), "upstream responded",
				slog.Int("upstream_status", resp.StatusCode),
			)

			// If upstream returned a client error (4xx), pass through — no retry
			if resp.StatusCode >= 400 {
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				resp.Body.Close()
				return
			}

			isStream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

			// --- Non-streaming: buffer, check for empty, retry if needed ---
			if !isStream && !reqStruct.Stream {
				raw, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr != nil {
					slog.ErrorContext(r.Context(), "failed to read upstream response",
						slog.String("error", readErr.Error()),
					)
					http.Error(w, "bad gateway", http.StatusBadGateway)
					return
				}

				if adapter.IsEmptyResponse(raw) && attempt < maxAttempts-1 {
					slog.WarnContext(r.Context(), "upstream returned empty response, retrying",
						slog.Int("attempt", attempt+1),
						slog.Int("body_len", len(raw)),
					)
					continue
				}

				copyHeaders(w.Header(), resp.Header)
				w.Header().Del("Content-Length")
				w.Header().Del("Transfer-Encoding")
				w.WriteHeader(resp.StatusCode)
				transformed := adapter.TransformNonStreamingResponse(raw, cfg.ReasoningMode)
				w.Write(transformed)
				return
			}

			// --- Streaming: transform chunks, routing reasoning per mode ---
			copyHeaders(w.Header(), resp.Header)
			w.Header().Del("Content-Length")
			w.Header().Del("Transfer-Encoding")
			w.WriteHeader(resp.StatusCode)

			proc := adapter.NewStreamProcessor(cfg.ReasoningMode)
			hasContent := streamCopy(w, r, resp.Body, proc, cfg)
			resp.Body.Close()

			// If stream produced zero content chunks, we cannot retry because the
			// response headers were already committed. Log for diagnostics.
			if !hasContent && attempt < maxAttempts-1 {
				slog.WarnContext(r.Context(), "empty stream cannot be retried (headers already sent)",
					slog.Int("attempt", attempt+1),
					slog.String("request_id", middleware.GetRequestID(r.Context())),
				)
			}
			return
		}

		// Should not reach here, but just in case
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

// streamCopy reads SSE lines from upstream, transforms each via the
// StreamProcessor (which may emit multiple OpenAI chunks per upstream chunk),
// and flushes to the client. It logs periodic progress for diagnostics and
// returns whether any content/reasoning was emitted.
func streamCopy(w http.ResponseWriter, r *http.Request, body io.Reader, proc *adapter.StreamProcessor, cfg config.Config) bool {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(body)
	ctx := r.Context()
	reqID := middleware.GetRequestID(ctx)

	start := time.Now()
	lastLog := start
	firstByteLogged := false
	var chunks, bytesOut int

	writeOut := func(b []byte) bool {
		n, err := w.Write(b)
		bytesOut += n
		if err != nil {
			slog.WarnContext(ctx, "streamCopy: client disconnected",
				slog.String("error", err.Error()),
				slog.String("request_id", reqID),
			)
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !firstByteLogged {
				firstByteLogged = true
				slog.InfoContext(ctx, "stream first byte",
					slog.String("request_id", reqID),
					slog.String("reasoning_mode", cfg.ReasoningMode),
					slog.Duration("first_byte_after", time.Since(start)),
				)
			}
			for _, out := range transformSSELineMulti(line, proc) {
				if !writeOut(out) {
					return proc.HasContent()
				}
				chunks++
			}
		}

		if cfg.StreamLogEvery > 0 && time.Since(lastLog) >= cfg.StreamLogEvery {
			lastLog = time.Now()
			slog.InfoContext(ctx, "stream progress",
				slog.String("request_id", reqID),
				slog.String("reasoning_mode", cfg.ReasoningMode),
				slog.Duration("elapsed", time.Since(start)),
				slog.Int("chunks", chunks),
				slog.Int("bytes", bytesOut),
			)
		}

		if err != nil {
			// Flush any buffered partial reasoning/answer at end of stream.
			for _, out := range proc.Flush() {
				if !writeOut(out) {
					return proc.HasContent()
				}
				chunks++
			}
			if err != io.EOF {
				slog.WarnContext(ctx, "streamCopy: upstream read error",
					slog.String("error", err.Error()),
					slog.String("request_id", reqID),
				)
			}
			slog.InfoContext(ctx, "stream completed",
				slog.String("request_id", reqID),
				slog.String("reasoning_mode", cfg.ReasoningMode),
				slog.Duration("elapsed", time.Since(start)),
				slog.Int("chunks", chunks),
				slog.Int("bytes", bytesOut),
			)
			return proc.HasContent()
		}

		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "stream cancelled",
				slog.String("request_id", reqID),
				slog.String("reasoning_mode", cfg.ReasoningMode),
				slog.Duration("elapsed", time.Since(start)),
				slog.Int("chunks", chunks),
				slog.Int("bytes", bytesOut),
			)
			return proc.HasContent()
		default:
		}
	}
}

// transformSSELineMulti transforms a single upstream SSE line into zero or more
// OpenAI-compatible "data: ...\n\n" events. Blank separator lines are dropped
// (we emit our own terminators); non-data lines pass through unchanged.
func transformSSELineMulti(line []byte, proc *adapter.StreamProcessor) [][]byte {
	s := strings.TrimRight(string(line), "\r\n")
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "data: ") {
		return [][]byte{line}
	}
	payload := strings.TrimPrefix(s, "data: ")
	if payload == "[DONE]" {
		return [][]byte{[]byte("data: [DONE]\n\n")}
	}
	outs := proc.ProcessChunk([]byte(payload))
	if len(outs) == 0 {
		return nil
	}
	res := make([][]byte, 0, len(outs))
	for _, pld := range outs {
		buf := make([]byte, 0, len(pld)+8)
		buf = append(buf, "data: "...)
		buf = append(buf, pld...)
		buf = append(buf, '\n', '\n')
		res = append(res, buf)
	}
	return res
}

// proxyResponseHeaderBlacklist lists upstream headers that should never be
// forwarded to the client.
var proxyResponseHeaderBlacklist = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"set-cookie":          {},
	"x-frame-options":     {},
	"x-message-id":        {},
	"x-session-id":        {},
	"x-request-id":        {},
	"x-trace-id":          {},
	"x-nginx-header":      {},
	"vary":                {},
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := proxyResponseHeaderBlacklist[strings.ToLower(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	dst.Set("Connection", "close")
}
