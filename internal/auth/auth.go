// Package auth authenticates incoming requests by their Tollgate agent key
// and attaches the agent's identity to the request context for attribution.
//
// Agents present their key in the header their SDK already uses — `x-api-key`
// (Anthropic style) or `Authorization: Bearer` (OpenAI style) — so pointing
// an existing agent at Tollgate requires only a base-URL and key change.
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/opslync/tollgate/internal/config"
)

// Agent is the authenticated identity attached to a request.
type Agent struct {
	Name      string
	Team      string
	Namespace string
}

type ctxKey struct{}

// FromContext returns the agent authenticated for this request, if any.
func FromContext(ctx context.Context) (Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(Agent)
	return a, ok
}

type Authenticator struct {
	byKey map[string]Agent
}

func New(agents []config.Agent) *Authenticator {
	byKey := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byKey[a.Key] = Agent{Name: a.Name, Team: a.Team, Namespace: a.Namespace}
	}
	return &Authenticator{byKey: byKey}
}

// Middleware rejects requests without a known agent key and stores the
// matched agent in the request context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, ok := a.byKey[extractKey(r)]
		if !ok {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, agent)))
	})
}

func extractKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return bearer
	}
	return ""
}

// unauthorized answers in the Anthropic error shape so provider SDKs surface
// the message instead of choking on an unfamiliar body.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	//nolint:errcheck // best-effort error body
	w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid or missing Tollgate agent key; pass your agent key in x-api-key or Authorization: Bearer"}}`))
}
