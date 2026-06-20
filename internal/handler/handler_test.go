package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"teleagent2api/internal/adapter"
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
