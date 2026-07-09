package metrics

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/budget"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

func newEngine(t *testing.T, budgets []config.Budget) *budget.Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return budget.New(st, budgets, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func usdBudget(agent string, limit float64, action string) config.Budget {
	return config.Budget{
		Agent: agent, Window: config.Duration(time.Hour),
		LimitUSD: limit, AlertAt: 0.8, Action: action,
		ThrottleInterval: config.Duration(30 * time.Second),
	}
}

// prime runs the initial store re-sync so a following Record survives like it
// would in production.
func prime(t *testing.T, e *budget.Engine, agent auth.Agent) {
	t.Helper()
	if d := e.Check(context.Background(), agent); d.Kind != budget.Allow {
		t.Fatalf("prime: decision = %v, want Allow", d.Kind)
	}
}

func TestBudgetCollector(t *testing.T) {
	agent := auth.Agent{Name: "support-bot"}
	tests := []struct {
		name    string
		budgets []config.Budget
		record  float64 // usd recorded after prime
		kill    bool
		want    string
	}{
		{
			name:    "under budget",
			budgets: []config.Budget{usdBudget("support-bot", 10, "block")},
			record:  5.0,
			want: `
tollgate_budget_consumed_ratio{dimension="agent",target="support-bot"} 0.5
tollgate_budget_state{dimension="agent",target="support-bot"} 0
`,
		},
		{
			name:    "near limit alerts",
			budgets: []config.Budget{usdBudget("support-bot", 10, "block")},
			record:  8.5,
			want: `
tollgate_budget_consumed_ratio{dimension="agent",target="support-bot"} 0.85
tollgate_budget_state{dimension="agent",target="support-bot"} 1
`,
		},
		{
			name:    "over throttle budget",
			budgets: []config.Budget{usdBudget("support-bot", 10, "throttle")},
			record:  12.0,
			want: `
tollgate_budget_consumed_ratio{dimension="agent",target="support-bot"} 1.2
tollgate_budget_state{dimension="agent",target="support-bot"} 2
`,
		},
		{
			name:    "over block budget",
			budgets: []config.Budget{usdBudget("support-bot", 10, "block")},
			record:  15.0,
			want: `
tollgate_budget_consumed_ratio{dimension="agent",target="support-bot"} 1.5
tollgate_budget_state{dimension="agent",target="support-bot"} 3
`,
		},
		{
			name:    "killed with budget keeps ratio",
			budgets: []config.Budget{usdBudget("support-bot", 10, "block")},
			record:  5.0,
			kill:    true,
			want: `
tollgate_budget_consumed_ratio{dimension="agent",target="support-bot"} 0.5
tollgate_budget_state{dimension="agent",target="support-bot"} 3
`,
		},
		{
			name: "killed without budget has no ratio",
			kill: true,
			want: `
tollgate_budget_state{dimension="agent",target="support-bot"} 3
`,
		},
	}

	const header = `# HELP tollgate_budget_consumed_ratio Fraction of a budget consumed (0-1+), by dimension and target.
# TYPE tollgate_budget_consumed_ratio gauge
# HELP tollgate_budget_state Budget enforcement state: 0=ok, 1=alert, 2=throttled, 3=blocked.
# TYPE tollgate_budget_state gauge
`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine(t, tt.budgets)
			if len(tt.budgets) > 0 {
				prime(t, e, agent)
				e.Record(agent, 0, tt.record)
			}
			if tt.kill {
				if err := e.Kill(context.Background(), agent.Name); err != nil {
					t.Fatal(err)
				}
			}
			// A killed-without-budget case emits no ratio family, so its HELP/TYPE
			// header must be dropped too for the compare to line up.
			header := header
			if !strings.Contains(tt.want, "consumed_ratio") {
				header = `# HELP tollgate_budget_state Budget enforcement state: 0=ok, 1=alert, 2=throttled, 3=blocked.
# TYPE tollgate_budget_state gauge
`
			}
			if err := testutil.CollectAndCompare(NewBudgetCollector(e), strings.NewReader(header+strings.TrimLeft(tt.want, "\n"))); err != nil {
				t.Error(err)
			}
		})
	}
}
