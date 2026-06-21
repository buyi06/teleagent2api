package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"teleagent2api/internal/adapter"
	"teleagent2api/internal/config"
	"teleagent2api/internal/proxy"
)

func TestTransformFlushCopyWithPrecommitDoesNotCommitDoneOnlyStream(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
	}

	committed := transformFlushCopyWithPrecommit(rec, req, resp, adapter.NewStreamChunkState())
	if committed {
		t.Fatal("DONE-only stream should not commit response")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("recorder should remain uncommitted/default status, got %d", rec.Code)
	}
}

func TestTransformFlushCopyWithPrecommitCommitsUsefulStream(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
				"data: [DONE]\n\n",
		)),
	}

	committed := transformFlushCopyWithPrecommit(rec, req, resp, adapter.NewStreamChunkState())
	if !committed {
		t.Fatal("useful stream should commit response")
	}
	if !strings.Contains(rec.Body.String(), `"hi"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected stream body: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content-type: %s", got)
	}
}

func TestChatCompletionsHidesReasoningRaisesTokensAndAliasesModel(t *testing.T) {
	var upstreamMaxTokens int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamMaxTokens = req.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"upstream-id",
			"object":"chat.completion",
			"created":1,
			"model":"glm-5-turbo",
			"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"hidden thinking"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"extra":9}
		}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		BaseURL:            upstream.URL,
		AppVersion:         "2.0.0",
		UserAgent:          "test",
		UpstreamAPIKey:     "upstream-key",
		Credentials:        []config.Credential{{Token: "aaa.bbb.ccc", DeviceID: "device", InstallID: "install"}},
		ModelMeta:          map[string]config.ModelMeta{"chat-flash": {ID: "chat-flash", MaxOutput: 65536}},
		MinOutputTokens:    1024,
		ExposeReasoning:    false,
		ReasoningToContent: false,
	}
	up, err := proxy.NewUpstreamProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"model":"chat-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	ChatCompletions(up, upstream.Client(), cfg)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamMaxTokens != 1024 {
		t.Fatalf("max_tokens was not raised before upstream request: %d", upstreamMaxTokens)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "chat-flash" {
		t.Fatalf("response model was not aliased: %#v", got["model"])
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if _, ok := msg["reasoning_content"]; ok {
		t.Fatalf("reasoning_content leaked to client: %#v", msg)
	}
	if msg["content"] != "" {
		t.Fatalf("reasoning_content was copied into content: %#v", msg)
	}
	usage := got["usage"].(map[string]any)
	if _, ok := usage["extra"]; ok {
		t.Fatalf("non-standard usage leaked to client: %#v", usage)
	}
}
