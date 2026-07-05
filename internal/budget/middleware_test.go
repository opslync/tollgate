package budget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
)

const middlewareTestKey = "tg_support_0123456789abcdef"

func middlewareServer(t *testing.T, e *Engine) *httptest.Server {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "forwarded")
	})
	authn := auth.New([]config.Agent{{Name: "support-bot", Key: middlewareTestKey, Team: "support"}})
	srv := httptest.NewServer(authn.Middleware(e.Middleware(next)))
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, srv *httptest.Server) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", nil)
	req.Header.Set("x-api-key", middlewareTestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed map[string]any
	json.Unmarshal(body, &parsed)
	return resp, parsed
}

func errType(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

func TestMiddlewareAllows(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	resp, _ := doReq(t, middlewareServer(t, e))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMiddlewareBlocks(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 2.0)

	resp, body := doReq(t, middlewareServer(t, e))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if errType(body) != "budget_exceeded" {
		t.Errorf("error type = %q, body = %v", errType(body), body)
	}
}

func TestMiddlewareBlockMessageKeepsSmallLimitsExact(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 0.005, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 0.01)

	_, body := doReq(t, middlewareServer(t, e))
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "$0.005 limit") {
		t.Errorf("message must show the exact limit, got: %s", msg)
	}
}

func TestMiddlewareThrottles(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 1.0, "throttle")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 2.0)
	srv := middlewareServer(t, e)

	// Trickle request goes through.
	if resp, _ := doReq(t, srv); resp.StatusCode != http.StatusOK {
		t.Fatalf("trickle status = %d, want 200", resp.StatusCode)
	}
	// Next is throttled with Retry-After.
	resp, body := doReq(t, srv)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if errType(body) != "rate_limit_error" {
		t.Errorf("error type = %q", errType(body))
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("Retry-After header missing")
	}
}

func TestMiddlewareKilled(t *testing.T) {
	e, _, _ := newTestEngine(t, nil)
	if err := e.Kill(context.Background(), "support-bot"); err != nil {
		t.Fatal(err)
	}
	resp, body := doReq(t, middlewareServer(t, e))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if errType(body) != "agent_disabled" {
		t.Errorf("error type = %q", errType(body))
	}
}

func TestMiddlewareOpenModePassesThrough(t *testing.T) {
	e, _, _ := newTestEngine(t, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	srv := httptest.NewServer(e.Middleware(next)) // no auth middleware → no agent in context
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
