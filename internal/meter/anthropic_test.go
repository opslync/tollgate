package meter

import (
	"os"
	"strings"
	"testing"
)

func feedInChunks(p Parser, data []byte, chunkSize int) {
	for len(data) > 0 {
		n := min(chunkSize, len(data))
		p.Feed(data[:n])
		data = data[n:]
	}
}

func TestForResponse(t *testing.T) {
	tests := []struct {
		contentType string
		wantParser  bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"text/html", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ForResponse(tt.contentType) != nil; got != tt.wantParser {
			t.Errorf("ForResponse(%q) parser = %v, want %v", tt.contentType, got, tt.wantParser)
		}
	}
}

func TestJSONParser(t *testing.T) {
	data, err := os.ReadFile("testdata/response.json")
	if err != nil {
		t.Fatal(err)
	}
	p := ForResponse("application/json")
	feedInChunks(p, data, 7)

	u, ok := p.Finish()
	if !ok {
		t.Fatal("Finish: no usage parsed")
	}
	want := Usage{
		Model:                    "claude-sonnet-5",
		InputTokens:              25,
		OutputTokens:             50,
		CacheCreationInputTokens: 100,
		CacheReadInputTokens:     200,
	}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestJSONParserNoUsage(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"error response", `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`},
		{"garbage", "not json at all"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ForResponse("application/json")
			p.Feed([]byte(tt.body))
			if _, ok := p.Finish(); ok {
				t.Error("Finish ok = true, want false")
			}
		})
	}
}

func TestJSONParserOverflow(t *testing.T) {
	p := &jsonParser{}
	chunk := make([]byte, 1<<20)
	for i := 0; i < 11; i++ {
		p.Feed(chunk)
	}
	if _, ok := p.Finish(); ok {
		t.Error("Finish ok = true after overflow, want false")
	}
	if p.buf.Len() != 0 {
		t.Errorf("buffer not released on overflow, holds %d bytes", p.buf.Len())
	}
}

func TestSSEParser(t *testing.T) {
	data, err := os.ReadFile("testdata/stream.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Small chunks exercise partial-line reassembly across Feed calls.
	for _, chunkSize := range []int{1, 7, len(data)} {
		p := ForResponse("text/event-stream")
		feedInChunks(p, data, chunkSize)

		u, ok := p.Finish()
		if !ok {
			t.Fatalf("chunkSize %d: no usage parsed", chunkSize)
		}
		want := Usage{
			Model:                    "claude-sonnet-5",
			InputTokens:              472,
			OutputTokens:             91, // final message_delta wins over message_start's 2
			CacheCreationInputTokens: 100,
			CacheReadInputTokens:     200,
		}
		if u != want {
			t.Errorf("chunkSize %d: usage = %+v, want %+v", chunkSize, u, want)
		}
	}
}

func TestSSEParserCRLF(t *testing.T) {
	stream := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":10}}}\r\n\r\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\r\n\r\n"
	p := ForResponse("text/event-stream")
	p.Feed([]byte(stream))
	u, ok := p.Finish()
	if !ok || u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("usage = %+v ok=%v, want in=10 out=5", u, ok)
	}
}

func TestSSEParserNoUsage(t *testing.T) {
	p := ForResponse("text/event-stream")
	p.Feed([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\ndata: not json\n\n"))
	if _, ok := p.Finish(); ok {
		t.Error("Finish ok = true, want false")
	}
}

func TestSSEParserLongLineDiscarded(t *testing.T) {
	p := ForResponse("text/event-stream")
	// An oversized data line is dropped without corrupting later parsing.
	p.Feed([]byte("data: " + strings.Repeat("x", maxSSELine+1) + "\n"))
	p.Feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3}}}\n"))
	u, ok := p.Finish()
	if !ok || u.InputTokens != 3 {
		t.Errorf("usage = %+v ok=%v, want in=3", u, ok)
	}
}
