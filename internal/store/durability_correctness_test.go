package store

// Group A — durability at the storage layer. The regression guard for the
// write-contention bug this suite found: with a default connection pool,
// concurrent Inserts fought over SQLite's single write lock, exhausted
// busy_timeout, and returned SQLITE_BUSY. The recorder can only log that, so
// the request's spend was silently dropped — measured at ~29% loss with 200
// concurrent requests.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/meter"
)

// TestCorrectness_ConcurrentInsertsAllPersist covers the storage half of A3.
//
// INVARIANT: every Insert that returns without error is on disk, and under
// concurrency none of them returns an error.
//
// The second half matters as much as the first: an Insert that fails is spend
// that was really consumed and will never be counted, because nothing retries
// it and nothing else knows about it.
func TestCorrectness_ConcurrentInsertsAllPersist(t *testing.T) {
	const writers = 200

	st, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	var mu sync.Mutex
	var failures []error

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// The same 10s deadline cmd/tollgate's recorder uses.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := st.Insert(ctx, Record{
				Time: now, Agent: "support-bot", Team: "support", Provider: "test",
				Model: "claude-haiku-4-5", Status: 200,
				Usage:       meter.Usage{InputTokens: 100, OutputTokens: 50},
				CostUSD:     0.01,
				UsageStatus: UsageOK,
			})
			if err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Errorf("%d of %d concurrent Inserts failed (first: %v) — each one is a "+
			"request's spend lost for good", len(failures), writers, failures[0])
	}

	rows, err := st.Aggregate(context.Background(), AggregateOptions{
		GroupBy: "agent", Since: now.Add(-time.Hour), Until: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("aggregate rows = %d, want 1", len(rows))
	}
	if rows[0].Requests != writers {
		t.Errorf("persisted requests = %d, want %d", rows[0].Requests, writers)
	}
	if got, want := rows[0].InputTokens, int64(writers*100); got != want {
		t.Errorf("input tokens = %d, want %d", got, want)
	}
	if got := int64(rows[0].CostUSD*100 + 0.5); got != writers {
		t.Errorf("cost = $%.4f, want $%.2f", rows[0].CostUSD, float64(writers)/100)
	}
}

// TestCorrectness_ReadsAndWritesDoNotStarveEachOther covers the mixed workload:
// the budget engine's Spend re-sync and GET /usage aggregation run against the
// same file the request path is writing to.
//
// INVARIANT: concurrent reads and writes both complete without error.
func TestCorrectness_ReadsAndWritesDoNotStarveEachOther(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mixed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	ctx := context.Background()
	var mu sync.Mutex
	var failures []error
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		failures = append(failures, err)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			record(st.Insert(ctx, Record{
				Time: now, Agent: "support-bot", Team: "support", Provider: "test",
				Status: 200, Usage: meter.Usage{InputTokens: 10}, CostUSD: 0.01,
				UsageStatus: UsageOK,
			}))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, err := st.Spend(ctx, "agent", "support-bot", now.Add(-time.Hour))
			record(err)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := st.Aggregate(ctx, AggregateOptions{
				Since: now.Add(-time.Hour), Until: now.Add(time.Hour),
			})
			record(err)
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Errorf("%d operations failed under mixed read/write load (first: %v)",
			len(failures), failures[0])
	}
}
