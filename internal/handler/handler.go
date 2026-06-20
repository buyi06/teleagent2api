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
				transformed := adapter.TransformNonStreamingResponse(raw)
				w.Write(transformed)
				return
			}

			// --- Streaming: copy with flush, check if stream was empty ---
			copyHeaders(w.Header(), resp.Header)
			w.Header().Del("Content-Length")
			w.Header().Del("Transfer-Encoding")
			w.WriteHeader(resp.StatusCode)

			state := adapter.NewStreamChunkState()
			transformFlushCopy(w, r, resp.Body, state)
			resp.Body.Close()

			// If stream produced zero content chunks, retry with different credential
			if !state.HasContent() && attempt < maxAttempts-1 {
				slog.WarnContext(r.Context(), "upstream stream was empty, retrying",
					slog.Int("attempt", attempt+1),
				)
				// Can't retry: response headers already sent to client.
				// Log for diagnostics. Future improvement: buffer first N chunks
				// before committing to the response writer.
				slog.WarnContext(r.Context(), "empty stream cannot be retried (headers already sent)",
					slog.String("request_id", middleware.GetRequestID(r.Context())),
				)
			}
			return
		}

		// Should not reach here, but just in case
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}


// transformFlushCopy reads SSE lines from upstream, transforms each data
// payload to be OpenAI-compatible, and flushes to the client.
func transformFlushCopy(w http.ResponseWriter, r *http.Request, body io.Reader, state *adapter.StreamChunkState) {
	flusher, ok := w.(http.Flusher)
	reader := bufio.NewReader(body)
	ctx := r.Context()

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			transformed, skip := transformSSELine(line, state)
			if !skip {
				if _, writeErr := w.Write(transformed); writeErr != nil {
					slog.WarnContext(ctx, "transformFlushCopy: client disconnected",
						slog.String("error", writeErr.Error()),
						slog.String("request_id", middleware.GetRequestID(ctx)),
					)
					return
				}
				if ok {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.WarnContext(ctx, "transformFlushCopy: upstream read error",
					slog.String("error", err.Error()),
				)
			}
			return
		}

		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "transformFlushCopy: context cancelled, stopping copy")
			return
		default:
		}
	}
}

// transformSSELine transforms a single SSE line. Only "data: " lines with
// JSON payloads are transformed; everything else passes through unchanged.
// Returns (output, skip). If skip is true, this line should be dropped.
func transformSSELine(line []byte, state *adapter.StreamChunkState) ([]byte, bool) {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(trimmed, "data: ") {
		return line, false
	}

	payload := strings.TrimPrefix(trimmed, "data: ")

	// "data: [DONE]" passes through as-is
	if payload == "[DONE]" {
		return line, false
	}

	transformed, skip := state.TransformChunk([]byte(payload))
	if skip {
		return nil, true
	}

	var sb strings.Builder
	sb.WriteString("data: ")
	sb.Write(transformed)
	sb.WriteString("\n")
	return []byte(sb.String()), false
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
