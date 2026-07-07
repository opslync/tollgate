package trace

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/proxy"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRecord(agent string) proxy.RequestRecord {
	return proxy.RequestRecord{
		Time:   time.Now(),
		Agent:  auth.Agent{Name: agent},
		Method: "POST", Path: "/v1/messages", Status: 200,
	}
}

func TestExportSendsBatch(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go exp.Run(ctx)

	exp.Export(testRecord("agent-1"), 0.01)
	exp.Export(testRecord("agent-2"), 0.02)

	// The interval ticker (batchInterval) triggers the flush; give it room
	// to fire without hardcoding an exact timing assumption.
	deadline := time.Now().Add(batchInterval + time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("got %d POSTs, want 1 (one interval-triggered flush)", len(bodies))
	}
	var req exportRequest
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}
	spans := req.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("got %d spans in batch, want 2", len(spans))
	}
}

func TestExportNonBlockingWhenServerSlow(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	exp := NewExporter(srv.URL, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go exp.Run(ctx)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			exp.Export(testRecord("agent-1"), 0)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Export blocked on a slow collector")
	}
}

func TestExportDropsWhenBufferFull(t *testing.T) {
	// No Run loop draining the channel: every send after the buffer fills
	// must return immediately rather than block.
	exp := NewExporter("http://127.0.0.1:0", testLogger())
	for i := 0; i < bufferCapacity; i++ {
		exp.jobs <- spanJob{rec: testRecord("agent-1")}
	}

	done := make(chan struct{})
	go func() {
		exp.Export(testRecord("overflow"), 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Export blocked on a full buffer instead of dropping")
	}
}

func TestExportBatchesByCount(t *testing.T) {
	var posts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&posts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go exp.Run(ctx)

	for i := 0; i < batchMaxSpans; i++ {
		exp.Export(testRecord("agent-1"), 0)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&posts) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&posts); got != 1 {
		t.Fatalf("got %d POSTs after reaching batchMaxSpans, want 1 (count-triggered flush, before the interval ticks)", got)
	}
}
