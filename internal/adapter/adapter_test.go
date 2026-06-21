package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"teleagent2api/internal/config"
)

func TestSanitizeRequestRaisesSmallMaxTokensAndCapsToModelLimit(t *testing.T) {
	meta := map[string]config.ModelMeta{
		"chat-flash": {ID: "chat-flash", MaxOutput: 2048},
		"chat-lite":  {ID: "chat-lite", MaxOutput: 512},
	}

	out := SanitizeRequestWithOptions([]byte(`{"model":"chat-flash","messages":[],"max_tokens":16,"logprobs":true}`), meta, 1024)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"].(float64) != 1024 {
		t.Fatalf("small max_tokens was not raised: %#v", got["max_tokens"])
	}
	if _, ok := got["logprobs"]; ok {
		t.Fatalf("unsupported field was not stripped: %#v", got)
	}

	out = SanitizeRequestWithOptions([]byte(`{"model":"chat-lite","messages":[],"max_tokens":999999}`), meta, 1024)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"].(float64) != 512 {
		t.Fatalf("max_tokens was not capped to model limit: %#v", got["max_tokens"])
	}
}

func TestSanitizeRequestAddsMinOutputTokensWhenMissing(t *testing.T) {
	meta := map[string]config.ModelMeta{
		"chat-flash": {ID: "chat-flash", MaxOutput: 2048},
	}

	out := SanitizeRequestWithOptions([]byte(`{"model":"chat-flash","messages":[]}`), meta, 1024)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"].(float64) != 1024 {
		t.Fatalf("missing max_tokens was not defaulted to min output tokens: %#v", got)
	}
}

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

func TestTransformNonStreamingHidesReasoningAndDoesNotFallbackToContent(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","model":"glm-5-turbo","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"hidden thinking"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	out := TransformNonStreamingResponseWithOptions(body, TransformOptions{ModelAlias: "chat-flash"})

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "chat-flash" {
		t.Fatalf("model alias was not applied: %#v", got["model"])
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["reasoning_content"]; ok {
		t.Fatalf("reasoning_content should be hidden by default options: %#v", msg)
	}
	if msg["content"] != "" {
		t.Fatalf("reasoning_content leaked into content: %#v", msg["content"])
	}
}

func TestTransformNonStreamingCanOptIntoReasoningFallback(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"visible if opted in"}}]}`)
	out := TransformNonStreamingResponseWithOptions(body, TransformOptions{
		ExposeReasoning:          true,
		ReasoningContentFallback: true,
	})

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "visible if opted in" || msg["reasoning_content"] != "visible if opted in" {
		t.Fatalf("opt-in reasoning fallback did not preserve legacy behavior: %#v", msg)
	}
}

func TestTransformChunkHidesReasoningOnlyChunks(t *testing.T) {
	state := NewStreamChunkStateWithOptions(TransformOptions{ModelAlias: "chat-flash"})
	body := []byte(`{"id":"1","object":"chat.completion.chunk","model":"glm-5-turbo","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"}}]}`)
	out, skip := state.TransformChunk(body)
	if !skip {
		t.Fatalf("reasoning-only chunk should be skipped when reasoning is hidden, got: %s", out)
	}
}

func TestTransformChunkKeepsFinishChunkWhenReasoningIsHidden(t *testing.T) {
	state := NewStreamChunkStateWithOptions(TransformOptions{ModelAlias: "chat-flash"})
	body := []byte(`{"id":"1","object":"chat.completion.chunk","model":"glm-5-turbo","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"extra":9}}`)
	out, skip := state.TransformChunk(body)
	if skip {
		t.Fatal("finish chunk must not be skipped even when reasoning is hidden")
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "chat-flash" {
		t.Fatalf("model alias was not applied to stream chunk: %#v", got["model"])
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason lost: %#v", choice)
	}
	delta := choice["delta"].(map[string]any)
	if _, ok := delta["reasoning_content"]; ok {
		t.Fatalf("reasoning_content should be hidden from stream delta: %#v", delta)
	}
	usage := got["usage"].(map[string]any)
	if _, ok := usage["extra"]; ok {
		t.Fatalf("non-standard usage field was not stripped: %#v", usage)
	}
}
