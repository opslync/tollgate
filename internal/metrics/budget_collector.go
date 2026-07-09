package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/opslync/tollgate/internal/budget"
)

var (
	budgetConsumedRatioDesc = prometheus.NewDesc(
		"tollgate_budget_consumed_ratio",
		"Fraction of a budget consumed (0-1+), by dimension and target.",
		[]string{"dimension", "target"}, nil)
	budgetStateDesc = prometheus.NewDesc(
		"tollgate_budget_state",
		"Budget enforcement state: 0=ok, 1=alert, 2=throttled, 3=blocked.",
		[]string{"dimension", "target"}, nil)
)

// BudgetCollector emits the budget gauges freshly at each scrape from a
// snapshot of the engine's live counters — the correct pull-based shape, so the
// gauges never drift the way a per-request push would.
type BudgetCollector struct {
	engine *budget.Engine
}

func NewBudgetCollector(engine *budget.Engine) *BudgetCollector {
	return &BudgetCollector{engine: engine}
}

func (c *BudgetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- budgetConsumedRatioDesc
	ch <- budgetStateDesc
}

func (c *BudgetCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.engine.States() {
		ch <- prometheus.MustNewConstMetric(budgetStateDesc, prometheus.GaugeValue, float64(s.State), s.Dimension, s.Target)
		if s.HasRatio {
			ch <- prometheus.MustNewConstMetric(budgetConsumedRatioDesc, prometheus.GaugeValue, s.Ratio, s.Dimension, s.Target)
		}
	}
}
