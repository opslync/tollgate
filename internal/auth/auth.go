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

// Workload is pod/workload enrichment attached to requests authenticated by a
// Kubernetes ServiceAccount token. Static-key requests never carry it.
type Workload struct {
	Pod            string
	ServiceAccount string
	WorkloadKind   string
	Workload       string
}

// TokenReviewer resolves a ServiceAccount token to an agent identity plus
// workload enrichment. It is declared here (not imported from internal/k8s) so
// this package stays free of the k8s dependency; main.go injects the concrete
// implementation, breaking what would otherwise be an import cycle.
type TokenReviewer interface {
	Review(ctx context.Context, token string) (Agent, Workload, bool)
}

type ctxKey struct{}

type workloadCtxKey struct{}

// FromContext returns the agent authenticated for this request, if any.
func FromContext(ctx context.Context) (Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(Agent)
	return a, ok
}

// WithWorkload attaches workload enrichment to a request context.
func WithWorkload(ctx context.Context, w Workload) context.Context {
	return context.WithValue(ctx, workloadCtxKey{}, w)
}

// WorkloadFromContext returns the workload enrichment for this request, if any.
func WorkloadFromContext(ctx context.Context) (Workload, bool) {
	w, ok := ctx.Value(workloadCtxKey{}).(Workload)
	return w, ok
}

type Authenticator struct {
	byKey    map[string]Agent
	reviewer TokenReviewer // nil when kubernetes.enabled is false
}

// New builds an authenticator over the static agent keys. reviewer, when
// non-nil, authenticates ServiceAccount tokens that miss the static map.
func New(agents []config.Agent, reviewer TokenReviewer) *Authenticator {
	byKey := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byKey[a.Key] = Agent{Name: a.Name, Team: a.Team, Namespace: a.Namespace}
	}
	return &Authenticator{byKey: byKey, reviewer: reviewer}
}

// Middleware authenticates each request. Static agent keys are tried first
// (zero change for existing configs); on a miss, a JWT-shaped credential is
// handed to the TokenReviewer when one is configured. Anything unresolved is
// rejected exactly as before.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := extractKey(r)
		if agent, ok := a.byKey[key]; ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, agent)))
			return
		}
		if a.reviewer != nil && looksLikeJWT(key) {
			if agent, wl, ok := a.reviewer.Review(r.Context(), key); ok {
				ctx := context.WithValue(r.Context(), ctxKey{}, agent)
				ctx = WithWorkload(ctx, wl)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		unauthorized(w)
	})
}

// looksLikeJWT is a cheap sniff (three dot-separated segments) so a mistyped
// static key doesn't trigger a TokenReview round-trip on every request.
func looksLikeJWT(s string) bool {
	return s != "" && strings.Count(s, ".") == 2
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
