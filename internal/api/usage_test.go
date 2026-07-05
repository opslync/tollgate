package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/internal/store"
)

func seededStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now()
	ctx := context.Background()
	records := []store.Record{
		{Time: now, Agent: "support-bot", Team: "support", Provider: "anthropic", Model: "claude-sonnet-5",
			Status: 200, Usage: meter.Usage{InputTokens: 100, OutputTokens: 50}, CostUSD: 0.00105},
		{Time: now, Agent: "research-bot", Team: "research", Provider: "anthropic", Model: "claude-opus-4-8",
			Status: 200, Usage: meter.Usage{InputTokens: 1000, OutputTokens: 500}, CostUSD: 0.0175},
		{Time: now.Add(-72 * time.Hour), Agent: "support-bot", Team: "support", Provider: "anthropic",
			Model: "claude-sonnet-5", Status: 200, Usage: meter.Usage{InputTokens: 9999}, CostUSD: 1},
	}
	for _, r := range records {
		if err := st.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func getUsage(t *testing.T, handler http.Handler, query string) (int, usageResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/usage"+query, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var resp usageResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

func TestUsageDefaults(t *testing.T) {
	handler := UsageHandler(seededStore(t))
	code, resp := getUsage(t, handler, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.GroupBy != "agent" || len(resp.Rows) != 2 {
		t.Fatalf("resp = %+v (72h-old record must be outside default 24h window)", resp)
	}
	if resp.Rows[0].Key != "research-bot" {
		t.Errorf("rows not sorted by cost desc: %+v", resp.Rows)
	}
}

func TestUsageGroupByModelAndWindow(t *testing.T) {
	handler := UsageHandler(seededStore(t))
	code, resp := getUsage(t, handler, "?group_by=model&since=96h")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("rows = %+v, want sonnet+opus with 96h window", resp.Rows)
	}
	for _, row := range resp.Rows {
		if row.Key == "claude-sonnet-5" && row.Requests != 2 {
			t.Errorf("sonnet requests = %d, want 2 (old record inside 96h)", row.Requests)
		}
	}
}

func TestUsageAgentFilter(t *testing.T) {
	handler := UsageHandler(seededStore(t))
	code, resp := getUsage(t, handler, "?agent=support-bot")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].Key != "support-bot" || resp.Rows[0].InputTokens != 100 {
		t.Errorf("rows = %+v", resp.Rows)
	}
}

func TestUsageBadParams(t *testing.T) {
	handler := UsageHandler(seededStore(t))
	for _, query := range []string{"?group_by=password", "?since=whenever", "?until=later"} {
		if code, _ := getUsage(t, handler, query); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, code)
		}
	}
}
