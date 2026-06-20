package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsEmptyResponseTreatsToolCallsAsContent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}`)
	if IsEmptyResponse(body) {
		t.Fatal("tool_calls-only response must not be treated as empty")
	}
}

func TestTransformChunkKeepsFinishReasonAndUsage(t *testing.T) {
	state := NewStreamChunkState()
	body := []byte(`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"extra":9}}`)
	out, skip := state.TransformChunk(body)
	if skip {
		t.Fatal("finish_reason chunk must not be skipped")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason lost: %#v", choice)
	}
	usage := got["usage"].(map[string]any)
	if _, ok := usage["extra"]; ok {
		t.Fatalf("non-standard usage field was not stripped: %#v", usage)
	}
}

func TestTransformNonStreamingResponseToSSE(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	out := string(TransformNonStreamingResponseToSSE(body))
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "chat.completion.chunk") || !strings.Contains(out, "hello") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("unexpected SSE output: %s", out)
	}
}
