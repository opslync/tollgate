package store

import (
	"context"
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
