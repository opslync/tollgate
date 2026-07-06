package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeClient points a Client at a test server with no token auth.
func fakeClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{baseURL: srv.URL, http: srv.Client()}
}

// tokenReviewServer answers TokenReview POSTs from a token→status table.
func tokenReviewServer(t *testing.T, statuses map[string]tokenReviewStatus) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var in tokenReview
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		status, ok := statuses[in.Spec.Token]
		if !ok {
			status = tokenReviewStatus{Authenticated: false, Error: "unknown token"}
		}
		out := tokenReview{
			APIVersion: "authentication.k8s.io/v1",
			Kind:       "TokenReview",
			Status:     status,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReviewToken(t *testing.T) {
	statuses := map[string]tokenReviewStatus{
		"bound-token": {
			Authenticated: true,
			User: userInfo{
				Username: "system:serviceaccount:payments:checkout",
				Extra: map[string][]string{
					"authentication.kubernetes.io/pod-name": {"checkout-worker-abc"},
					"authentication.kubernetes.io/pod-uid":  {"uid-123"},
				},
			},
		},
		"unbound-token": {
			Authenticated: true,
			User:          userInfo{Username: "system:serviceaccount:search:indexer"},
		},
		"human-token": {
			Authenticated: true,
			User:          userInfo{Username: "alice@example.com"},
		},
		"expired-token": {Authenticated: false, Error: "token expired"},
	}
	srv := tokenReviewServer(t, statuses)
	a := NewAuthenticator(fakeClient(t, srv), nil)
	ctx := context.Background()

	t.Run("bound token yields full identity", func(t *testing.T) {
		id, ok := a.ReviewToken(ctx, "bound-token")
		if !ok {
			t.Fatal("expected authenticated")
		}
		want := Identity{
			Username:       "system:serviceaccount:payments:checkout",
			Namespace:      "payments",
			ServiceAccount: "checkout",
			PodName:        "checkout-worker-abc",
			PodUID:         "uid-123",
		}
		if id != want {
			t.Errorf("identity = %+v, want %+v", id, want)
		}
	})

	t.Run("unbound token authenticates SA without pod", func(t *testing.T) {
		id, ok := a.ReviewToken(ctx, "unbound-token")
		if !ok {
			t.Fatal("expected authenticated")
		}
		if id.Namespace != "search" || id.ServiceAccount != "indexer" {
			t.Errorf("identity = %+v", id)
		}
		if id.PodUID != "" || id.PodName != "" {
			t.Errorf("expected no pod metadata, got %+v", id)
		}
	})

	t.Run("human user rejected", func(t *testing.T) {
		if _, ok := a.ReviewToken(ctx, "human-token"); ok {
			t.Error("non-serviceaccount token must not authenticate")
		}
	})

	t.Run("expired token rejected", func(t *testing.T) {
		if _, ok := a.ReviewToken(ctx, "expired-token"); ok {
			t.Error("expired token must be rejected")
		}
	})

	t.Run("unknown token rejected", func(t *testing.T) {
		if _, ok := a.ReviewToken(ctx, "never-seen"); ok {
			t.Error("unknown token must be rejected")
		}
	})
}

func TestReviewTokenSendsAudiences(t *testing.T) {
	var gotAudiences []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in tokenReview
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotAudiences = in.Spec.Audiences
		_ = json.NewEncoder(w).Encode(tokenReview{
			Status: tokenReviewStatus{Authenticated: true,
				User: userInfo{Username: "system:serviceaccount:x:y"}},
		})
	}))
	t.Cleanup(srv.Close)

	a := NewAuthenticator(fakeClient(t, srv), []string{"https://kubernetes.default.svc"})
	if _, ok := a.ReviewToken(context.Background(), "t"); !ok {
		t.Fatal("expected authenticated")
	}
	if len(gotAudiences) != 1 || gotAudiences[0] != "https://kubernetes.default.svc" {
		t.Errorf("audiences = %v, want configured allowlist", gotAudiences)
	}
}

func TestParseServiceAccountUsername(t *testing.T) {
	tests := []struct {
		in       string
		ns, name string
		ok       bool
	}{
		{"system:serviceaccount:payments:checkout", "payments", "checkout", true},
		{"system:serviceaccount:default:default", "default", "default", true},
		{"alice@example.com", "", "", false},
		{"system:serviceaccount:onlyns", "", "", false},
		{"system:serviceaccount::checkout", "", "", false},
		{"system:serviceaccount:payments:", "", "", false},
	}
	for _, tt := range tests {
		ns, name, ok := parseServiceAccountUsername(tt.in)
		if ns != tt.ns || name != tt.name || ok != tt.ok {
			t.Errorf("parse(%q) = %q,%q,%v want %q,%q,%v", tt.in, ns, name, ok, tt.ns, tt.name, tt.ok)
		}
	}
}
