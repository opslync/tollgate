package budget

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

var (
	supportBot = auth.Agent{Name: "support-bot", Team: "support"}
	otherBot   = auth.Agent{Name: "other-bot", Team: "research"}
)

func newTestEngine(t *testing.T, budgets []config.Budget) (*Engine, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := New(st, budgets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	e.now = func() time.Time { return now }
	return e, st, &now
}

// prime runs the engine's initial store re-sync for the agent's budgets, so
// subsequent Record calls behave like production (where every Record follows
// a store insert and survives re-syncs).
func prime(t *testing.T, e *Engine, agent auth.Agent) {
	t.Helper()
	if d := e.Check(context.Background(), agent); d.Kind != Allow {
		t.Fatalf("prime: decision = %v, want Allow", d.Kind)
	}
}

func usdBudget(agent string, limit float64, action string) config.Budget {
	return config.Budget{
		Agent: agent, Window: config.Duration(time.Hour),
		LimitUSD: limit, AlertAt: 0.8, Action: action,
		ThrottleInterval: config.Duration(30 * time.Second),
	}
}

func TestAllowUnderBudget(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	if d := e.Check(context.Background(), supportBot); d.Kind != Allow {
		t.Errorf("decision = %v, want Allow", d.Kind)
	}
}

func TestBlockAtLimitViaLiveIncrements(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "block")})
	ctx := context.Background()

	// Runaway loop: no store writes at all, only live increments — the
	// engine must block without waiting for a re-sync.
	for i := 0; i < 10; i++ {
		if d := e.Check(ctx, supportBot); d.Kind != Allow {
			t.Fatalf("request %d: decision = %v, want Allow", i, d.Kind)
		}
		e.Record(supportBot, 100, 0.11)
	}
	d := e.Check(ctx, supportBot)
	if d.Kind != BlockedBudget {
		t.Fatalf("decision = %v, want BlockedBudget (spend $1.10 >= $1)", d.Kind)
	}
	if d.SpendUSD < 1.0 || d.Budget.LimitUSD != 1.0 {
		t.Errorf("decision detail = %+v", d)
	}

	// Unrelated agent is unaffected.
	if d := e.Check(ctx, otherBot); d.Kind != Allow {
		t.Errorf("other agent = %v, want Allow", d.Kind)
	}
}

func TestBlockFromStoredSpend(t *testing.T) {
	e, st, now := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "block")})
	ctx := context.Background()

	// Spend already in the store (e.g. from before a restart).
	st.Insert(ctx, store.Record{Time: *now, Agent: "support-bot", Team: "support", Provider: "p", Status: 200, CostUSD: 1.5})

	if d := e.Check(ctx, supportBot); d.Kind != BlockedBudget {
		t.Errorf("decision = %v, want BlockedBudget from persisted spend", d.Kind)
	}
}

func TestWindowAgingUnblocks(t *testing.T) {
	e, st, now := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "block")})
	ctx := context.Background()

	st.Insert(ctx, store.Record{Time: *now, Agent: "support-bot", Team: "support", Provider: "p", Status: 200, CostUSD: 1.5})
	if d := e.Check(ctx, supportBot); d.Kind != BlockedBudget {
		t.Fatalf("decision = %v, want BlockedBudget", d.Kind)
	}

	// Two hours later the spend is outside the 1h window; the refresh must
	// clear the counters and allow again.
	*now = now.Add(2 * time.Hour)
	if d := e.Check(ctx, supportBot); d.Kind != Allow {
		t.Errorf("decision after window aged = %v, want Allow", d.Kind)
	}
}

func TestThrottleTrickle(t *testing.T) {
	e, st, now := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "throttle")})
	ctx := context.Background()
	// Already over: spend sits in the store, picked up by the first re-sync.
	st.Insert(ctx, store.Record{Time: *now, Agent: "support-bot", Team: "support", Provider: "p", Status: 200, CostUSD: 2.0})

	// First over-limit request is the allowed trickle.
	if d := e.Check(ctx, supportBot); d.Kind != Allow {
		t.Fatalf("first = %v, want Allow (trickle)", d.Kind)
	}
	// Immediately after: throttled with Retry-After.
	d := e.Check(ctx, supportBot)
	if d.Kind != Throttled {
		t.Fatalf("second = %v, want Throttled", d.Kind)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 30*time.Second {
		t.Errorf("RetryAfter = %v", d.RetryAfter)
	}
	// After the interval, one more is allowed.
	*now = now.Add(31 * time.Second)
	if d := e.Check(ctx, supportBot); d.Kind != Allow {
		t.Errorf("after interval = %v, want Allow", d.Kind)
	}
}

func TestTokenLimit(t *testing.T) {
	b := config.Budget{
		Agent: "support-bot", Window: config.Duration(time.Hour),
		LimitTokens: 1000, AlertAt: 0.8, Action: "block",
	}
	e, _, _ := newTestEngine(t, []config.Budget{b})
	ctx := context.Background()

	prime(t, e, supportBot)
	e.Record(supportBot, 999, 0)
	if d := e.Check(ctx, supportBot); d.Kind != Allow {
		t.Fatalf("999 tokens = %v, want Allow", d.Kind)
	}
	e.Record(supportBot, 1, 0)
	if d := e.Check(ctx, supportBot); d.Kind != BlockedBudget {
		t.Errorf("1000 tokens = %v, want BlockedBudget", d.Kind)
	}
}

func TestTeamBudgetAggregatesAgents(t *testing.T) {
	b := config.Budget{
		Team: "support", Window: config.Duration(time.Hour),
		LimitUSD: 1.0, AlertAt: 0.8, Action: "block",
	}
	e, _, _ := newTestEngine(t, []config.Budget{b})
	ctx := context.Background()

	teammate := auth.Agent{Name: "teammate-bot", Team: "support"}
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 0.6)
	e.Record(teammate, 0, 0.6)

	// Both agents share the team budget and both are blocked.
	if d := e.Check(ctx, supportBot); d.Kind != BlockedBudget {
		t.Errorf("support-bot = %v, want BlockedBudget", d.Kind)
	}
	if d := e.Check(ctx, teammate); d.Kind != BlockedBudget {
		t.Errorf("teammate = %v, want BlockedBudget", d.Kind)
	}
	if d := e.Check(ctx, otherBot); d.Kind != Allow {
		t.Errorf("other team = %v, want Allow", d.Kind)
	}
}

func TestMostRestrictiveWins(t *testing.T) {
	budgets := []config.Budget{
		usdBudget("support-bot", 1.0, "throttle"),
		{Team: "support", Window: config.Duration(time.Hour), LimitUSD: 1.0, AlertAt: 0.8, Action: "block"},
	}
	e, _, _ := newTestEngine(t, budgets)
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 2.0)

	if d := e.Check(context.Background(), supportBot); d.Kind != BlockedBudget {
		t.Errorf("decision = %v, want BlockedBudget (block beats throttle)", d.Kind)
	}
}

func TestAlertOnce(t *testing.T) {
	var logs bytes.Buffer
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := New(st, []config.Budget{usdBudget("support-bot", 1.0, "block")}, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()

	if d := e.Check(ctx, supportBot); d.Kind != Allow { // initial re-sync
		t.Fatalf("prime: %v", d.Kind)
	}
	e.Record(supportBot, 0, 0.85) // over 80% threshold, under limit
	for i := 0; i < 3; i++ {
		if d := e.Check(ctx, supportBot); d.Kind != Allow {
			t.Fatalf("decision = %v, want Allow", d.Kind)
		}
	}
	if got := bytes.Count(logs.Bytes(), []byte("budget alert threshold crossed")); got != 1 {
		t.Errorf("alert logged %d times, want exactly 1:\n%s", got, logs.String())
	}
}

func TestKillSwitch(t *testing.T) {
	e, st, _ := newTestEngine(t, nil)
	ctx := context.Background()

	if err := e.Kill(ctx, "support-bot"); err != nil {
		t.Fatal(err)
	}
	if d := e.Check(ctx, supportBot); d.Kind != BlockedKilled {
		t.Fatalf("decision = %v, want BlockedKilled", d.Kind)
	}
	if d := e.Check(ctx, otherBot); d.Kind != Allow {
		t.Errorf("other agent = %v, want Allow", d.Kind)
	}

	// A fresh engine (simulated restart) inherits the kill from the store.
	e2 := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e2.LoadKills(ctx); err != nil {
		t.Fatal(err)
	}
	if d := e2.Check(ctx, supportBot); d.Kind != BlockedKilled {
		t.Errorf("after restart = %v, want BlockedKilled", d.Kind)
	}

	if err := e2.Revive(ctx, "support-bot"); err != nil {
		t.Fatal(err)
	}
	if d := e2.Check(ctx, supportBot); d.Kind != Allow {
		t.Errorf("after revive = %v, want Allow", d.Kind)
	}
}
