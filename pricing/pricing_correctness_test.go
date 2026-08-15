package pricing

// Group C4 — model resolution. The storage half (that an unpriced model is
// flagged rather than silently written as a $0 row) lives in cmd/tollgate.

import (
	"math"
	"testing"

	"github.com/opslync/tollgate/internal/meter"
)

// TestCorrectness_UnknownModelIsNotPricedAtZero covers C4.
//
// INVARIANT: a model missing from pricing.yaml returns ok=false, so its $0 is
// visibly "we don't know" rather than "it was free".
//
// The failure this guards against: resolving an unknown model to zero rates and
// recording a confident $0. An agent on a model we have not added to the table
// would then appear to cost nothing and would never trip a budget.
func TestCorrectness_UnknownModelIsNotPricedAtZero(t *testing.T) {
	tbl, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	// 1M input + 1M output, so expected costs are just the per-MTok rates.
	usage := meter.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	tests := []struct {
		name     string
		model    string
		wantOK   bool
		wantUSD  float64 // only checked when wantOK
		whyMatch string
	}{
		{
			name:     "exact match",
			model:    "claude-haiku-4-5",
			wantOK:   true,
			wantUSD:  1.00 + 5.00,
			whyMatch: "the table key itself",
		},
		{
			name:     "dated snapshot resolves to its base model",
			model:    "claude-haiku-4-5-20251001",
			wantOK:   true,
			wantUSD:  1.00 + 5.00,
			whyMatch: "longest dash-boundary prefix",
		},
		{
			name:     "longest prefix wins over a shorter one",
			model:    "gpt-5-mini-2026-01-01",
			wantOK:   true,
			wantUSD:  0.25 + 2.00, // gpt-5-mini rates, not gpt-5's
			whyMatch: "gpt-5-mini is longer than gpt-5",
		},
		{
			name:   "unknown vendor is a miss",
			model:  "llama-3-70b-instruct",
			wantOK: false,
		},
		{
			name:   "empty model is a miss",
			model:  "",
			wantOK: false,
		},
		{
			name:   "prefix without a dash boundary is a miss",
			model:  "claude-haiku-4-56",
			wantOK: false,
		},
		{
			name:   "a table key that is a suffix, not a prefix, is a miss",
			model:  "custom-claude-haiku-4-5",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost, ok := tbl.Cost(tc.model, usage)
			if ok != tc.wantOK {
				t.Fatalf("Cost(%q) ok = %v, want %v (%s)", tc.model, ok, tc.wantOK, tc.whyMatch)
			}
			if !tc.wantOK {
				if cost != 0 {
					t.Errorf("Cost(%q) = %v on a miss, want 0", tc.model, cost)
				}
				return
			}
			if math.Abs(cost-tc.wantUSD) > 1e-9 {
				t.Errorf("Cost(%q) = %.6f, want %.6f", tc.model, cost, tc.wantUSD)
			}
		})
	}

	// The distinction that makes the flag worth storing: a priced request that
	// genuinely costs nothing and an unpriced request both report $0, and only
	// the ok flag tells them apart.
	freeCost, freeOK := tbl.Cost("claude-haiku-4-5", meter.Usage{})
	missCost, missOK := tbl.Cost("llama-3-70b-instruct", usage)
	if freeCost != 0 || !freeOK {
		t.Errorf("zero-usage priced request = (%v, %v), want (0, true)", freeCost, freeOK)
	}
	if missCost != 0 || missOK {
		t.Errorf("unpriced model = (%v, %v), want (0, false)", missCost, missOK)
	}
}
