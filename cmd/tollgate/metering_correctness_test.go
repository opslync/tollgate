package main

// Group C — metering accuracy, storage side: what actually lands in SQLite when
// metering goes wrong. The parser-side cases live in internal/meter and the
// model-resolution cases in pricing.

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

var testAgents = []config.Agent{
	agentKey("support-bot", "k-support-bot-000001", "support", "prod"),
}

const testKey = "k-support-bot-000001"

// TestCorrectness_CancelledStreamAttributesTokensSeen covers C1.
//
// INVARIANT: a client disconnect mid-stream records exactly the usage the
// upstream had already declared — never a partial-but-wrong total, and never
// nothing at all.
//
// The intended semantic, decided up front: Tollgate bills what it saw. An
// Anthropic stream declares input and cache tokens in message_start and output
// tokens in message_delta, so a stream cut in between yields a row with real
// input tokens and zero output tokens, flagged "ok" because the usage we do
// have was genuinely parsed.
//
// The honest consequence, documented in docs/correctness.md: if the upstream
// kept generating after the client hung up, those output tokens are billed by
// the provider and not by Tollgate. This is a bounded undercount on cancelled
// streams only. The alternative — discarding the request entirely — would
// undercount strictly more, and guessing at the output tokens would invent
// money that no one can reconcile against a provider invoice.
func TestCorrectness_CancelledStreamAttributesTokensSeen(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":` +
			`{"input_tokens":1000,"cache_read_input_tokens":4000}}}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\n" +
			`data: {"type":"content_block_delta","delta":{"text":"partial"}}` + "\n\n"))
		flusher.Flush()
		// The client cancels; the proxy propagates that to this request's
		// context. message_delta — and its output_tokens — never gets written.
		<-r.Context().Done()
	})

	h := newHarness(t, harnessOptions{agents: testAgents, upstream: upstream})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.server.URL+"/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read until message_start has definitely reached us — which means it has
	// definitely passed through the metering tee.
	br := bufio.NewReader(resp.Body)
	var seen strings.Builder
	for !strings.Contains(seen.String(), "message_start") {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		seen.WriteString(line)
	}
	// Hang up mid-stream.
	cancel()
	resp.Body.Close()

	h.waitForRecords(1)

	rows := h.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1 (a cancelled stream must be recorded once)", len(rows))
	}
	got := rows[0]
	if got.InputTokens != 1000 || got.CacheRead != 4000 {
		t.Errorf("input = %d, cache read = %d; want 1000 / 4000 (declared before the disconnect)",
			got.InputTokens, got.CacheRead)
	}
	if got.OutputTokens != 0 {
		t.Errorf("output tokens = %d, want 0: the upstream never declared any", got.OutputTokens)
	}
	if got.UsageStatus != store.UsageOK {
		t.Errorf("usage_status = %q, want %q: the usage we have was parsed fine",
			got.UsageStatus, store.UsageOK)
	}
	// 1000 input @ $1/MTok + 4000 cache read @ $0.10/MTok = $0.0014.
	if cents6(got.CostUSD) != cents6(0.0014) {
		t.Errorf("cost = $%.6f, want $0.001400", got.CostUSD)
	}
	if !got.Stream {
		t.Error("stream = false, want true")
	}
}

// cents6 rounds to millionths of a dollar for comparing sub-cent costs.
func cents6(usd float64) int64 { return int64(usd*1e6 + 0.5) }

// TestCorrectness_UnpricedRequestIsFlaggedNotZeroed covers the storage half of
// C3 and C4.
//
// INVARIANT: a request whose cost could not be computed is stored with a flag
// saying so — never as an indistinguishable $0 row.
//
// This is the gap the suite was written to find, and it was real: before this
// change the recorder left CostUSD at zero whenever UsageOK was false and
// inserted the row with no marker at all, so an unreadable usage block, an
// unpriced model, and a genuinely free request were byte-identical on disk. The
// fix is the usage_status column (internal/store) plus the branch in
// newRecorder that sets it.
func TestCorrectness_UnpricedRequestIsFlaggedNotZeroed(t *testing.T) {
	tests := []struct {
		name            string
		upstreamStatus  int
		upstreamType    string
		upstreamBody    string
		wantUsageStatus string
		wantCostUSD     float64
		wantInputTokens int64
	}{
		{
			name:            "truncated json",
			upstreamStatus:  200,
			upstreamType:    "application/json",
			upstreamBody:    `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output`,
			wantUsageStatus: store.UsageUnparsed,
		},
		{
			name:            "200 with no usage field",
			upstreamStatus:  200,
			upstreamType:    "application/json",
			upstreamBody:    `{"model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}]}`,
			wantUsageStatus: store.UsageUnparsed,
		},
		{
			name:           "stream ends before the usage event",
			upstreamStatus: 200,
			upstreamType:   "text/event-stream",
			upstreamBody: "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n",
			wantUsageStatus: store.UsageUnparsed,
		},
		{
			name:            "model missing from the pricing table",
			upstreamStatus:  200,
			upstreamType:    "application/json",
			upstreamBody:    `{"model":"llama-3-70b-instruct","usage":{"input_tokens":1000,"output_tokens":500}}`,
			wantUsageStatus: store.UsageModelUnpriced,
			wantInputTokens: 1000, // tokens are trustworthy; only the price is not
		},
		{
			name:            "upstream error carries no usage and is not flagged",
			upstreamStatus:  500,
			upstreamType:    "application/json",
			upstreamBody:    `{"type":"error","error":{"type":"api_error","message":"boom"}}`,
			wantUsageStatus: store.UsageNotMetered,
		},
		{
			name:            "control: a fully priced request",
			upstreamStatus:  200,
			upstreamType:    "application/json",
			upstreamBody:    `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output_tokens":500}}`,
			wantUsageStatus: store.UsageOK,
			wantCostUSD:     0.0035, // 1000 @ $1/MTok + 500 @ $5/MTok
			wantInputTokens: 1000,
		},
		{
			name:            "control: a genuinely free priced request",
			upstreamStatus:  200,
			upstreamType:    "application/json",
			upstreamBody:    `{"model":"claude-haiku-4-5","usage":{"input_tokens":0,"output_tokens":0}}`,
			wantUsageStatus: store.UsageOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.upstreamType)
				w.WriteHeader(tc.upstreamStatus)
				_, _ = w.Write([]byte(tc.upstreamBody))
			})
			h := newHarness(t, harnessOptions{agents: testAgents, upstream: upstream})

			h.do(t, testKey, "/v1/messages", `{"model":"claude-haiku-4-5"}`, nil)
			h.waitForRecords(1)

			rows := h.rows(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			got := rows[0]
			if got.UsageStatus != tc.wantUsageStatus {
				t.Errorf("usage_status = %q, want %q", got.UsageStatus, tc.wantUsageStatus)
			}
			if cents6(got.CostUSD) != cents6(tc.wantCostUSD) {
				t.Errorf("cost = $%.6f, want $%.6f", got.CostUSD, tc.wantCostUSD)
			}
			if got.InputTokens != tc.wantInputTokens {
				t.Errorf("input tokens = %d, want %d", got.InputTokens, tc.wantInputTokens)
			}

			// The flag has to be legible to the aggregation that answers
			// GET /usage, otherwise storing it changes nothing for the operator.
			agg, err := h.store.Aggregate(context.Background(), store.AggregateOptions{
				GroupBy: "agent",
				Since:   time.Now().Add(-time.Hour),
				Until:   time.Now().Add(time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(agg) != 1 {
				t.Fatalf("aggregate rows = %d, want 1", len(agg))
			}
			wantUnpriced := int64(0)
			if tc.wantUsageStatus == store.UsageUnparsed || tc.wantUsageStatus == store.UsageModelUnpriced {
				wantUnpriced = 1
			}
			if agg[0].UnpricedRequests != wantUnpriced {
				t.Errorf("aggregate unpriced_requests = %d, want %d",
					agg[0].UnpricedRequests, wantUnpriced)
			}
		})
	}
}

// TestCorrectness_UnparseableUsageIsLoggedLoudly checks the operator-facing half
// of C3: a flagged row is useless if nothing ever says it happened.
//
// INVARIANT: an unreadable usage block produces a warning naming the agent.
func TestCorrectness_UnparseableUsageIsLoggedLoudly(t *testing.T) {
	h := newHarness(t, harnessOptions{
		agents:   testAgents,
		upstream: jsonUpstream(`{"model":"claude-haiku-4-5","content":[]}`),
	})
	h.do(t, testKey, "/v1/messages", `{"model":"claude-haiku-4-5"}`, nil)
	h.waitForRecords(1)

	if n := h.logs.count("response carried no parseable usage"); n != 1 {
		t.Errorf("unparseable-usage warnings = %d, want 1\n%s", n, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "support-bot") {
		t.Error("warning does not name the agent whose spend is unaccounted for")
	}
}
