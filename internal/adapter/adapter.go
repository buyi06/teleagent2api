package adapter

import (
	"encoding/json"

	"teleagent2api/internal/config"
)

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

	// Resolve model name and cap max_tokens
	if modelRaw, ok := cleaned["model"]; ok {
		var modelName string
		_ = json.Unmarshal(modelRaw, &modelName)
		if meta, ok := modelMeta[modelName]; ok {
			if maxTokensRaw, ok := cleaned["max_tokens"]; ok {
				var maxTokens int
				_ = json.Unmarshal(maxTokensRaw, &maxTokens)
				if maxTokens > meta.MaxOutput {
					capped, _ := json.Marshal(meta.MaxOutput)
					cleaned["max_tokens"] = capped
				}
			}
		}
	}

	out, err := json.Marshal(cleaned)
	if err != nil {
		return body
	}
	return out
}

// TransformNonStreamingResponse rewrites an upstream non-streaming response
// to be fully OpenAI-compatible. reasoning_content is preserved as it is
// supported by OpenAI o1/o3 models and clients like Claude Code.
func TransformNonStreamingResponse(body []byte) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}

	// Transform choices — keep reasoning_content, just ensure content isn't empty
	if choicesRaw, ok := resp["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err != nil {
			return body
		}
		for i, choice := range choices {
			choices[i] = transformChoice(choice)
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

// transformChoice rewrites a single choice object.
// Preserves reasoning_content but ensures content is not empty when
// reasoning_content exists.
func transformChoice(choice map[string]json.RawMessage) map[string]json.RawMessage {
	msgRaw, ok := choice["message"]
	if !ok {
		return choice
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return choice
	}

	// Keep reasoning_content — it's valid in OpenAI extended format
	// But if content is empty and reasoning_content exists, move it to content
	var content string
	if cRaw, ok := msg["content"]; ok {
		_ = json.Unmarshal(cRaw, &content)
	}
	if content == "" {
		if rcRaw, ok := msg["reasoning_content"]; ok {
			var rc string
			_ = json.Unmarshal(rcRaw, &rc)
			if rc != "" {
				contentOut, _ := json.Marshal(rc)
				msg["content"] = contentOut
			}
		}
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
		if content != "" || reasoning != "" {
			return false
		}
	}
	return true
}

// StreamChunkState tracks state across SSE chunks for transformation.
type StreamChunkState struct {
	roleSent       bool // whether we've emitted the role in a content delta
	hasContent     bool // whether any chunk had actual content or reasoning
}

// NewStreamChunkState creates a new state tracker for streaming transformations.
func NewStreamChunkState() *StreamChunkState {
	return &StreamChunkState{}
}

// TransformChunk rewrites a single SSE data payload to be OpenAI-compatible.
// Returns (transformedJSON, skip). If skip is true, this chunk should be
// dropped entirely. reasoning_content is preserved in streaming deltas.
func (s *StreamChunkState) TransformChunk(data []byte) ([]byte, bool) {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, false
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
			// Keep reasoning_content — clients like Claude Code support it
		}

		_, hasContent := delta["content"]

		// Check if this is a reasoning-only chunk with no content at all
		if hasReasoningContent && !hasContent {
			// Check for empty reasoning_content (chat-pro sends these during phases)
			var rcStr string
			_ = json.Unmarshal(delta["reasoning_content"], &rcStr)
			if rcStr == "" && len(delta) <= 2 { // only role + empty reasoning
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

			// Skip empty content chunks without reasoning (usage updates)
			if contentStr == "" && !hasReasoningContent {
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
