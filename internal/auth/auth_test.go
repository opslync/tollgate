package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opslync/tollgate/internal/config"
)

// fakeReviewer is a TokenReviewer double: it records the token it was handed
// and returns a canned result.
type fakeReviewer struct {
	agent    Agent
	workload Workload
	ok       bool
	called   bool
	gotToken string
}

func (f *fakeReviewer) Review(_ context.Context, token string) (Agent, Workload, bool) {
	f.called = true
	f.gotToken = token
	return f.agent, f.workload, f.ok
}

// A syntactically JWT-shaped token (three dot-separated segments).
const jwtToken = "header.payload.signature"

const testKey = "tg_test_0123456789abcdef"

func testHandler(t *testing.T) (http.Handler, *Agent) {
	t.Helper()
	seen := &Agent{}
	authn := New([]config.Agent{
		{Name: "support-bot", Key: testKey, Team: "support", Namespace: "prod"},
	}, nil)
	return authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := FromContext(r.Context()); ok {
			*seen = a
		}
		w.WriteHeader(http.StatusOK)
	})), seen
}

func TestValidKey(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{"x-api-key", "x-api-key", testKey},
		{"bearer", "Authorization", "Bearer " + testKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, seen := testHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.Header.Set(tt.header, tt.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			want := Agent{Name: "support-bot", Team: "support", Namespace: "prod"}
			if *seen != want {
				t.Errorf("agent in context = %+v, want %+v", *seen, want)
			}
		})
	}
}

func TestRejected(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*http.Request)
	}{
		{"missing key", func(r *http.Request) {}},
		{"unknown key", func(r *http.Request) { r.Header.Set("x-api-key", "tg_wrong_0123456789abcdef") }},
		{"malformed bearer", func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") }},
		{"empty x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, seen := testHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			tt.setup(req)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if seen.Name != "" {
				t.Error("handler ran despite rejection")
			}
			body, _ := io.ReadAll(rec.Body)
			var errBody struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &errBody); err != nil {
				t.Fatalf("401 body is not JSON: %v (%s)", err, body)
			}
			if errBody.Type != "error" || errBody.Error.Type != "authentication_error" {
				t.Errorf("401 body = %s, want anthropic error shape", body)
			}
		})
	}
}

// reviewerHandler wires the middleware with both a static key and a reviewer,
// capturing the agent and workload seen downstream.
func reviewerHandler(t *testing.T, rev TokenReviewer) (http.Handler, *Agent, *Workload) {
	t.Helper()
	agent, wl := &Agent{}, &Workload{}
	authn := New([]config.Agent{{Name: "support-bot", Key: testKey, Team: "support", Namespace: "prod"}}, rev)
	return authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := FromContext(r.Context()); ok {
			*agent = a
		}
		if wm, ok := WorkloadFromContext(r.Context()); ok {
			*wl = wm
		}
		w.WriteHeader(http.StatusOK)
	})), agent, wl
}

func TestStaticKeyWinsOverReviewer(t *testing.T) {
	rev := &fakeReviewer{ok: true, agent: Agent{Name: "should-not-be-used"}}
	handler, agent, _ := reviewerHandler(t, rev)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", testKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if agent.Name != "support-bot" {
		t.Errorf("agent = %q, want static support-bot", agent.Name)
	}
	if rev.called {
		t.Error("reviewer must not be called when a static key matches")
	}
}

func TestReviewedTokenAttributed(t *testing.T) {
	rev := &fakeReviewer{
		ok:       true,
		agent:    Agent{Name: "payments/checkout-worker", Team: "payments", Namespace: "payments"},
		workload: Workload{Pod: "checkout-worker-abc", ServiceAccount: "checkout", WorkloadKind: "Deployment", Workload: "checkout-worker"},
	}
	handler, agent, wl := reviewerHandler(t, rev)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rev.gotToken != jwtToken {
		t.Errorf("reviewer saw token %q, want %q", rev.gotToken, jwtToken)
	}
	want := Agent{Name: "payments/checkout-worker", Team: "payments", Namespace: "payments"}
	if *agent != want {
		t.Errorf("agent = %+v, want %+v", *agent, want)
	}
	wantWL := Workload{Pod: "checkout-worker-abc", ServiceAccount: "checkout", WorkloadKind: "Deployment", Workload: "checkout-worker"}
	if *wl != wantWL {
		t.Errorf("workload = %+v, want %+v", *wl, wantWL)
	}
}

func TestReviewerRejects401(t *testing.T) {
	rev := &fakeReviewer{ok: false}
	handler, agent, _ := reviewerHandler(t, rev)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if agent.Name != "" {
		t.Error("handler ran despite reviewer rejection")
	}
}

// A JWT-shaped credential with no reviewer configured must still 401 — proving
// zero regression when kubernetes.enabled=false.
func TestNoReviewerJWTRejected(t *testing.T) {
	handler, agent := testHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if agent.Name != "" {
		t.Error("handler ran without authentication")
	}
}

// A non-JWT credential must not trigger a TokenReview round-trip.
func TestNonJWTSkipsReviewer(t *testing.T) {
	rev := &fakeReviewer{ok: true, agent: Agent{Name: "x"}}
	handler, _, _ := reviewerHandler(t, rev)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "tg_not_a_jwt_but_wrong_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rev.called {
		t.Error("reviewer called for a non-JWT credential")
	}
}
