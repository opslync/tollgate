package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/budget"
	"github.com/opslync/tollgate/internal/store"
)

const testAdminKey = "admin-secret-0123456789"

func adminServer(t *testing.T) (*httptest.Server, *budget.Engine) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := budget.New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(Admin(e, testAdminKey, []string{"support-bot", "research-bot"}))
	t.Cleanup(srv.Close)
	return srv, e
}

func adminReq(t *testing.T, method, url, key string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func TestAdminAuthRequired(t *testing.T) {
	srv, _ := adminServer(t)
	for _, key := range []string{"", "wrong-key"} {
		resp, _ := adminReq(t, http.MethodGet, srv.URL+"/admin/kills", key)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
	}
}

func TestAdminKillReviveList(t *testing.T) {
	srv, e := adminServer(t)

	resp, body := adminReq(t, http.MethodPost, srv.URL+"/admin/agents/support-bot/kill", testAdminKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kill status = %d: %s", resp.StatusCode, body)
	}
	if d := e.Check(context.Background(), auth.Agent{Name: "support-bot"}); d.Kind != budget.BlockedKilled {
		t.Fatalf("engine decision = %v, want BlockedKilled", d.Kind)
	}

	_, body = adminReq(t, http.MethodGet, srv.URL+"/admin/kills", testAdminKey)
	var kills map[string][]string
	json.Unmarshal(body, &kills)
	if len(kills["killed"]) != 1 || kills["killed"][0] != "support-bot" {
		t.Errorf("kills = %s", body)
	}

	resp, _ = adminReq(t, http.MethodDelete, srv.URL+"/admin/agents/support-bot/kill", testAdminKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revive status = %d", resp.StatusCode)
	}
	if d := e.Check(context.Background(), auth.Agent{Name: "support-bot"}); d.Kind != budget.Allow {
		t.Errorf("engine decision after revive = %v, want Allow", d.Kind)
	}
}

func TestAdminKillUnknownAgent(t *testing.T) {
	srv, _ := adminServer(t)
	resp, _ := adminReq(t, http.MethodPost, srv.URL+"/admin/agents/typo-bot/kill", testAdminKey)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (typo protection)", resp.StatusCode)
	}
}
