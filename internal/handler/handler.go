package handler

import (
	"bufio"
	"bytes"
	"context"
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
		body = adapter.SanitizeRequestWithOptions(body, cfg.ModelMeta, cfg.MinOutputTokens)

		// Detect if client requested streaming
		var reqStruct struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &reqStruct)
		transformOpts := adapter.TransformOptions{
			ExposeReasoning:          cfg.ExposeReasoning,
			ReasoningContentFallback: cfg.ReasoningToContent,
			ModelAlias:               reqStruct.Model,
		}

		// Total attempts = initial attempt + configured upstream retries +
		// configured empty-response retries. Keep these knobs separate so
		// TELEAGENT_RETRY_COUNT=0 really means no generic retry.
		maxAttempts := 1 + cfg.RetryCount + cfg.EmptyRetryCount

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

			// If upstream returned an error (4xx/5xx), map business error codes to proper HTTP status
			if resp.StatusCode >= 400 {
				raw, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr != nil {
					slog.ErrorContext(r.Context(), "failed to read error response",
						slog.String("error", readErr.Error()),
					)
					http.Error(w, "bad gateway", http.StatusBadGateway)
					return
				}
				mappedStatus := mapUpstreamErrorCode(resp.StatusCode, raw)
				copyHeaders(w.Header(), resp.Header)
				w.Header().Del("Content-Length")
				w.WriteHeader(mappedStatus)
				w.Write(raw)
				return
			}

			isStream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

			// --- Non-streaming: buffer, check for empty, retry if needed ---
			if !isStream {
				if cfg.ChunkTimeout > 0 {
					resp.Body = withReadIdleTimeout(resp.Body, cfg.ChunkTimeout)
				}
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
				transformed := adapter.TransformNonStreamingResponseWithOptions(raw, transformOpts)
				if reqStruct.Stream {
					// Some upstream paths occasionally return a valid JSON chat
					// completion even when stream=true. Convert it to a small SSE
					// stream so OpenAI streaming clients do not choke on JSON.
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("X-Accel-Buffering", "no")
					transformed = adapter.TransformNonStreamingResponseToSSEWithOptions(raw, transformOpts)
				}
				w.WriteHeader(resp.StatusCode)
				w.Write(transformed)
				return
			}

			// --- Streaming: buffer until first useful content, then flush-copy ---
			state := adapter.NewStreamChunkStateWithOptions(transformOpts)
			if cfg.ChunkTimeout > 0 {
				resp.Body = withReadIdleTimeout(resp.Body, cfg.ChunkTimeout)
			}
			committed := transformFlushCopyWithPrecommit(w, r, resp, state)
			resp.Body.Close()

			// If stream produced zero useful chunks before committing response
			// headers, retry with a different credential. This avoids returning
			// a header-only / [DONE]-only stream to clients.
			if !committed && !state.HasContent() && attempt < maxAttempts-1 {
				slog.WarnContext(r.Context(), "upstream stream was empty, retrying",
					slog.Int("attempt", attempt+1),
				)
				continue
			}
			if !committed {
				http.Error(w, "empty upstream stream", http.StatusBadGateway)
			}
			return
		}

		// Should not reach here, but just in case
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

// mapUpstreamErrorCode maps TeleAgent business error codes to proper HTTP status codes.
// TeleAgent returns HTTP 500 for business errors with a code like 40001, 50001, etc.
// We remap these to semantically correct HTTP status codes.
func mapUpstreamErrorCode(httpStatus int, body []byte) int {
	var errResp struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &errResp) != nil || errResp.Code == 0 {
		return httpStatus
	}

	code := errResp.Code
	switch {
	case code >= 40000 && code < 50000:
		switch code {
		case 40001:
			return http.StatusBadRequest
		case 40101, 40301:
			return http.StatusUnauthorized
		case 40401:
			return http.StatusNotFound
		case 42901:
			return http.StatusTooManyRequests
		default:
			return http.StatusBadRequest
		}
	case code >= 50000 && code < 60000:
		// Model not found / no route is a client error (wrong model name),
		// not a server error.
		switch code {
		case 50001:
			return http.StatusNotFound
		default:
			return http.StatusBadGateway
		}
	default:
		return httpStatus
	}
}

// transformFlushCopy reads SSE lines from upstream, transforms each
// payload to be OpenAI-compatible, and flushes to the client.
func transformFlushCopyWithPrecommit(w http.ResponseWriter, r *http.Request, resp *http.Response, state *adapter.StreamChunkState) bool {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	ctx := r.Context()
	var pending bytes.Buffer
	committed := false

	commit := func() bool {
		if committed {
			return true
		}
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Del("Content-Length")
		w.Header().Del("Transfer-Encoding")
		w.WriteHeader(resp.StatusCode)
		if pending.Len() > 0 {
			if _, err := w.Write(pending.Bytes()); err != nil {
				slog.WarnContext(ctx, "stream precommit write failed",
					slog.String("error", err.Error()),
					slog.String("request_id", middleware.GetRequestID(ctx)),
				)
				return false
			}
			pending.Reset()
		}
		if flusher != nil {
			flusher.Flush()
		}
		committed = true
		return true
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			transformed, skip := transformSSELine(line, state)
			if !skip {
				if !committed {
					// Do not commit on comments/blank/[DONE]-only prelude. Wait
					// for the first useful transformed data chunk, so empty
					// streams can still be retried with another credential.
					if isUsefulSSELine(transformed) {
						pending.Write(transformed)
						if !commit() {
							return committed
						}
					} else {
						pending.Write(transformed)
					}
				} else {
					if _, writeErr := w.Write(transformed); writeErr != nil {
						slog.WarnContext(ctx, "transformFlushCopy: client disconnected",
							slog.String("error", writeErr.Error()),
							slog.String("request_id", middleware.GetRequestID(ctx)),
						)
						return committed
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.WarnContext(ctx, "transformFlushCopy: upstream read error",
					slog.String("error", err.Error()),
				)
			}
			return committed
		}

		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "transformFlushCopy: context cancelled, stopping copy")
			return committed
		default:
		}
	}
}

func isUsefulSSELine(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return false
	}
	var chunk struct {
		Choices []struct {
			Delta        map[string]json.RawMessage `json:"delta"`
			FinishReason json.RawMessage            `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return true
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" && string(chunk.Usage) != "{}" {
		return true
	}
	for _, choice := range chunk.Choices {
		if len(choice.FinishReason) > 0 && string(choice.FinishReason) != "null" && string(choice.FinishReason) != `""` {
			return true
		}
		for _, key := range []string{"content", "reasoning_content"} {
			if raw, ok := choice.Delta[key]; ok {
				var s string
				if json.Unmarshal(raw, &s) != nil || s != "" {
					return true
				}
			}
		}
		if raw, ok := choice.Delta["tool_calls"]; ok && len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" {
			return true
		}
	}
	return false
}

type readIdleTimeoutBody struct {
	body    io.ReadCloser
	timeout time.Duration
}

func withReadIdleTimeout(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	return &readIdleTimeoutBody{body: body, timeout: timeout}
}

func (b *readIdleTimeoutBody) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := b.body.Read(p)
		ch <- result{n: n, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-ctx.Done():
		_ = b.body.Close()
		return 0, ctx.Err()
	}
}

func (b *readIdleTimeoutBody) Close() error {
	return b.body.Close()
}

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

// transformSSELine transforms a single SSE line. Only "data:" lines with
// JSON payloads are transformed; everything else passes through unchanged.
// Returns (output, skip). If skip is true, this line should be dropped.
func transformSSELine(line []byte, state *adapter.StreamChunkState) ([]byte, bool) {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line, false
	}

	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))

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
