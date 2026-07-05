package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opslync/tollgate/internal/config"
)

const testKey = "tg_test_0123456789abcdef"

func testHandler(t *testing.T) (http.Handler, *Agent) {
	t.Helper()
	seen := &Agent{}
	authn := New([]config.Agent{
		{Name: "support-bot", Key: testKey, Team: "support", Namespace: "prod"},
	})
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
