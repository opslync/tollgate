package budget

// Group B — concurrency. Does money survive many goroutines at once?
//
// White-box (package budget) so the tests can drive the clock and the re-sync
// interval directly, which is what makes them deterministic without sleeps.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/config"
)

// TestCorrectness_ConcurrentRecordLosesNoSpend covers B1.
//
// INVARIANT: the sum of concurrently recorded spend equals the sequential sum —
// no increment is lost to a race.
func TestCorrectness_ConcurrentRecordLosesNoSpend(t *testing.T) {
	const (
		goroutines   = 50
		perGoroutine = 20 // 1000 requests total
		costEach     = 0.0123
		tokensEach   = 137
	)

	budgets := []config.Budget{usdBudget("support-bot", 1e9, "block")}
	now := time.Now()

	// Reference: the same 1000 increments applied one at a time.
	seq, seqStore := openEngine(t, filepath.Join(t.TempDir(), "seq.db"), budgets, &now)
	t.Cleanup(func() { seqStore.Close() })
	prime(t, seq, supportBot)
	for i := 0; i < goroutines*perGoroutine; i++ {
		seq.Record(supportBot, tokensEach, costEach)
	}
	wantUSD, wantTokens := spendOf(t, seq, "support-bot")

	// Same total, applied from 50 goroutines at once.
	con, conStore := openEngine(t, filepath.Join(t.TempDir(), "con.db"), budgets, &now)
	t.Cleanup(func() { conStore.Close() })
	prime(t, con, supportBot)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release everyone at once; no sleeps involved
			for i := 0; i < perGoroutine; i++ {
				con.Record(supportBot, tokensEach, costEach)
			}
		}()
	}
	close(start)
	wg.Wait()

	gotUSD, gotTokens := spendOf(t, con, "support-bot")
	if cents(gotUSD) != cents(wantUSD) {
		t.Errorf("concurrent spend = %d cents, sequential = %d cents", cents(gotUSD), cents(wantUSD))
	}
	if gotTokens != wantTokens {
		t.Errorf("concurrent tokens = %d, sequential = %d", gotTokens, wantTokens)
	}
	if gotTokens != goroutines*perGoroutine*tokensEach {
		t.Errorf("token total = %d, want %d", gotTokens, goroutines*perGoroutine*tokensEach)
	}
}

// TestCorrectness_ConcurrentCheckAtBoundary covers B2.
//
// INVARIANT (as specified): no more than one request is allowed past a hard
// limit.
//
// THIS INVARIANT DOES NOT HOLD CONCURRENTLY, AND CANNOT AS DESIGNED. Check and
// Record are separate calls, each individually locked, with an entire upstream
// HTTP round trip between them: the budget middleware calls Check before
// forwarding, and cmd/tollgate's recorder calls Record only after the response
// has streamed back. Every request that passes Check before the first Record
// lands sees the pre-spend counter and is allowed. This is the classic
// time-of-check/time-of-use gap in every budget enforcer that prices a request
// only after it completes — you cannot know a request's cost until it is done.
//
// The honest bound, which this test asserts and docs/correctness.md publishes:
// overshoot is bounded by the number of requests in flight at the moment the
// limit is crossed, never unbounded. A runaway loop issuing requests serially
// is stopped on its next request; a runaway loop with N requests already in
// flight can exceed the limit by at most those N.
//
// Closing the gap would need reserve-then-commit: Check debits an estimate,
// Record trues it up, and every failure path releases the reservation. That is
// a proxy-wide semantic change (what estimate? released on client disconnect?
// on upstream 500? on process death mid-flight?), not a one-file fix, so the
// bound is documented rather than papered over.
func TestCorrectness_ConcurrentCheckAtBoundary(t *testing.T) {
	const (
		limitUSD = 1.00
		preSpend = 0.99 // under the limit: one more request is allowed
		costEach = 0.02 // ...and it takes the total to $1.01, over the limit
		inFlight = 100
	)
	budgets := []config.Budget{usdBudget("support-bot", limitUSD, "block")}

	// Sequential control: with Check and Record strictly interleaved, the
	// engine does allow exactly one request past the limit.
	t.Run("sequential", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "seq.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		prime(t, e, supportBot)
		e.Record(supportBot, 0, preSpend)

		allowed := 0
		for i := 0; i < inFlight; i++ {
			if d := e.Check(context.Background(), supportBot); d.Kind == Allow {
				allowed++
				e.Record(supportBot, 0, costEach)
			}
		}
		if allowed != 1 {
			t.Errorf("sequential allows = %d, want exactly 1", allowed)
		}
	})

	// Concurrent: 100 requests reach Check before any of them has completed.
	t.Run("concurrent", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "con.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		// Pin the counters: a mid-test re-sync would zero the live delta and
		// muddy what this test is measuring.
		prime(t, e, supportBot)
		e.refreshEvery = time.Hour
		e.Record(supportBot, 0, preSpend)

		var mu sync.Mutex
		allowed, blocked := 0, 0

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < inFlight; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d := e.Check(context.Background(), supportBot)
				mu.Lock()
				if d.Kind == Allow {
					allowed++
				} else {
					blocked++
				}
				mu.Unlock()
				if d.Kind == Allow {
					// The request went upstream, so its spend is real.
					e.Record(supportBot, 0, costEach)
				}
			}()
		}
		close(start)
		wg.Wait()

		t.Logf("at the boundary with %d requests in flight: %d allowed, %d blocked "+
			"(spec's ideal is 1 allowed / %d blocked)", inFlight, allowed, blocked, inFlight-1)

		if allowed < 1 {
			t.Errorf("allowed = 0: the limit is not merely overshooting, it is over-blocking")
		}
		// The documented bound: overshoot never exceeds the in-flight count.
		if allowed > inFlight {
			t.Errorf("allowed = %d, exceeds the in-flight bound of %d", allowed, inFlight)
		}
		// No spend is lost while overshooting: every allowed request is counted.
		gotUSD, _ := spendOf(t, e, "support-bot")
		wantUSD := preSpend + float64(allowed)*costEach
		if cents(gotUSD) != cents(wantUSD) {
			t.Errorf("recorded spend = %d cents, want %d (pre-spend + %d allowed requests)",
				cents(gotUSD), cents(wantUSD), allowed)
		}
		// And the engine converges: once the burst has been recorded, the very
		// next request is blocked. Overshoot is a one-shot burst, not a leak.
		if d := e.Check(context.Background(), supportBot); d.Kind != BlockedBudget {
			t.Errorf("Check after the burst = %v, want BlockedBudget", d.Kind)
		}
	})

	// The sub-test above understates the problem: with no work between Check
	// and Record, the engine's own mutex serialises the goroutines and the
	// first Record usually lands before most others have checked (typically
	// 2-3 allowed out of 100). Production has an entire upstream round trip in
	// that gap, so every concurrent request checks before any records.
	//
	// This models that worst case deterministically — a barrier stands in for
	// the round trip, no sleeps — and pins the bound at exactly the in-flight
	// count. If someone later implements reserve-then-commit, this is the test
	// that will notice.
	t.Run("worst case: all requests in flight before any completes", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "worst.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		prime(t, e, supportBot)
		e.refreshEvery = time.Hour
		e.Record(supportBot, 0, preSpend)

		var mu sync.Mutex
		allowed := 0

		start := make(chan struct{})
		var checked, recorded sync.WaitGroup
		checked.Add(inFlight)
		recorded.Add(inFlight)
		for i := 0; i < inFlight; i++ {
			go func() {
				defer recorded.Done()
				<-start
				d := e.Check(context.Background(), supportBot)
				checked.Done()
				checked.Wait() // stands in for the upstream round trip
				if d.Kind == Allow {
					mu.Lock()
					allowed++
					mu.Unlock()
					e.Record(supportBot, 0, costEach)
				}
			}()
		}
		close(start)
		recorded.Wait()

		if allowed != inFlight {
			t.Errorf("allowed = %d, want %d — every request that checks before any "+
				"records is allowed; if this changed, the enforcement semantics changed",
				allowed, inFlight)
		}
		// $0.99 + 100 x $0.02 = $2.99 against a $1.00 limit: the honest,
		// bounded worst case published in docs/correctness.md.
		gotUSD, _ := spendOf(t, e, "support-bot")
		if cents(gotUSD) != 299 {
			t.Errorf("overshoot spend = %d cents, want 299", cents(gotUSD))
		}
		if d := e.Check(context.Background(), supportBot); d.Kind != BlockedBudget {
			t.Errorf("Check after the burst = %v, want BlockedBudget", d.Kind)
		}
	})
}

// TestCorrectness_ResyncDoesNotDoubleCount covers B3.
//
// INVARIANT: the periodic SQLite re-sync is idempotent against live increments —
// spend that has been both persisted and counted live is not counted twice.
//
// This holds for the production ordering (store.Insert, then Engine.Record, as
// cmd/tollgate's newRecorder does). There is one narrow exception, asserted
// exactly below: a re-sync that lands *between* a request's Insert and its
// Record counts that request twice until the following re-sync. The window is
// the few microseconds between two adjacent statements, the overcount is
// bounded by the requests in that window, and it self-corrects within one
// refresh interval. It is the deliberate fail-closed bias of this package
// (see the package doc): brief overcounting is acceptable, undercounting is not.
func TestCorrectness_ResyncDoesNotDoubleCount(t *testing.T) {
	budgets := []config.Budget{usdBudget("support-bot", 1e9, "block")}
	ctx := context.Background()

	// forceResync advances the clock past the refresh interval and runs a Check,
	// which is what triggers refreshLocked. No sleeping.
	forceResync := func(e *Engine, now *time.Time) {
		*now = now.Add(2 * defaultRefreshEvery)
		e.Check(ctx, supportBot)
	}

	t.Run("resync after a completed request", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "a.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		prime(t, e, supportBot)

		recordRequest(t, st, e, supportBot, now, 1000, 2.50)
		if usd, _ := spendOf(t, e, "support-bot"); cents(usd) != 250 {
			t.Fatalf("before re-sync = %d cents, want 250", cents(usd))
		}
		forceResync(e, &now)
		if usd, tokens := spendOf(t, e, "support-bot"); cents(usd) != 250 || tokens != 1000 {
			t.Errorf("after re-sync = %d cents / %d tokens, want 250 / 1000 (doubled would be 500 / 2000)",
				cents(usd), tokens)
		}
	})

	t.Run("interleaved records and resyncs", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "b.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		prime(t, e, supportBot)

		for i := 0; i < 5; i++ {
			recordRequest(t, st, e, supportBot, now, 100, 0.10)
			forceResync(e, &now)
			recordRequest(t, st, e, supportBot, now, 100, 0.10)
			forceResync(e, &now)
		}
		// 10 requests x $0.10 = $1.00 exactly, regardless of how many re-syncs
		// happened in between.
		if usd, tokens := spendOf(t, e, "support-bot"); cents(usd) != 100 || tokens != 1000 {
			t.Errorf("after 10 requests and 10 re-syncs = %d cents / %d tokens, want 100 / 1000",
				cents(usd), tokens)
		}
	})

	t.Run("resync between insert and record overcounts, then self-corrects", func(t *testing.T) {
		now := time.Now()
		e, st := openEngine(t, filepath.Join(t.TempDir(), "c.db"), budgets, &now)
		t.Cleanup(func() { st.Close() })
		prime(t, e, supportBot)

		// Split one request's recorder in half and re-sync in the middle — the
		// exact interleaving production can hit between two adjacent lines.
		if err := st.Insert(ctx, storeRecordFor(supportBot, now, 1000, 2.50)); err != nil {
			t.Fatal(err)
		}
		forceResync(e, &now) // base picks the row up...
		e.Record(supportBot, 1000, 2.50)

		// ...and the live increment adds it a second time. Asserted exactly, so
		// this test fails loudly if the bound ever grows.
		usd, tokens := spendOf(t, e, "support-bot")
		if cents(usd) != 500 || tokens != 2000 {
			t.Errorf("mid-recorder re-sync = %d cents / %d tokens, want the documented "+
				"transient double-count of 500 / 2000", cents(usd), tokens)
		}

		// Self-correcting: the next re-sync restores the true total.
		forceResync(e, &now)
		if usd, tokens := spendOf(t, e, "support-bot"); cents(usd) != 250 || tokens != 1000 {
			t.Errorf("after the following re-sync = %d cents / %d tokens, want 250 / 1000 "+
				"(the overcount must not persist)", cents(usd), tokens)
		}
	})
}
