package meter

import (
	"bytes"
	"encoding/json"
)

const (
	// maxJSONBody bounds how much of a non-streaming response we buffer for
	// usage parsing. A Messages API response is far smaller in practice.
	maxJSONBody = 10 << 20
	// maxSSELine bounds the partial-line buffer; data lines longer than this
	// (no legitimate usage event comes close) are discarded unparsed.
	maxSSELine = 1 << 20
)

type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u *usageJSON) mergeInto(dst *Usage) {
	if u.InputTokens > 0 {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = u.CacheReadInputTokens
	}
}

// jsonParser buffers a non-streaming response and reads model + usage from
// the complete JSON document.
type jsonParser struct {
	buf      bytes.Buffer
	overflow bool
}

func (p *jsonParser) Feed(b []byte) {
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

func (p *jsonParser) Finish() (Usage, bool) {
	if p.overflow {
		return Usage{}, false
	}
	var body struct {
		Model string     `json:"model"`
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(p.buf.Bytes(), &body); err != nil || body.Usage == nil {
		return Usage{}, false
	}
	u := Usage{Model: body.Model}
	body.Usage.mergeInto(&u)
	return u, true
}

// sseParser scans an Anthropic streaming response. For the Messages API,
// `message_start` carries the model and input (+ cache) tokens; the final
// `message_delta` carries the output tokens.
type sseParser struct {
	scanner sseScanner
	usage   Usage
	seen    bool
}

func newAnthropicSSE() *sseParser {
	p := &sseParser{}
	p.scanner.onData = p.handleData
	return p
}

func (p *sseParser) Feed(b []byte) { p.scanner.Feed(b) }

func (p *sseParser) handleData(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string     `json:"model"`
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message == nil {
			return
		}
		p.usage.Model = ev.Message.Model
		if ev.Message.Usage != nil {
			ev.Message.Usage.mergeInto(&p.usage)
		}
		p.seen = true
	case "message_delta":
		if ev.Usage == nil {
			return
		}
		ev.Usage.mergeInto(&p.usage)
		p.seen = true
	}
}

func (p *sseParser) Finish() (Usage, bool) {
	return p.usage, p.seen
}
