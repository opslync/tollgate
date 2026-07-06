package k8s

import (
	"context"
	"log/slog"

	"github.com/opslync/tollgate/internal/auth"
)

// Resolver composes TokenReview (identity), the pod cache (workload), and the
// team map (team) into an auth.TokenReviewer. It is the concrete type main.go
// injects into internal/auth, keeping that package free of a k8s import.
type Resolver struct {
	authn  *Authenticator
	cache  *PodCache
	teams  *TeamMap
	logger *slog.Logger
}

func NewResolver(authn *Authenticator, cache *PodCache, teams *TeamMap, logger *slog.Logger) *Resolver {
	return &Resolver{authn: authn, cache: cache, teams: teams, logger: logger}
}

// Review authenticates a ServiceAccount token and enriches it. Enrichment
// degrades gracefully: an authenticated identity is never rejected just because
// pod metadata is missing (unbound token, or pod not yet in the cache) — it is
// attributed by ServiceAccount instead.
func (r *Resolver) Review(ctx context.Context, token string) (auth.Agent, auth.Workload, bool) {
	id, ok := r.authn.ReviewToken(ctx, token)
	if !ok {
		return auth.Agent{}, auth.Workload{}, false
	}
	team := r.teams.Team(id.Namespace)

	if id.PodUID == "" {
		r.logger.Warn("serviceaccount token without pod binding; attributing by service account only",
			"namespace", id.Namespace, "service_account", id.ServiceAccount)
		agent, wl := r.saLevel(id, team)
		return agent, wl, true
	}

	meta, found := r.cache.Lookup(id.PodUID)
	if !found {
		r.logger.Warn("pod not found in cache; attributing by service account",
			"namespace", id.Namespace, "pod_uid", id.PodUID)
		agent, wl := r.saLevel(id, team)
		wl.Pod = id.PodName
		return agent, wl, true
	}

	agent := auth.Agent{
		Name:      meta.Namespace + "/" + meta.Workload,
		Team:      team,
		Namespace: meta.Namespace,
	}
	wl := auth.Workload{
		Pod:            meta.Pod,
		ServiceAccount: meta.ServiceAccount,
		WorkloadKind:   meta.WorkloadKind,
		Workload:       meta.Workload,
	}
	return agent, wl, true
}

// saLevel attributes an identity to its ServiceAccount when workload metadata
// is unavailable: name is <namespace>/<serviceaccount>.
func (r *Resolver) saLevel(id Identity, team string) (auth.Agent, auth.Workload) {
	return auth.Agent{
			Name:      id.Namespace + "/" + id.ServiceAccount,
			Team:      team,
			Namespace: id.Namespace,
		}, auth.Workload{
			ServiceAccount: id.ServiceAccount,
		}
}
