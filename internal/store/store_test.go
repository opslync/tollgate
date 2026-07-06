package store

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/meter"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func record(agent, team, model string, in, out int64, cost float64, at time.Time) Record {
	return Record{
		Time: at, Agent: agent, Team: team, Namespace: "prod",
		Provider: "anthropic", Model: model, Method: "POST", Path: "/v1/messages",
		Status: 200, DurationMS: 42, Stream: false,
		Usage:   meter.Usage{Model: model, InputTokens: in, OutputTokens: out},
		CostUSD: cost,
	}
}

func TestInsertAndAggregate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, r := range []Record{
		record("support-bot", "support", "claude-sonnet-5", 100, 50, 0.001, now),
		record("support-bot", "support", "claude-sonnet-5", 200, 100, 0.002, now),
		record("research-bot", "research", "claude-opus-4-8", 1000, 500, 0.02, now),
		record("old-bot", "support", "claude-sonnet-5", 999, 999, 9.99, now.Add(-48*time.Hour)), // outside window
	} {
		if err := s.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.Aggregate(ctx, AggregateOptions{
		GroupBy: "agent",
		Since:   now.Add(-24 * time.Hour),
		Until:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (48h-old record must be excluded): %+v", len(rows), rows)
	}
	// Sorted by cost desc: research-bot first.
	if rows[0].Key != "research-bot" || rows[0].Requests != 1 || rows[0].InputTokens != 1000 {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1].Key != "support-bot" || rows[1].Requests != 2 || rows[1].InputTokens != 300 ||
		rows[1].OutputTokens != 150 || math.Abs(rows[1].CostUSD-0.003) > 1e-9 {
		t.Errorf("rows[1] = %+v", rows[1])
	}
}

func TestAggregateGroupByAndFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	s.Insert(ctx, record("a", "t1", "m1", 10, 5, 0.1, now))
	s.Insert(ctx, record("b", "t1", "m2", 20, 10, 0.2, now))
	s.Insert(ctx, record("c", "t2", "m1", 30, 15, 0.5, now))

	window := AggregateOptions{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}

	byTeam := window
	byTeam.GroupBy = "team"
	rows, err := s.Aggregate(ctx, byTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "t2" || rows[1].InputTokens != 30 {
		t.Errorf("group by team = %+v", rows)
	}

	byModel := window
	byModel.GroupBy = "model"
	byModel.Model = "m1"
	rows, err = s.Aggregate(ctx, byModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "m1" || rows[0].Requests != 2 {
		t.Errorf("model filter = %+v", rows)
	}

	if _, err := s.Aggregate(ctx, AggregateOptions{GroupBy: "id; DROP TABLE requests"}); err == nil {
		t.Error("expected error for non-allowlisted group_by")
	}
}

func TestSpend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	s.Insert(ctx, record("a", "t1", "m", 100, 50, 0.5, now))
	s.Insert(ctx, record("a", "t1", "m", 200, 100, 1.0, now))
	s.Insert(ctx, record("b", "t1", "m", 1000, 0, 3.0, now))
	s.Insert(ctx, record("a", "t1", "m", 9999, 0, 99, now.Add(-2*time.Hour))) // outside window

	usd, tokens, err := s.Spend(ctx, "agent", "a", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(usd-1.5) > 1e-9 || tokens != 450 {
		t.Errorf("agent spend = $%v / %d tokens, want $1.5 / 450", usd, tokens)
	}

	usd, tokens, err = s.Spend(ctx, "team", "t1", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(usd-4.5) > 1e-9 || tokens != 1450 {
		t.Errorf("team spend = $%v / %d tokens, want $4.5 / 1450", usd, tokens)
	}

	usd, tokens, err = s.Spend(ctx, "agent", "nobody", now.Add(-time.Hour))
	if err != nil || usd != 0 || tokens != 0 {
		t.Errorf("empty spend = $%v / %d / %v, want zeros", usd, tokens, err)
	}

	if _, _, err := s.Spend(ctx, "password", "x", now); err == nil {
		t.Error("expected error for invalid dimension")
	}
}

func TestKillPersistence(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Kill(ctx, "runaway-bot", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Kill(ctx, "runaway-bot", time.Now()); err != nil {
		t.Fatalf("double kill must be idempotent: %v", err)
	}
	kills, err := s.Kills(ctx)
	if err != nil || len(kills) != 1 || kills[0] != "runaway-bot" {
		t.Fatalf("kills = %v, %v", kills, err)
	}
	if err := s.Revive(ctx, "runaway-bot"); err != nil {
		t.Fatal(err)
	}
	kills, _ = s.Kills(ctx)
	if len(kills) != 0 {
		t.Errorf("kills after revive = %v", kills)
	}
}

func TestWorkloadAttribution(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	r := record("payments/checkout-worker", "payments", "claude-sonnet-5", 100, 50, 0.01, now)
	r.Pod = "checkout-worker-abc123"
	r.WorkloadKind = "Deployment"
	r.Workload = "checkout-worker"
	r.ServiceAccount = "checkout"
	if err := s.Insert(ctx, r); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Aggregate(ctx, AggregateOptions{
		GroupBy: "deployment",
		Since:   now.Add(-time.Hour),
		Until:   now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "checkout-worker" || rows[0].Requests != 1 {
		t.Errorf("group by deployment = %+v", rows)
	}
}

// TestFreshDBColumnDefaults verifies a record inserted without workload fields
// stores empty-string defaults for the four M7 columns.
func TestFreshDBColumnDefaults(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, record("a", "t", "m", 1, 1, 0.1, time.Now())); err != nil {
		t.Fatal(err)
	}
	var pod, kind, workload, sa string
	row := s.db.QueryRowContext(ctx, `SELECT pod, workload_kind, workload, service_account FROM requests LIMIT 1`)
	if err := row.Scan(&pod, &kind, &workload, &sa); err != nil {
		t.Fatal(err)
	}
	if pod != "" || kind != "" || workload != "" || sa != "" {
		t.Errorf("defaults = %q/%q/%q/%q, want empty", pod, kind, workload, sa)
	}
}

// TestMigrateAddsColumns opens a hand-written pre-M7 database and asserts Open
// evolves it: the four new columns appear, and pre-existing rows read back the
// empty-string default.
func TestMigrateAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// The pre-M7 requests table, verbatim (no workload columns).
	old := `
CREATE TABLE requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	agent TEXT NOT NULL DEFAULT '',
	team TEXT NOT NULL DEFAULT '',
	namespace TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	stream INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0
);`
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests (ts, provider, status, duration_ms) VALUES (?, 'anthropic', 200, 5)`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-M7 db: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var pod, kind, workload, sa string
	row := s.db.QueryRowContext(context.Background(),
		`SELECT pod, workload_kind, workload, service_account FROM requests LIMIT 1`)
	if err := row.Scan(&pod, &kind, &workload, &sa); err != nil {
		t.Fatalf("new columns not queryable after migration: %v", err)
	}
	if pod != "" || kind != "" || workload != "" || sa != "" {
		t.Errorf("migrated row = %q/%q/%q/%q, want empty defaults", pod, kind, workload, sa)
	}

	// A fresh insert with workload fields must round-trip.
	r := record("payments/x", "payments", "m", 1, 1, 0.1, time.Now())
	r.Workload = "x"
	if err := s.Insert(context.Background(), r); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}

// TestConcurrentInserts exercises the WAL/busy_timeout setup with parallel
// writers, matching how request goroutines insert in production.
func TestConcurrentInserts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := s.Insert(ctx, record("agent", "team", "m", 1, 1, 0.0001, now)); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent insert: %v", err)
	}

	rows, err := s.Aggregate(ctx, AggregateOptions{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != writers*perWriter {
		t.Errorf("rows = %+v, want %d requests", rows, writers*perWriter)
	}
}
