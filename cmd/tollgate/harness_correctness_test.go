package main

// Shared harness for the correctness suites that need the real wiring —
// auth -> budget -> proxy -> upstream -> newRecorder -> SQLite. Nothing here is
// a test; it exists so groups A3, C and D exercise the same code path
// cmd/tollgate assembles in run(), rather than a reimplementation of it.

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/budget"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/proxy"
	"github.com/opslync/tollgate/internal/store"
	"github.com/opslync/tollgate/pricing"
)

// recordedRow mirrors one persisted requests row, including the usage_status
// column that separates a flagged $0 from a genuine one.
type recordedRow struct {
	Agent        string
	Team         string
	Namespace    string
	Model        string
	Path         string
	Status       int
	CostUSD      float64
	UsageStatus  string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	Stream       bool
}

type harness struct {
	t      *testing.T
	server *httptest.Server // Tollgate itself
	store  *store.Store
	engine *budget.Engine
	dbPath string

	// recorded receives every RequestRecord after its row has been persisted,
	// so tests can synchronise on "the recorder finished" without sleeping.
	recorded chan proxy.RequestRecord
	// gate, when non-nil, blocks the recorder before it persists anything.
	// It models the process dying between "client got the response" and
	// "row hits SQLite".
	gate chan struct{}

	logs *lockedBuffer
}

// lockedBuffer collects slog output from many request goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// count returns how many log lines contain substr.
func (b *lockedBuffer) count(substr string) int {
	return strings.Count(b.String(), substr)
}

type harnessOptions struct {
	agents       []config.Agent
	budgets      []config.Budget
	providerType string // "anthropic" (default) or "openai"
	upstream     http.Handler
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	if opts.providerType == "" {
		opts.providerType = "anthropic"
	}

	upstream := httptest.NewServer(opts.upstream)
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "correctness.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	engine := budget.New(st, opts.budgets, logger)

	h := &harness{
		t: t, store: st, engine: engine, dbPath: dbPath,
		recorded: make(chan proxy.RequestRecord, 4096),
		logs:     logs,
	}

	p := proxy.New(proxy.Options{
		Name: "test", Type: opts.providerType, Upstream: upstreamURL,
	}, logger)
	// The production recorder, wrapped only to signal completion to the test.
	real := newRecorder(st, prices, engine, nil, logger)
	p.SetRecorder(func(rec proxy.RequestRecord) {
		if h.gate != nil {
			<-h.gate
		}
		real(rec)
		h.recorded <- rec
	})

	var handler http.Handler = engine.Middleware(p)
	if len(opts.agents) > 0 {
		handler = auth.New(opts.agents, nil).Middleware(handler)
	}
	h.server = httptest.NewServer(handler)
	t.Cleanup(h.server.Close)
	return h
}

// waitForRecords blocks until n requests have been fully recorded.
func (h *harness) waitForRecords(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		<-h.recorded
	}
}

// do sends a request to Tollgate with the given agent key and returns the
// response, with the body already drained and closed.
func (h *harness) do(t *testing.T, key, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(got)
}

func stringReader(s string) io.Reader {
	if s == "" {
		return nil
	}
	return strings.NewReader(s)
}

// rows reads every persisted row straight out of the SQLite file, in insert
// order. Reading the file rather than going through an accessor keeps the
// assertion honest about what is actually on disk.
func (h *harness) rows(t *testing.T) []recordedRow {
	t.Helper()
	return rowsAt(t, h.dbPath)
}

func rowsAt(t *testing.T, dbPath string) []recordedRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rs, err := db.QueryContext(context.Background(), `
		SELECT agent, team, namespace, model, path, status, cost_usd, usage_status,
		       input_tokens, output_tokens, cache_read_input_tokens, stream
		FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	var out []recordedRow
	for rs.Next() {
		var r recordedRow
		if err := rs.Scan(&r.Agent, &r.Team, &r.Namespace, &r.Model, &r.Path,
			&r.Status, &r.CostUSD, &r.UsageStatus, &r.InputTokens, &r.OutputTokens,
			&r.CacheRead, &r.Stream); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// jsonUpstream answers every request with one fixed JSON body.
func jsonUpstream(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func agentKey(name, key, team, namespace string) config.Agent {
	return config.Agent{Name: name, Key: key, Team: team, Namespace: namespace}
}
