package meter

import (
	"os"
	"testing"
)

// wantOpenAIUsage: prompt_tokens 57 includes 32 cached — InputTokens must be
// the uncached remainder so cost math stays uniform across providers.
var wantOpenAIUsage = Usage{
	Model:                "gpt-5",
	InputTokens:          25,
	OutputTokens:         17,
	CacheReadInputTokens: 32,
}

func TestOpenAIJSONParser(t *testing.T) {
	data, err := os.ReadFile("testdata/openai_response.json")
	if err != nil {
		t.Fatal(err)
	}
	p := ForResponse("openai", "application/json")
	feedInChunks(p, data, 7)

	u, ok := p.Finish()
	if !ok {
		t.Fatal("Finish: no usage parsed")
	}
	if u != wantOpenAIUsage {
		t.Errorf("usage = %+v, want %+v", u, wantOpenAIUsage)
	}
}

func TestOpenAIJSONParserNoUsage(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"error response", `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`},
		{"garbage", "nope"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ForResponse("openai", "application/json")
			p.Feed([]byte(tt.body))
			if _, ok := p.Finish(); ok {
				t.Error("Finish ok = true, want false")
			}
		})
	}
}

func TestOpenAISSEParser(t *testing.T) {
	data, err := os.ReadFile("testdata/openai_stream.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunkSize := range []int{1, 7, len(data)} {
		p := ForResponse("openai", "text/event-stream")
		feedInChunks(p, data, chunkSize)

		u, ok := p.Finish()
		if !ok {
			t.Fatalf("chunkSize %d: no usage parsed", chunkSize)
		}
		if u != wantOpenAIUsage {
			t.Errorf("chunkSize %d: usage = %+v, want %+v", chunkSize, u, wantOpenAIUsage)
		}
	}
}

func TestOpenAISSEParserWithoutUsageChunk(t *testing.T) {
	// Client did not request stream_options.include_usage: chunks only,
	// then [DONE]. No usage must be reported.
	p := ForResponse("openai", "text/event-stream")
	p.Feed([]byte(`data: {"model":"gpt-5","choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n"))
	p.Feed([]byte("data: [DONE]\n\n"))
	if _, ok := p.Finish(); ok {
		t.Error("Finish ok = true, want false without a usage chunk")
	}
}

func TestOpenAISSEParserNoCachedDetails(t *testing.T) {
	// vLLM commonly omits prompt_tokens_details entirely.
	p := ForResponse("openai", "text/event-stream")
	p.Feed([]byte(`data: {"model":"qwen-2.5-7b","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n"))
	p.Feed([]byte("data: [DONE]\n\n"))
	u, ok := p.Finish()
	if !ok || u.Model != "qwen-2.5-7b" || u.InputTokens != 10 || u.OutputTokens != 5 || u.CacheReadInputTokens != 0 {
		t.Errorf("usage = %+v ok=%v", u, ok)
	}
}
