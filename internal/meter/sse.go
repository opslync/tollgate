package meter

import "bytes"

// sseScanner splits a streaming body into SSE data payloads without
// buffering the stream. Oversized lines (beyond maxSSELine) are discarded
// unparsed; no legitimate usage event comes close to the cap.
type sseScanner struct {
	line    []byte
	discard bool
	onData  func(data []byte)
}

func (s *sseScanner) Feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.append(b)
			return
		}
		s.append(b[:i])
		if !s.discard {
			s.handleLine(s.line)
		}
		s.line = s.line[:0]
		s.discard = false
		b = b[i+1:]
	}
}

func (s *sseScanner) append(b []byte) {
	if s.discard {
		return
	}
	if len(s.line)+len(b) > maxSSELine {
		s.discard = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, b...)
}

func (s *sseScanner) handleLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	s.onData(bytes.TrimSpace(data))
}
