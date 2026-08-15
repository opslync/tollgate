package budget

// Group A — durability. Does spend survive the process dying?
//
// These are white-box tests (package budget) so they can read the engine's
// in-memory counters directly and drive its clock, exactly as the existing
// budget_test.go helpers already do. See docs/correctness.md for the invariant
// table these comments feed.

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/internal/store"
)

// usageTokens builds a usage block whose store-side token total (input+output)
// equals n, matching what the engine is told via Record.
func usageTokens(n int64) meter.Usage {
	return meter.Usage{Model: "claude-haiku-4-5", InputTokens: n}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// openEngine opens (or reopens) the SQLite file at path and builds an engine
// over it with a caller-controlled clock — the "restart" primitive for group A.
func openEngine(t *testing.T, path string, budgets []config.Budget, now *time.Time) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := New(st, budgets, discardLogger())
	e.now = func() time.Time { return *now }
	return e, st
}

// spendOf reads a tracked budget's current in-memory counter (base + live
// delta) — the number enforcement actually decides on.
func spendOf(t *testing.T, e *Engine, target string) (usd float64, tokens int64) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, b := range e.budgets {
		if b.value == target {
			return b.usd(), b.tokens()
		}
	}
	t.Fatalf("no budget tracked for %q", target)
	return 0, 0
}

// cents rounds a dollar amount to whole cents so assertions compare money, not
// float64 bit patterns.
func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

// storeRecordFor is the row a completed request would persist.
func storeRecordFor(agent auth.Agent, at time.Time, tokens int64, usd float64) store.Record {
	return store.Record{
		Time: at, Agent: agent.Name, Team: agent.Team, Provider: "test",
		Status: 200, Usage: usageTokens(tokens), CostUSD: usd,
		UsageStatus: store.UsageOK,
	}
}

// recordRequest performs a completed request the way production does: persist
// the row first, then increment the engine's live counter (the ordering in
// cmd/tollgate's newRecorder).
func recordRequest(t *testing.T, st *store.Store, e *Engine, agent auth.Agent, at time.Time, tokens int64, usd float64) {
	t.Helper()
	if err := st.Insert(context.Background(), storeRecordFor(agent, at, tokens, usd)); err != nil {
		t.Fatal(err)
	}
	e.Record(agent, tokens, usd)
}

// TestCorrectness_RestartPreservesSpend covers A1.
//
// INVARIANT: spend recorded before a restart is still counted after it.
func TestCorrectness_RestartPreservesSpend(t *testing.T) {
	// Two agents with deliberately different states: one comfortably under its
	// alert threshold, one already past it. A restart must restore the state,
	// not merely a number that happens to be non-zero.
	budgets := []config.Budget{
		usdBudget("thrifty-bot", 10.0, "block"),
		usdBudget("spendy-bot", 10.0, "block"),
	}
	thrifty := auth.Agent{Name: "thrifty-bot", Team: "support"}
	spendy := auth.Agent{Name: "spendy-bot", Team: "support"}

	path := filepath.Join(t.TempDir(), "spend.db")
	now := time.Now()

	e1, st1 := openEngine(t, path, budgets, &now)
	prime(t, e1, thrifty)
	prime(t, e1, spendy)

	// $0.07 x 13 = $0.91 (9.1% of the limit) and $0.07 x 123 = $8.61 (86.1%,
	// past the 0.8 alert threshold). Amounts chosen to be inexact in binary
	// floating point, so a sloppy round-trip shows up.
	for i := 0; i < 13; i++ {
		recordRequest(t, st1, e1, thrifty, now, 100, 0.07)
	}
	for i := 0; i < 123; i++ {
		recordRequest(t, st1, e1, spendy, now, 100, 0.07)
	}

	thriftyUSD, thriftyTokens := spendOf(t, e1, "thrifty-bot")
	spendyUSD, spendyTokens := spendOf(t, e1, "spendy-bot")
	if cents(thriftyUSD) != 91 || cents(spendyUSD) != 861 {
		t.Fatalf("pre-restart spend = $%.4f / $%.4f, want $0.91 / $8.61", thriftyUSD, spendyUSD)
	}

	// Hard stop and cold start from the same file.
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}
	e2, st2 := openEngine(t, path, budgets, &now)
	t.Cleanup(func() { st2.Close() })

	// The first Check is what seeds the counters from SQLite.
	if d := e2.Check(context.Background(), thrifty); d.Kind != Allow {
		t.Fatalf("thrifty-bot after restart = %v, want Allow", d.Kind)
	}
	if d := e2.Check(context.Background(), spendy); d.Kind != Allow {
		t.Fatalf("spendy-bot after restart = %v, want Allow (86%% of limit)", d.Kind)
	}

	gotThriftyUSD, gotThriftyTokens := spendOf(t, e2, "thrifty-bot")
	gotSpendyUSD, gotSpendyTokens := spendOf(t, e2, "spendy-bot")
	if cents(gotThriftyUSD) != cents(thriftyUSD) {
		t.Errorf("thrifty-bot spend after restart = %d cents, want %d", cents(gotThriftyUSD), cents(thriftyUSD))
	}
	if cents(gotSpendyUSD) != cents(spendyUSD) {
		t.Errorf("spendy-bot spend after restart = %d cents, want %d", cents(gotSpendyUSD), cents(spendyUSD))
	}
	if gotThriftyTokens != thriftyTokens || gotSpendyTokens != spendyTokens {
		t.Errorf("tokens after restart = %d / %d, want %d / %d",
			gotThriftyTokens, gotSpendyTokens, thriftyTokens, spendyTokens)
	}

	// State, not just the number: the restarted engine must still see one
	// agent below its alert threshold and the other above it.
	e2.mu.Lock()
	defer e2.mu.Unlock()
	for _, b := range e2.budgets {
		wantNear := b.value == "spendy-bot"
		if b.nearLimit() != wantNear {
			t.Errorf("%s nearLimit() = %v after restart, want %v", b.value, b.nearLimit(), wantNear)
		}
		if b.over() {
			t.Errorf("%s over() = true after restart, want false", b.value)
		}
	}

	// Documented limitation, not asserted as a failure: the `alerted` de-dup
	// flag is in-memory only, so the agent that had already alerted before the
	// restart will log its "budget alert threshold crossed" line once more
	// after it. Spend accounting is unaffected; only the log is duplicated.
}

// TestCorrectness_RestartPreservesKill covers A2.
//
// INVARIANT: a killed agent stays killed across a restart, from the very first
// request — not from the first re-sync tick.
func TestCorrectness_RestartPreservesKill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kill.db")
	now := time.Now()

	e1, st1 := openEngine(t, path, nil, &now)
	if err := e1.Kill(context.Background(), "runaway-bot"); err != nil {
		t.Fatal(err)
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	e2, st2 := openEngine(t, path, nil, &now)
	t.Cleanup(func() { st2.Close() })
	// A re-sync must not be what rescues us: make the interval longer than the
	// test could ever run, so only LoadKills can supply the kill state.
	e2.refreshEvery = time.Hour
	if err := e2.LoadKills(context.Background()); err != nil {
		t.Fatal(err)
	}

	runaway := auth.Agent{Name: "runaway-bot", Team: "support"}
	if d := e2.Check(context.Background(), runaway); d.Kind != BlockedKilled {
		t.Errorf("first Check after restart = %v, want BlockedKilled", d.Kind)
	}
	if d := e2.Check(context.Background(), auth.Agent{Name: "other-bot"}); d.Kind != Allow {
		t.Errorf("unrelated agent after restart = %v, want Allow", d.Kind)
	}

	// And a revive is equally durable in the other direction.
	if err := e2.Revive(context.Background(), "runaway-bot"); err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	e3, st3 := openEngine(t, path, nil, &now)
	t.Cleanup(func() { st3.Close() })
	if err := e3.LoadKills(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := e3.Check(context.Background(), runaway); d.Kind != Allow {
		t.Errorf("after revive + restart = %v, want Allow", d.Kind)
	}
}

// TestCorrectness_MidWindowRestartPreservesWindow covers A4.
//
// INVARIANT: a rolling window's elapsed portion is preserved across a restart —
// spend from before the restart still counts, and still ages out on schedule
// rather than restarting its clock at boot.
func TestCorrectness_MidWindowRestartPreservesWindow(t *testing.T) {
	budgets := []config.Budget{usdBudget("support-bot", 1.0, "block")}
	path := filepath.Join(t.TempDir(), "window.db")

	start := time.Now()
	now := start

	// Spend lands 30 minutes into a 1h window, then the process dies.
	e1, st1 := openEngine(t, path, budgets, &now)
	prime(t, e1, supportBot)
	recordRequest(t, st1, e1, supportBot, now.Add(-30*time.Minute), 1000, 1.5)
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	e2, st2 := openEngine(t, path, budgets, &now)
	t.Cleanup(func() { st2.Close() })

	// t+0 after restart: the 30-minute-old spend is inside the window.
	if d := e2.Check(context.Background(), supportBot); d.Kind != BlockedBudget {
		t.Fatalf("immediately after restart = %v, want BlockedBudget ($1.50 of $1 limit)", d.Kind)
	}
	if usd, _ := spendOf(t, e2, "support-bot"); cents(usd) != 150 {
		t.Errorf("spend after restart = %d cents, want 150", cents(usd))
	}

	// t+29m: the spend is 59 minutes old — still inside the 1h window.
	now = start.Add(29 * time.Minute)
	if d := e2.Check(context.Background(), supportBot); d.Kind != BlockedBudget {
		t.Errorf("at t+29m = %v, want BlockedBudget (spend is 59m old)", d.Kind)
	}

	// t+31m: the spend is 61 minutes old — the window has moved past it, and
	// the re-sync (not a restart) is what ages it out.
	now = start.Add(31 * time.Minute)
	if d := e2.Check(context.Background(), supportBot); d.Kind != Allow {
		t.Errorf("at t+31m = %v, want Allow (spend is 61m old, outside the 1h window)", d.Kind)
	}
	if usd, _ := spendOf(t, e2, "support-bot"); cents(usd) != 0 {
		t.Errorf("spend after window aged out = %d cents, want 0", cents(usd))
	}
}
