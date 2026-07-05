package meter

import (
	"bytes"
	"encoding/json"
)

// openaiUsage is the usage block of OpenAI-compatible responses (OpenAI,
// vLLM, and most self-hosted servers). prompt_tokens INCLUDES cached tokens,
// unlike Anthropic — toUsage subtracts so Usage semantics stay uniform.
type openaiUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *openaiUsage) toUsage(model string) Usage {
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return Usage{
		Model:                model,
		InputTokens:          u.PromptTokens - cached,
		OutputTokens:         u.CompletionTokens,
		CacheReadInputTokens: cached,
	}
}

// openaiJSONParser buffers a non-streaming chat completion and reads model +
// usage from the complete document.
type openaiJSONParser struct {
	buf      bytes.Buffer
	overflow bool
}

func (p *openaiJSONParser) Feed(b []byte) {
	if p.overflow {
		return
	}
	if p.buf.Len()+len(b) > maxJSONBody {
		p.overflow = true
		p.buf.Reset()
		return
	}
	p.buf.Write(b)
}

func (p *openaiJSONParser) Finish() (Usage, bool) {
	if p.overflow {
		return Usage{}, false
	}
	var body struct {
		Model string       `json:"model"`
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(p.buf.Bytes(), &body); err != nil || body.Usage == nil {
		return Usage{}, false
	}
	return body.Usage.toUsage(body.Model), true
}

// openaiSSEParser scans a streaming chat completion. Usage arrives in a
// late chunk with a non-null "usage" field — sent when the client requests
// stream_options.include_usage (vLLM emits it the same way) — followed by
// the [DONE] sentinel.
type openaiSSEParser struct {
	scanner sseScanner
	usage   Usage
	seen    bool
}

func newOpenAISSE() *openaiSSEParser {
	p := &openaiSSEParser{}
	p.scanner.onData = p.handleData
	return p
}

func (p *openaiSSEParser) Feed(b []byte) { p.scanner.Feed(b) }

func (p *openaiSSEParser) handleData(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var chunk struct {
		Model string       `json:"model"`
		Usage *openaiUsage `json:"usage"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	if p.usage.Model == "" && chunk.Model != "" {
		p.usage.Model = chunk.Model
	}
	if chunk.Usage != nil {
		model := p.usage.Model
		if chunk.Model != "" {
			model = chunk.Model
		}
		p.usage = chunk.Usage.toUsage(model)
		p.seen = true
	}
}

func (p *openaiSSEParser) Finish() (Usage, bool) {
	return p.usage, p.seen
}
