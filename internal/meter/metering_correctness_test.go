package meter_test

// Group C — metering accuracy, at the parser/pricing layer. The storage half of
// C3/C4 (what actually lands in SQLite) lives in cmd/tollgate.
//
// This is an external test package (meter_test) so it can import pricing, which
// itself imports meter — the parse-then-price path is only meaningful end to end.

import (
	"math"
	"testing"

	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/pricing"
)

// feedInChunks pushes the body through the parser in small pieces, the way the
// proxy's tee reader does — a parser that only works on whole bodies is a bug.
func feedInChunks(p meter.Parser, body string, chunk int) {
	for i := 0; i < len(body); i += chunk {
		end := i + chunk
		if end > len(body) {
			end = len(body)
		}
		p.Feed([]byte(body[i:end]))
	}
}

const usdEpsilon = 1e-9 // costs here are fractions of a cent; compare exactly

// TestCorrectness_CachedTokensPriceAtCacheRate covers C2.
//
// INVARIANT: cached input is priced at the cache rate, not the input rate.
//
// Two ways to get this wrong, both of which have shipped elsewhere: pricing
// cache reads as ordinary input (overcharges ~10x on Anthropic), and failing to
// subtract OpenAI's cached tokens from prompt_tokens, which double-counts them
// as both input and cache read.
func TestCorrectness_CachedTokensPriceAtCacheRate(t *testing.T) {
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Anthropic rates (pricing.yaml): in 1.00, out 5.00, write 1.25, read 0.10.
	// 1000 in + 500 out + 2000 write + 4000 read
	//   = 0.001 + 0.0025 + 0.0025 + 0.0004 = 0.0064
	// Pricing the 6000 cached/write tokens as plain input instead gives 0.0095.
	const (
		anthropicWant       = 0.0064
		anthropicIfNotCache = 0.0095
	)
	// OpenAI rates: in 1.25, out 10.00, read 0.125, no cache-write charge.
	// prompt_tokens 5000 INCLUDES 4000 cached, so uncached input is 1000:
	//   1000*1.25 + 500*10.00 + 4000*0.125 (per MTok) = 0.00125 + 0.005 + 0.0005
	//   = 0.00675
	// Forgetting to subtract bills the 4000 twice: 0.01175.
	const (
		openaiWant          = 0.00675
		openaiIfNotSubtract = 0.01175
	)

	tests := []struct {
		name         string
		providerType string
		contentType  string
		body         string
		wantUsage    meter.Usage
		wantUSD      float64
		// wantNotUSD is what the cost would be if cached tokens were mispriced.
		// Asserting we are not at that number is the real point of the test.
		wantNotUSD float64
	}{
		{
			name:         "anthropic json",
			providerType: "anthropic",
			contentType:  "application/json",
			body: `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation_input_tokens":2000,"cache_read_input_tokens":4000}}`,
			wantUsage: meter.Usage{
				Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 500,
				CacheCreationInputTokens: 2000, CacheReadInputTokens: 4000,
			},
			wantUSD:    anthropicWant,
			wantNotUSD: anthropicIfNotCache,
		},
		{
			name:         "anthropic sse",
			providerType: "anthropic",
			contentType:  "text/event-stream",
			body: "event: message_start\n" +
				`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":` +
				`{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":4000}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":500}}` + "\n\n",
			wantUsage: meter.Usage{
				Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 500,
				CacheCreationInputTokens: 2000, CacheReadInputTokens: 4000,
			},
			wantUSD:    anthropicWant,
			wantNotUSD: anthropicIfNotCache,
		},
		{
			name:         "anthropic dated snapshot id prices like its base model",
			providerType: "anthropic",
			contentType:  "application/json",
			body: `{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation_input_tokens":2000,"cache_read_input_tokens":4000}}`,
			wantUsage: meter.Usage{
				Model: "claude-haiku-4-5-20251001", InputTokens: 1000, OutputTokens: 500,
				CacheCreationInputTokens: 2000, CacheReadInputTokens: 4000,
			},
			wantUSD:    anthropicWant,
			wantNotUSD: anthropicIfNotCache,
		},
		{
			name:         "openai json subtracts cached from prompt_tokens",
			providerType: "openai",
			contentType:  "application/json",
			body: `{"model":"gpt-5","usage":{"prompt_tokens":5000,"completion_tokens":500,` +
				`"prompt_tokens_details":{"cached_tokens":4000}}}`,
			wantUsage: meter.Usage{
				Model: "gpt-5", InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 4000,
			},
			wantUSD:    openaiWant,
			wantNotUSD: openaiIfNotSubtract,
		},
		{
			name:         "openai sse subtracts cached from prompt_tokens",
			providerType: "openai",
			contentType:  "text/event-stream",
			body: `data: {"model":"gpt-5","choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n" +
				`data: {"model":"gpt-5","choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":500,` +
				`"prompt_tokens_details":{"cached_tokens":4000}}}` + "\n\n" +
				"data: [DONE]\n\n",
			wantUsage: meter.Usage{
				Model: "gpt-5", InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 4000,
			},
			wantUSD:    openaiWant,
			wantNotUSD: openaiIfNotSubtract,
		},
		{
			name:         "openai without cached tokens bills all prompt tokens as input",
			providerType: "openai",
			contentType:  "application/json",
			body:         `{"model":"gpt-5","usage":{"prompt_tokens":5000,"completion_tokens":500}}`,
			wantUsage: meter.Usage{
				Model: "gpt-5", InputTokens: 5000, OutputTokens: 500,
			},
			// 5000*1.25 + 500*10.00 per MTok = 0.00625 + 0.005
			wantUSD:    0.01125,
			wantNotUSD: openaiWant,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := meter.ForResponse(tc.providerType, tc.contentType)
			if p == nil {
				t.Fatalf("no parser for %s/%s", tc.providerType, tc.contentType)
			}
			feedInChunks(p, tc.body, 7)
			got, ok := p.Finish()
			if !ok {
				t.Fatalf("Finish() ok = false, want usage parsed")
			}
			if got != tc.wantUsage {
				t.Errorf("usage = %+v, want %+v", got, tc.wantUsage)
			}

			cost, priced := prices.Cost(got.Model, got)
			if !priced {
				t.Fatalf("model %q missing from the pricing table", got.Model)
			}
			if math.Abs(cost-tc.wantUSD) > usdEpsilon {
				t.Errorf("cost = %.8f, want %.8f", cost, tc.wantUSD)
			}
			if math.Abs(cost-tc.wantNotUSD) <= usdEpsilon {
				t.Errorf("cost = %.8f, which is the mispriced-cache figure", cost)
			}
		})
	}
}

// TestCorrectness_UnparseableUsageIsNotZero covers the parser half of C3.
//
// INVARIANT: a response whose usage cannot be read reports ok=false, so the
// caller can flag it — it never reports a clean, confident zero.
//
// Finish() returning (Usage{}, true) for any of these would be the dangerous
// outcome: it is indistinguishable from a genuinely free request.
func TestCorrectness_UnparseableUsageIsNotZero(t *testing.T) {
	tests := []struct {
		name         string
		providerType string
		contentType  string
		body         string
	}{
		{
			name:         "anthropic truncated json",
			providerType: "anthropic",
			contentType:  "application/json",
			body:         `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output`,
		},
		{
			name:         "anthropic 200 with no usage field",
			providerType: "anthropic",
			contentType:  "application/json",
			body:         `{"model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}]}`,
		},
		{
			name:         "anthropic stream ends before message_delta",
			providerType: "anthropic",
			contentType:  "text/event-stream",
			body: "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n",
		},
		{
			name:         "anthropic empty body",
			providerType: "anthropic",
			contentType:  "application/json",
			body:         "",
		},
		{
			name:         "openai truncated json",
			providerType: "openai",
			contentType:  "application/json",
			body:         `{"model":"gpt-5","usage":{"prompt_tokens":5000,"comple`,
		},
		{
			name:         "openai 200 with no usage field",
			providerType: "openai",
			contentType:  "application/json",
			body:         `{"model":"gpt-5","choices":[{"message":{"content":"hi"}}]}`,
		},
		{
			name:         "openai stream ends before the usage chunk",
			providerType: "openai",
			contentType:  "text/event-stream",
			body: `data: {"model":"gpt-5","choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := meter.ForResponse(tc.providerType, tc.contentType)
			if p == nil {
				t.Fatalf("no parser for %s/%s", tc.providerType, tc.contentType)
			}
			feedInChunks(p, tc.body, 7)
			got, ok := p.Finish()
			if ok {
				t.Errorf("Finish() ok = true with usage %+v; unparseable usage must "+
					"not be reported as a confident zero", got)
			}
		})
	}
}

// TestCorrectness_TruncatedStreamReportsTokensSeen covers the parser half of C1.
//
// INVARIANT: a stream cut short still reports exactly the usage the upstream
// had already declared — never a partial-but-wrong total.
//
// Anthropic sends input (and cache) tokens up front in message_start and output
// tokens at the end in message_delta. A stream cut in between therefore reports
// real input tokens and zero output tokens. That is the deliberate semantic:
// bill for what the upstream told us it consumed, not for a guess. The
// undercount it implies is documented in docs/correctness.md.
func TestCorrectness_TruncatedStreamReportsTokensSeen(t *testing.T) {
	const head = "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":` +
		`{"input_tokens":1000,"cache_read_input_tokens":4000}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"partial"}}` + "\n\n"

	p := meter.ForResponse("anthropic", "text/event-stream")
	feedInChunks(p, head, 13) // client disconnects here; no message_delta ever arrives
	got, ok := p.Finish()
	if !ok {
		t.Fatal("Finish() ok = false, want the input tokens already declared upstream")
	}
	want := meter.Usage{
		Model: "claude-haiku-4-5", InputTokens: 1000, CacheReadInputTokens: 4000,
	}
	if got != want {
		t.Errorf("usage after mid-stream disconnect = %+v, want %+v", got, want)
	}
	if got.OutputTokens != 0 {
		t.Errorf("output tokens = %d, want 0: none were ever declared upstream", got.OutputTokens)
	}
}
