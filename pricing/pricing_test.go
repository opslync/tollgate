package pricing

import (
	"math"
	"testing"

	"github.com/opslync/tollgate/internal/meter"
)

func TestLoadEmbedded(t *testing.T) {
	tbl, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Version == "" {
		t.Error("version missing")
	}
	for _, model := range []string{"claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5"} {
		if _, ok := tbl.Models[model]; !ok {
			t.Errorf("table missing %s", model)
		}
	}
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCost(t *testing.T) {
	tbl := &Table{
		Version: "test",
		Models: map[string]ModelRates{
			"claude-sonnet-5": {InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.30},
		},
	}
	u := meter.Usage{
		InputTokens:              1_000_000,
		OutputTokens:             200_000,
		CacheCreationInputTokens: 400_000,
		CacheReadInputTokens:     2_000_000,
	}
	cost, ok := tbl.Cost("claude-sonnet-5", u)
	if !ok {
		t.Fatal("model not found")
	}
	// 3 + 0.2*15 + 0.4*3.75 + 2*0.30 = 3 + 3 + 1.5 + 0.6
	if want := 8.1; !almostEqual(cost, want) {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

func TestPrefixMatching(t *testing.T) {
	tbl := &Table{
		Version: "test",
		Models: map[string]ModelRates{
			"claude-haiku-4-5": {InputPerMTok: 1, OutputPerMTok: 5},
			"claude-haiku-4":   {InputPerMTok: 9, OutputPerMTok: 9},
		},
	}
	tests := []struct {
		model    string
		wantRate float64
		wantOK   bool
	}{
		{"claude-haiku-4-5", 1, true},          // exact
		{"claude-haiku-4-5-20251001", 1, true}, // dated ID → longest prefix
		{"claude-haiku-4-5000", 9, true},       // no dash boundary for -4-5; falls to -4 entry
		{"claude-haiku-4-9", 9, true},          // matches shorter entry
		{"gpt-4o", 0, false},                   // unknown model
	}
	for _, tt := range tests {
		cost, ok := tbl.Cost(tt.model, meter.Usage{InputTokens: 1_000_000})
		if ok != tt.wantOK {
			t.Errorf("%s: ok = %v, want %v", tt.model, ok, tt.wantOK)
			continue
		}
		if ok && !almostEqual(cost, tt.wantRate) {
			t.Errorf("%s: cost = %v, want %v", tt.model, cost, tt.wantRate)
		}
	}
}

func TestUnknownModelCostsZero(t *testing.T) {
	tbl, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cost, ok := tbl.Cost("some-local-vllm-model", meter.Usage{InputTokens: 5000})
	if ok || cost != 0 {
		t.Errorf("cost = %v ok = %v, want 0 false", cost, ok)
	}
}
