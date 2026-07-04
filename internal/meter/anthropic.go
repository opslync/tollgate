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

// sseParser scans a streaming response line by line without buffering the
// stream. For the Messages API, `message_start` carries the model and input
// (+ cache) tokens; the final `message_delta` carries the output tokens.
type sseParser struct {
	line    []byte
	discard bool
	usage   Usage
	seen    bool
}

func (p *sseParser) Feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.append(b)
			return
		}
		p.append(b[:i])
		if !p.discard {
			p.handleLine(p.line)
		}
		p.line = p.line[:0]
		p.discard = false
		b = b[i+1:]
	}
}

func (p *sseParser) append(b []byte) {
	if p.discard {
		return
	}
	if len(p.line)+len(b) > maxSSELine {
		p.discard = true
		p.line = p.line[:0]
		return
	}
	p.line = append(p.line, b...)
}

func (p *sseParser) handleLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string     `json:"model"`
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
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
