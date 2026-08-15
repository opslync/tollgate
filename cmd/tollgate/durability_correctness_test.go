package main

// Group A3 — hard-kill durability of the recorder path. Groups A1/A2/A4 live in
// internal/budget; this one needs the real proxy-to-SQLite path, which is
// assembled here in cmd/tollgate.

import (
	"net/http"
	"sync"
	"testing"

	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

const anthropicJSONBody = `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output_tokens":500}}`

// TestCorrectness_HardKillPersistsAcceptedSpend covers A3.
//
// INVARIANT: spend_events_accepted == spend_events_persisted.
//
// Tollgate has no queue, buffer, or background flusher between the recorder and
// SQLite: store.Insert is called synchronously inside the recorder callback, on
// the request goroutine, and the handler does not return until it completes.
// So there is nothing to drain and nothing to lose in a batch — the first
// sub-test asserts that.
//
// It is NOT trivially perfect, though, and the second sub-test pins down
// exactly why. The recorder runs AFTER proxy.ServeHTTP has finished writing the
// response to the client (see internal/proxy's ServeHTTP). Between "the client
// holds a complete, successful response" and "the row is in SQLite" there is a
// real window. A hard kill inside it loses that request's spend permanently —
// the agent got its tokens, and Tollgate never counted them.
//
// The bound: at most one lost record per request in flight at the moment of the
// kill, never more, and never for a request the client did not receive.
func TestCorrectness_HardKillPersistsAcceptedSpend(t *testing.T) {
	agents := []config.Agent{agentKey("support-bot", "k-support-bot-000001", "support", "prod")}

	t.Run("every accepted request is persisted synchronously", func(t *testing.T) {
		const requests = 200
		h := newHarness(t, harnessOptions{
			agents:   agents,
			upstream: jsonUpstream(anthropicJSONBody),
		})

		var mu sync.Mutex
		accepted := 0

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < requests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, _ := h.do(t, "k-support-bot-000001", "/v1/messages", `{"model":"claude-haiku-4-5"}`, nil)
				if resp.StatusCode == http.StatusOK {
					mu.Lock()
					accepted++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		h.waitForRecords(requests)

		// Terminate the storage layer without any drain step — there is no
		// drain step to call, which is the point.
		if err := h.store.Close(); err != nil {
			t.Fatal(err)
		}

		persisted := rowsAt(t, h.dbPath)
		if accepted != requests {
			t.Fatalf("accepted = %d, want %d", accepted, requests)
		}
		if len(persisted) != accepted {
			t.Errorf("spend_events_persisted = %d, spend_events_accepted = %d; "+
				"persist failures logged = %d",
				len(persisted), accepted, h.logs.count("failed to persist usage record"))
		}
		for i, r := range persisted {
			if r.UsageStatus != store.UsageOK || r.InputTokens != 1000 || r.OutputTokens != 500 {
				t.Fatalf("row %d = %+v, want a fully metered row", i, r)
			}
		}
	})

	// The honest part: prove the window between "client served" and "row
	// persisted" exists, deterministically, by holding the recorder shut.
	t.Run("the loss window: client is served before the row is persisted", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			agents:   agents,
			upstream: jsonUpstream(anthropicJSONBody),
		})
		h.gate = make(chan struct{}) // recorder blocks until this is closed

		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, body := h.do(t, "k-support-bot-000001", "/v1/messages", `{"model":"claude-haiku-4-5"}`, nil)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if body != anthropicJSONBody {
				t.Errorf("body = %q, want the upstream response verbatim", body)
			}
		}()

		// The client has its complete, successful response...
		<-done
		// ...and nothing has been persisted. A SIGKILL here loses this request's
		// spend, and the agent still got its tokens.
		if rows := h.rows(t); len(rows) != 0 {
			t.Fatalf("rows persisted while the recorder was gated = %d, want 0 "+
				"(if this changed, the recorder now runs before the response completes)", len(rows))
		}

		close(h.gate)
		h.waitForRecords(1)
		if rows := h.rows(t); len(rows) != 1 {
			t.Errorf("rows after the recorder ran = %d, want 1", len(rows))
		}
	})
}
