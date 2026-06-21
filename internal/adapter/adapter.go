package adapter

import (
	"encoding/json"

	"teleagent2api/internal/config"
)

// TransformOptions controls compatibility fixes applied while translating
// TeleAgent responses to OpenAI-compatible responses.
type TransformOptions struct {
	// ExposeReasoning preserves provider-specific reasoning_content fields.
	// Keep this disabled for OpenAI-compatible coding clients such as Claude Code:
	// many clients either ignore this field or treat it like visible assistant
	// output, which makes streams noisy and wastes output tokens.
	ExposeReasoning bool

	// ReasoningContentFallback copies reasoning_content into content when the
	// final content is empty. This preserves the previous behavior for users who
	// explicitly opt in, but should stay disabled by default because it can turn
	// hidden chain-of-thought-style text into visible assistant output.
	ReasoningContentFallback bool

	// ModelAlias rewrites top-level response model IDs back to the model name the
	// client requested (for example chat-flash instead of the upstream GLM name).
	ModelAlias string
}

func legacyTransformOptions() TransformOptions {
	return TransformOptions{
		ExposeReasoning:          true,
		ReasoningContentFallback: true,
	}
}

// allowedRequestFields lists the fields we forward to the upstream API.
// Any field not in this list is stripped before forwarding.
var allowedRequestFields = map[string]bool{
	"model":       true,
	"messages":    true,
	"stream":      true,
	"temperature": true,
	"top_p":       true,
	"max_tokens":  true,
	"tools":       true,
	"tool_choice": true,
}

// SanitizeRequest strips fields that the upstream does not support,
// preventing "API 调用参数有误" errors from Claude Code requests.
// It also caps max_tokens to the model's maximum output limit.
func SanitizeRequest(body []byte, modelMeta map[string]config.ModelMeta) []byte {
	return SanitizeRequestWithOptions(body, modelMeta, 0)
}

// SanitizeRequestWithOptions strips unsupported request fields, caps
// max_tokens to the model limit, and optionally raises too-small max_tokens.
// TeleAgent models can spend a large part of the completion budget on
// reasoning_content before emitting final content. Claude Code often sends
// small max_tokens for probes; without a floor those requests can be truncated
// before any usable answer or tool call is produced.
func SanitizeRequestWithOptions(body []byte, modelMeta map[string]config.ModelMeta, minOutputTokens int) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body // not valid JSON, forward as-is
	}

	cleaned := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if allowedRequestFields[k] {
			cleaned[k] = v
		}
	}

	// Resolve model name, raise too-small max_tokens, and cap to model limit.
	maxOutput := 0
	if modelRaw, ok := cleaned["model"]; ok {
		var modelName string
		_ = json.Unmarshal(modelRaw, &modelName)
		if meta, ok := modelMeta[modelName]; ok {
			maxOutput = meta.MaxOutput
		}
	}
	if minOutputTokens > 0 || maxOutput > 0 {
		targetMin := minOutputTokens
		if maxOutput > 0 && targetMin > maxOutput {
			targetMin = maxOutput
		}

		maxTokens := 0
		hasMaxTokens := false
		if maxTokensRaw, ok := cleaned["max_tokens"]; ok {
			if err := json.Unmarshal(maxTokensRaw, &maxTokens); err == nil {
				hasMaxTokens = true
			}
		}

		switch {
		case hasMaxTokens && maxTokens > 0:
			if targetMin > 0 && maxTokens < targetMin {
				maxTokens = targetMin
			}
			if maxOutput > 0 && maxTokens > maxOutput {
				maxTokens = maxOutput
			}
			out, _ := json.Marshal(maxTokens)
			cleaned["max_tokens"] = out
		case !hasMaxTokens && targetMin > 0:
			out, _ := json.Marshal(targetMin)
			cleaned["max_tokens"] = out
		}
	}

	out, err := json.Marshal(cleaned)
	if err != nil {
		return body
	}
	return out
}

// TransformNonStreamingResponse rewrites an upstream non-streaming response
// using the legacy behavior kept for direct package callers.
func TransformNonStreamingResponse(body []byte) []byte {
	return TransformNonStreamingResponseWithOptions(body, legacyTransformOptions())
}

// TransformNonStreamingResponseWithOptions rewrites an upstream non-streaming
// response according to the provided compatibility options.
func TransformNonStreamingResponseWithOptions(body []byte, opts TransformOptions) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}

	if opts.ModelAlias != "" {
		modelOut, _ := json.Marshal(opts.ModelAlias)
		resp["model"] = modelOut
	}

	// Transform choices — keep reasoning_content, just ensure content isn't empty
	if choicesRaw, ok := resp["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err != nil {
			return body
		}
		for i, choice := range choices {
			choices[i] = transformChoice(choice, opts)
		}
		choicesOut, _ := json.Marshal(choices)
		resp["choices"] = choicesOut
	}

	// Clean usage: only keep standard OpenAI fields
	if usageRaw, ok := resp["usage"]; ok {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(usageRaw, &usage); err == nil {
			keepUsage := make(map[string]json.RawMessage)
			for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
				if v, ok := usage[k]; ok {
					keepUsage[k] = v
				}
			}
			usageOut, _ := json.Marshal(keepUsage)
			resp["usage"] = usageOut
		}
	}

	// Remove non-standard top-level fields
	delete(resp, "request_id")
	delete(resp, "system_fingerprint")

	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}

// TransformNonStreamingResponseToSSE converts a valid non-streaming chat
// completion into a minimal OpenAI-compatible SSE stream. This is a fallback
// for upstream responses that ignore stream=true but still return HTTP 200
// JSON. OpenAI clients that asked for streaming expect text/event-stream, not
// a raw JSON object.
func TransformNonStreamingResponseToSSE(body []byte) []byte {
	return TransformNonStreamingResponseToSSEWithOptions(body, legacyTransformOptions())
}

// TransformNonStreamingResponseToSSEWithOptions converts a valid non-streaming
// response into a minimal SSE stream using the same response-transform options.
func TransformNonStreamingResponseToSSEWithOptions(body []byte, opts TransformOptions) []byte {
	transformed := TransformNonStreamingResponseWithOptions(body, opts)

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(transformed, &resp); err != nil {
		return transformed
	}

	if objectRaw, ok := resp["object"]; ok {
		var object string
		if json.Unmarshal(objectRaw, &object) == nil && object == "chat.completion" {
			resp["object"] = json.RawMessage(`"chat.completion.chunk"`)
		}
	}

	if choicesRaw, ok := resp["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err == nil {
			for i, choice := range choices {
				if msgRaw, ok := choice["message"]; ok {
					var msg map[string]json.RawMessage
					if json.Unmarshal(msgRaw, &msg) == nil {
						delta := make(map[string]json.RawMessage)
						for _, key := range []string{"role", "content", "reasoning_content", "tool_calls"} {
							if v, ok := msg[key]; ok {
								delta[key] = v
							}
						}
						if _, ok := delta["role"]; !ok {
							delta["role"] = json.RawMessage(`"assistant"`)
						}
						deltaOut, _ := json.Marshal(delta)
						choice["delta"] = deltaOut
						delete(choice, "message")
						choices[i] = choice
					}
				}
			}
			choicesOut, _ := json.Marshal(choices)
			resp["choices"] = choicesOut
		}
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		payload = transformed
	}

	out := make([]byte, 0, len(payload)+32)
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, "\n\ndata: [DONE]\n\n"...)
	return out
}

// transformChoice rewrites a single choice object.
// Preserves reasoning_content but ensures content is not empty when
// reasoning_content exists.
func transformChoice(choice map[string]json.RawMessage, opts TransformOptions) map[string]json.RawMessage {
	msgRaw, ok := choice["message"]
	if !ok {
		return choice
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return choice
	}

	var content string
	if cRaw, ok := msg["content"]; ok {
		_ = json.Unmarshal(cRaw, &content)
	}
	if content == "" && opts.ReasoningContentFallback {
		if rcRaw, ok := msg["reasoning_content"]; ok {
			var rc string
			_ = json.Unmarshal(rcRaw, &rc)
			if rc != "" {
				contentOut, _ := json.Marshal(rc)
				msg["content"] = contentOut
			}
		}
	}
	if !opts.ExposeReasoning {
		delete(msg, "reasoning_content")
	}

	msgOut, _ := json.Marshal(msg)
	choice["message"] = msgOut
	return choice
}

// IsEmptyResponse returns true if a non-streaming response has no usable
// content — both content and reasoning_content are empty or missing.
func IsEmptyResponse(body []byte) bool {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}

	choicesRaw, ok := resp["choices"]
	if !ok {
		return true
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil {
		return false
	}
	if len(choices) == 0 {
		return true
	}

	for _, choice := range choices {
		msgRaw, ok := choice["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}
		var content, reasoning string
		if c, ok := msg["content"]; ok {
			_ = json.Unmarshal(c, &content)
		}
		if rc, ok := msg["reasoning_content"]; ok {
			_ = json.Unmarshal(rc, &reasoning)
		}
		if toolCalls, ok := msg["tool_calls"]; ok && len(toolCalls) > 0 && string(toolCalls) != "null" && string(toolCalls) != "[]" {
			return false
		}
		if content != "" || reasoning != "" {
			return false
		}
	}
	return true
}

// StreamChunkState tracks state across SSE chunks for transformation.
type StreamChunkState struct {
	roleSent   bool // whether we've emitted the role in a content delta
	hasContent bool // whether any chunk had actual content or reasoning
	opts       TransformOptions
}

// NewStreamChunkState creates a new state tracker for streaming transformations.
func NewStreamChunkState() *StreamChunkState {
	return &StreamChunkState{opts: legacyTransformOptions()}
}

// NewStreamChunkStateWithOptions creates a stream transformer with explicit
// compatibility options.
func NewStreamChunkStateWithOptions(opts TransformOptions) *StreamChunkState {
	return &StreamChunkState{opts: opts}
}

// TransformChunk rewrites a single SSE data payload to be OpenAI-compatible.
// Returns (transformedJSON, skip). If skip is true, this chunk should be
// dropped entirely. reasoning_content is preserved in streaming deltas.
func (s *StreamChunkState) TransformChunk(data []byte) ([]byte, bool) {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, false
	}

	if s.opts.ModelAlias != "" {
		modelOut, _ := json.Marshal(s.opts.ModelAlias)
		chunk["model"] = modelOut
	}

	choicesRaw, ok := chunk["choices"]
	if !ok {
		return data, false
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil {
		return data, false
	}

	skipChunk := true // assume skip unless we find useful content

	for i, choice := range choices {
		deltaRaw, ok := choice["delta"]
		if !ok {
			skipChunk = false
			continue
		}
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &delta); err != nil {
			skipChunk = false
			continue
		}

		hasReasoningContent := false
		if _, ok := delta["reasoning_content"]; ok {
			hasReasoningContent = true
			// Keep reasoning_content only when explicitly requested.
		}
		if hasReasoningContent && !s.opts.ExposeReasoning {
			delete(delta, "reasoning_content")
		}

		_, hasContent := delta["content"]
		_, hasToolCalls := delta["tool_calls"]
		if hasToolCalls {
			s.hasContent = true
		}
		hasUsage := false
		if usageRaw, ok := chunk["usage"]; ok && len(usageRaw) > 0 && string(usageRaw) != "null" && string(usageRaw) != "{}" {
			hasUsage = true
		}
		hasFinishReason := false
		if finishRaw, ok := choice["finish_reason"]; ok && len(finishRaw) > 0 && string(finishRaw) != "null" && string(finishRaw) != `""` {
			hasFinishReason = true
		}

		// Check if this is a reasoning-only chunk with no content at all.
		// When reasoning is hidden, drop these prelude chunks entirely so
		// OpenAI-compatible coding clients only see final content/tool/finish
		// chunks.
		if hasReasoningContent && !hasContent {
			if !s.opts.ExposeReasoning && !hasToolCalls && !hasFinishReason && !hasUsage {
				continue
			}
			// Check for empty reasoning_content (chat-pro sends these during phases)
			var rcStr string
			if raw, ok := delta["reasoning_content"]; ok {
				_ = json.Unmarshal(raw, &rcStr)
			}
			if rcStr == "" && len(delta) <= 2 && !hasToolCalls && !hasFinishReason && !hasUsage { // only role + empty reasoning
				// Skip pure empty reasoning chunks
				continue
			}
			// Has actual reasoning content — keep it but add role if needed
			if !s.roleSent {
				delta["role"] = json.RawMessage(`"assistant"`)
				s.roleSent = true
			}
			s.hasContent = true
			skipChunk = false
			deltaOut, _ := json.Marshal(delta)
			choice["delta"] = deltaOut
			choices[i] = choice
			continue
		}

		// Has content
		if hasContent {
			var contentStr string
			_ = json.Unmarshal(delta["content"], &contentStr)

			// Skip empty content chunks only when they carry no other useful
			// stream information. Some providers send tool_calls or
			// finish_reason together with content:""; dropping those makes
			// clients hang or lose function calls.
			if contentStr == "" && !hasReasoningContent && !hasToolCalls && !hasFinishReason && !hasUsage {
				continue
			}
			if contentStr != "" {
				s.hasContent = true
			}
		}

		skipChunk = false

		// Role handling: only emit role on first real delta
		if hasContent || hasReasoningContent {
			if s.roleSent {
				delete(delta, "role")
			} else {
				s.roleSent = true
			}
		}

		deltaOut, _ := json.Marshal(delta)
		choice["delta"] = deltaOut
		choices[i] = choice
	}

	if skipChunk {
		return nil, true
	}

	choicesOut, _ := json.Marshal(choices)
	chunk["choices"] = choicesOut

	// Remove non-standard top-level fields from streaming chunks
	delete(chunk, "system_fingerprint")

	if usageRaw, ok := chunk["usage"]; ok {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(usageRaw, &usage); err == nil {
			keepUsage := make(map[string]json.RawMessage)
			for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
				if v, ok := usage[k]; ok {
					keepUsage[k] = v
				}
			}
			usageOut, _ := json.Marshal(keepUsage)
			chunk["usage"] = usageOut
		}
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return data, false
	}
	return out, false
}

// HasContent returns true if at least one chunk contained actual content or reasoning.
func (s *StreamChunkState) HasContent() bool {
	return s.hasContent
}
