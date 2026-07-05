// Package pricing converts token usage into dollar cost using a versioned
// rate table. The default table ships embedded in the binary; pricing.yaml in
// this directory is the source of truth we maintain.
package pricing

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/opslync/tollgate/internal/meter"
)

//go:embed pricing.yaml
var embedded []byte

type ModelRates struct {
	InputPerMTok      float64 `yaml:"input_per_mtok"`
	OutputPerMTok     float64 `yaml:"output_per_mtok"`
	CacheWritePerMTok float64 `yaml:"cache_write_per_mtok"`
	CacheReadPerMTok  float64 `yaml:"cache_read_per_mtok"`
}

type Table struct {
	Version string                `yaml:"version"`
	Models  map[string]ModelRates `yaml:"models"`
}

// Load returns the pricing table embedded at build time.
func Load() (*Table, error) {
	var t Table
	if err := yaml.Unmarshal(embedded, &t); err != nil {
		return nil, fmt.Errorf("parse embedded pricing table: %w", err)
	}
	if t.Version == "" || len(t.Models) == 0 {
		return nil, fmt.Errorf("embedded pricing table is missing version or models")
	}
	return &t, nil
}

// rates resolves a model ID to its rates: exact match first, then the longest
// table entry that is a dash-boundary prefix — so a dated ID like
// "claude-haiku-4-5-20251001" resolves to "claude-haiku-4-5".
func (t *Table) rates(model string) (ModelRates, bool) {
	if r, ok := t.Models[model]; ok {
		return r, true
	}
	var bestKey string
	for key := range t.Models {
		if len(key) > len(bestKey) && strings.HasPrefix(model, key+"-") {
			bestKey = key
		}
	}
	if bestKey == "" {
		return ModelRates{}, false
	}
	return t.Models[bestKey], true
}

// Cost returns the dollar cost of a request's usage; ok is false when the
// model has no entry in the table (cost is then 0 and should be flagged).
func (t *Table) Cost(model string, u meter.Usage) (costUSD float64, ok bool) {
	r, ok := t.rates(model)
	if !ok {
		return 0, false
	}
	const mtok = 1e6
	cost := float64(u.InputTokens)*r.InputPerMTok/mtok +
		float64(u.OutputTokens)*r.OutputPerMTok/mtok +
		float64(u.CacheCreationInputTokens)*r.CacheWritePerMTok/mtok +
		float64(u.CacheReadInputTokens)*r.CacheReadPerMTok/mtok
	return cost, true
}
