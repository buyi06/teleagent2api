package handler

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"teleagent2api/internal/config"
	"teleagent2api/internal/middleware"
	"teleagent2api/internal/proxy"
)

// modelCreated is a stable timestamp for the /v1/models response.
var modelCreated = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

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
			data = append(data, map[string]any{
				"id":       m,
				"object":   "model",
				"created":  modelCreated,
				"owned_by": "TeleAgent",
				"name":     m,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

func ChatCompletions(up *proxy.UpstreamProxy, client *http.Client, retryCount int) http.HandlerFunc {
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

		upstreamReq, err := up.BuildRequest(r, body)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to build upstream request",
				slog.String("error", err.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		var resp *http.Response
		for attempt := 0; attempt <= retryCount; attempt++ {
			if attempt > 0 {
				slog.WarnContext(r.Context(), "retrying upstream request",
					slog.Int("attempt", attempt),
					slog.Int("max_retries", retryCount),
				)
				// Rebuild request: previous attempt consumed the body context.
				upstreamReq, err = up.BuildRequest(r, body)
				if err != nil {
					slog.ErrorContext(r.Context(), "failed to rebuild upstream request",
						slog.String("error", err.Error()),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
			}
			resp, err = client.Do(upstreamReq)
			if err != nil {
				if attempt < retryCount {
					continue
				}
				slog.ErrorContext(r.Context(), "upstream request failed after retries",
					slog.String("error", err.Error()),
					slog.Int("attempts", attempt+1),
				)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			// Retry on server-side errors (5xx); client errors (4xx) are not retriable.
			if resp.StatusCode >= 500 && attempt < retryCount {
				resp.Body.Close()
				resp = nil
				continue
			}
			break
		}
		defer resp.Body.Close()

		slog.InfoContext(r.Context(), "upstream responded",
			slog.Int("upstream_status", resp.StatusCode),
		)

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			flushCopy(w, r, resp.Body)
			return
		}
		io.Copy(w, resp.Body)
	}
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

func flushCopy(w http.ResponseWriter, r *http.Request, body io.Reader) {
	flusher, ok := w.(http.Flusher)
	reader := bufio.NewReader(body)
	ctx := r.Context()

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := w.Write(line); writeErr != nil {
				slog.WarnContext(ctx, "flushCopy: client disconnected",
					slog.String("error", writeErr.Error()),
					slog.String("request_id", middleware.GetRequestID(ctx)),
				)
				return
			}
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.WarnContext(ctx, "flushCopy: upstream read error",
					slog.String("error", err.Error()),
				)
			}
			return
		}

		// Respect client disconnect.
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "flushCopy: context cancelled, stopping copy")
			return
		default:
		}
	}
}