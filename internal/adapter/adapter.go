package adapter

import (
	"encoding/json"
	"strings"

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
// to be fully OpenAI-compatible. The reasoning embedded as <think>...</think>
// in the message content is handled according to the configured mode.
func TransformNonStreamingResponse(body []byte, mode string) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}

	if choicesRaw, ok := resp["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err != nil {
			return body
		}
		for i, choice := range choices {
			choices[i] = transformChoice(choice, mode)
		}
		choicesOut, _ := json.Marshal(choices)
		resp["choices"] = choicesOut
	}

	// Clean usage: only keep standard OpenAI fields
	if usageRaw, ok := resp["usage"]; ok {
		if cleaned := cleanUsage(usageRaw); cleaned != nil {
			resp["usage"] = cleaned
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

// transformChoice rewrites a single non-streaming choice object, splitting the
// <think>...</think> reasoning out of the content per the configured mode.
func transformChoice(choice map[string]json.RawMessage, mode string) map[string]json.RawMessage {
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

	switch mode {
	case "reasoning_content", "visible", "strip":
		reasoning, answer := splitThinkFull(content)
		switch mode {
		case "reasoning_content":
			if answer != "" {
				cv, _ := json.Marshal(answer)
				msg["content"] = cv
			}
			if reasoning != "" {
				rv, _ := json.Marshal(reasoning)
				msg["reasoning_content"] = rv
			}
		case "visible":
			cv, _ := json.Marshal(reasoning + answer)
			msg["content"] = cv
			delete(msg, "reasoning_content")
		case "strip":
			cv, _ := json.Marshal(answer)
			msg["content"] = cv
			delete(msg, "reasoning_content")
		}
	default:
		// content mode: keep as-is, but if content is empty and the upstream
		// supplied reasoning_content, surface it so the message is not empty.
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

// --- Streaming transform ---------------------------------------------------

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

type segKind int

const (
	segAnswer segKind = iota
	segReasoning
)

type segment struct {
	kind segKind
	text string
}

// StreamProcessor transforms upstream SSE chunks into OpenAI-compatible chunks,
// rerouting the <think>...</think> reasoning according to the configured mode.
// It is stateful across the chunks of a single response and is NOT safe for
// concurrent use.
type StreamProcessor struct {
	mode       string
	roleSent   bool
	inThink    bool
	pending    string
	hasContent bool
}

// NewStreamProcessor creates a processor for the given reasoning mode.
func NewStreamProcessor(mode string) *StreamProcessor {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "reasoning_content", "visible", "strip", "content":
	default:
		m = "content"
	}
	return &StreamProcessor{mode: m}
}

// HasContent reports whether any visible content or reasoning was emitted.
func (p *StreamProcessor) HasContent() bool { return p.hasContent }

type chunkMeta struct {
	id      json.RawMessage
	object  json.RawMessage
	created json.RawMessage
	model   json.RawMessage
}

// ProcessChunk transforms one upstream SSE data payload (the JSON object after
// "data: ") into zero or more OpenAI-compatible JSON payloads.
func (p *StreamProcessor) ProcessChunk(data []byte) [][]byte {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal(data, &chunk); err != nil {
		return [][]byte{data} // unknown payload, pass through
	}

	meta := chunkMeta{
		id:      chunk["id"],
		object:  chunk["object"],
		created: chunk["created"],
		model:   chunk["model"],
	}

	var contentStr, upstreamReasoning, finish string
	haveFinish := false

	if cr, ok := chunk["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(cr, &choices); err == nil && len(choices) > 0 {
			ch := choices[0]
			if dr, ok := ch["delta"]; ok {
				var delta map[string]json.RawMessage
				if json.Unmarshal(dr, &delta) == nil {
					if cv, ok := delta["content"]; ok {
						_ = json.Unmarshal(cv, &contentStr)
					}
					if rv, ok := delta["reasoning_content"]; ok {
						_ = json.Unmarshal(rv, &upstreamReasoning)
					}
				}
			}
			if fr, ok := ch["finish_reason"]; ok {
				var f interface{}
				if json.Unmarshal(fr, &f) == nil && f != nil {
					haveFinish = true
					if s, ok := f.(string); ok {
						finish = s
					}
				}
			}
		}
	}

	var out [][]byte

	// Upstream-provided reasoning_content (rare for this upstream) is always
	// treated as reasoning text.
	if upstreamReasoning != "" {
		if b := p.emitSegment(meta, segment{segReasoning, upstreamReasoning}); b != nil {
			out = append(out, b)
		}
	}

	// Split the content into reasoning/answer segments.
	if contentStr != "" {
		var segs []segment
		if p.mode == "content" {
			segs = []segment{{segAnswer, contentStr}}
		} else {
			segs = p.splitThink(contentStr)
		}
		for _, seg := range segs {
			if b := p.emitSegment(meta, seg); b != nil {
				out = append(out, b)
			}
		}
	}

	// Carry finish_reason and/or usage on a trailing chunk.
	var usageOut json.RawMessage
	if ur, ok := chunk["usage"]; ok {
		usageOut = cleanUsage(ur)
	}
	if haveFinish || usageOut != nil {
		out = append(out, p.buildTail(meta, finish, haveFinish, usageOut))
	}

	return out
}

// emitSegment renders one segment to a chunk payload, or returns nil if the
// segment is dropped (empty, or reasoning under "strip" mode).
func (p *StreamProcessor) emitSegment(meta chunkMeta, seg segment) []byte {
	if seg.text == "" {
		return nil
	}
	field := "content"
	if seg.kind == segReasoning {
		switch p.mode {
		case "reasoning_content":
			field = "reasoning_content"
		case "visible":
			field = "content"
		case "strip":
			return nil
		default:
			field = "content"
		}
	}
	p.hasContent = true
	return p.buildDelta(meta, field, seg.text)
}

// splitThink advances the <think> state machine over s, returning the
// reasoning/answer segments and buffering any trailing partial tag.
func (p *StreamProcessor) splitThink(s string) []segment {
	buf := p.pending + s
	p.pending = ""
	var segs []segment

	for len(buf) > 0 {
		if p.inThink {
			idx := strings.Index(buf, thinkClose)
			if idx >= 0 {
				if idx > 0 {
					segs = append(segs, segment{segReasoning, buf[:idx]})
				}
				buf = buf[idx+len(thinkClose):]
				p.inThink = false
				continue
			}
			keep := partialSuffix(buf, thinkClose)
			if emit := buf[:len(buf)-keep]; emit != "" {
				segs = append(segs, segment{segReasoning, emit})
			}
			p.pending = buf[len(buf)-keep:]
			return segs
		}

		idx := strings.Index(buf, thinkOpen)
		if idx >= 0 {
			if idx > 0 {
				segs = append(segs, segment{segAnswer, buf[:idx]})
			}
			buf = buf[idx+len(thinkOpen):]
			p.inThink = true
			continue
		}
		keep := partialSuffix(buf, thinkOpen)
		if emit := buf[:len(buf)-keep]; emit != "" {
			segs = append(segs, segment{segAnswer, emit})
		}
		p.pending = buf[len(buf)-keep:]
		return segs
	}
	return segs
}

// Flush emits any buffered partial text at the end of the stream.
func (p *StreamProcessor) Flush() [][]byte {
	if p.pending == "" {
		return nil
	}
	text := p.pending
	p.pending = ""
	kind := segAnswer
	if p.inThink {
		kind = segReasoning
	}
	meta := chunkMeta{object: json.RawMessage(`"chat.completion.chunk"`)}
	if b := p.emitSegment(meta, segment{kind, text}); b != nil {
		return [][]byte{b}
	}
	return nil
}

// partialSuffix returns the length of the longest suffix of buf that is a
// proper prefix of tag, so a tag split across chunk boundaries is not lost.
func partialSuffix(buf, tag string) int {
	n := len(tag) - 1
	if n > len(buf) {
		n = len(buf)
	}
	for k := n; k >= 1; k-- {
		if buf[len(buf)-k:] == tag[:k] {
			return k
		}
	}
	return 0
}

func (p *StreamProcessor) buildDelta(meta chunkMeta, field, text string) []byte {
	delta := map[string]json.RawMessage{}
	if !p.roleSent {
		delta["role"] = json.RawMessage(`"assistant"`)
		p.roleSent = true
	}
	tv, _ := json.Marshal(text)
	delta[field] = tv
	return assemble(meta, delta, "null", nil)
}

func (p *StreamProcessor) buildTail(meta chunkMeta, finish string, haveFinish bool, usage json.RawMessage) []byte {
	finishRaw := "null"
	if haveFinish {
		fv, _ := json.Marshal(finish)
		finishRaw = string(fv)
	}
	return assemble(meta, map[string]json.RawMessage{}, finishRaw, usage)
}

func assemble(meta chunkMeta, delta map[string]json.RawMessage, finishRaw string, usage json.RawMessage) []byte {
	deltaRaw, _ := json.Marshal(delta)
	choice := map[string]json.RawMessage{
		"index":         json.RawMessage(`0`),
		"delta":         deltaRaw,
		"finish_reason": json.RawMessage(finishRaw),
	}
	choiceRaw, _ := json.Marshal(choice)

	obj := map[string]json.RawMessage{
		"choices": json.RawMessage("[" + string(choiceRaw) + "]"),
	}
	if meta.id != nil {
		obj["id"] = meta.id
	}
	if meta.object != nil {
		obj["object"] = meta.object
	} else {
		obj["object"] = json.RawMessage(`"chat.completion.chunk"`)
	}
	if meta.created != nil {
		obj["created"] = meta.created
	}
	if meta.model != nil {
		obj["model"] = meta.model
	}
	if usage != nil {
		obj["usage"] = usage
	}
	raw, _ := json.Marshal(obj)
	return raw
}

// cleanUsage keeps only the standard OpenAI usage fields.
func cleanUsage(ur json.RawMessage) json.RawMessage {
	var usage map[string]json.RawMessage
	if json.Unmarshal(ur, &usage) != nil {
		return nil
	}
	keep := make(map[string]json.RawMessage)
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if v, ok := usage[k]; ok {
			keep[k] = v
		}
	}
	if len(keep) == 0 {
		return nil
	}
	raw, _ := json.Marshal(keep)
	return raw
}

// splitThinkFull splits a complete string into (reasoning, answer) by extracting
// a single <think>...</think> block. Used for non-streaming responses.
func splitThinkFull(s string) (reasoning, answer string) {
	open := strings.Index(s, thinkOpen)
	if open < 0 {
		return "", s
	}
	rest := s[open+len(thinkOpen):]
	close := strings.Index(rest, thinkClose)
	if close < 0 {
		// Unterminated think block: everything after the tag is reasoning.
		return rest, s[:open]
	}
	reasoning = rest[:close]
	answer = s[:open] + rest[close+len(thinkClose):]
	return reasoning, answer
}
