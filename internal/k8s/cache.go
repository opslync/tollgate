package k8s

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// PodMeta is the enrichment we attach to a request once its pod is identified.
type PodMeta struct {
	Namespace      string
	Pod            string
	ServiceAccount string
	WorkloadKind   string // Deployment, StatefulSet, DaemonSet, Job, ReplicaSet, Pod
	Workload       string // owning controller name (Deployment/StatefulSet/...)
}

// PodCache holds pod metadata keyed by pod UID, refreshed by a background poll.
// Staleness only affects enrichment fields — never auth or budget correctness —
// so a plain periodic list (no informers/watches) is sufficient.
type PodCache struct {
	client   *Client
	interval time.Duration

	mu    sync.RWMutex
	byUID map[string]PodMeta
}

func NewPodCache(client *Client, interval time.Duration) *PodCache {
	return &PodCache{client: client, interval: interval, byUID: map[string]PodMeta{}}
}

// Lookup returns the cached metadata for a pod UID (as reported by TokenReview).
func (c *PodCache) Lookup(podUID string) (PodMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.byUID[podUID]
	return m, ok
}

// Run refreshes once immediately (so the cache is warm) then every interval
// until ctx is cancelled.
func (c *PodCache) Run(ctx context.Context, logger *slog.Logger) {
	if err := c.refresh(ctx); err != nil {
		logger.Warn("pod cache initial refresh failed", "error", err)
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.refresh(ctx); err != nil {
				logger.Warn("pod cache refresh failed", "error", err)
			}
		}
	}
}

func (c *PodCache) refresh(ctx context.Context) error {
	var pods podList
	if err := c.client.doRequest(ctx, http.MethodGet, "/api/v1/pods", nil, &pods); err != nil {
		return err
	}
	var rsList replicaSetList
	if err := c.client.doRequest(ctx, http.MethodGet, "/apis/apps/v1/replicasets", nil, &rsList); err != nil {
		return err
	}

	rsByUID := make(map[string]objectMeta, len(rsList.Items))
	for _, rs := range rsList.Items {
		rsByUID[rs.Metadata.UID] = rs.Metadata
	}

	next := make(map[string]PodMeta, len(pods.Items))
	for _, p := range pods.Items {
		kind, workload := resolveWorkload(p.Metadata, rsByUID)
		next[p.Metadata.UID] = PodMeta{
			Namespace:      p.Metadata.Namespace,
			Pod:            p.Metadata.Name,
			ServiceAccount: p.Spec.ServiceAccountName,
			WorkloadKind:   kind,
			Workload:       workload,
		}
	}

	c.mu.Lock()
	c.byUID = next
	c.mu.Unlock()
	return nil
}

// resolveWorkload walks the owner chain to the top controller. A pod owned by a
// ReplicaSet is really owned by that RS's Deployment — resolving via the RS's
// own owner reference avoids fragile hash-stripping of the RS name.
func resolveWorkload(pod objectMeta, rsByUID map[string]objectMeta) (kind, workload string) {
	owner := controllerRef(pod.OwnerReferences)
	if owner == nil {
		return "Pod", pod.Name
	}
	if owner.Kind == "ReplicaSet" {
		if rs, ok := rsByUID[owner.UID]; ok {
			if d := controllerRef(rs.OwnerReferences); d != nil && d.Kind == "Deployment" {
				return "Deployment", d.Name
			}
		}
		return "ReplicaSet", owner.Name
	}
	return owner.Kind, owner.Name
}

func controllerRef(refs []ownerReference) *ownerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
}

type podSpec struct {
	ServiceAccountName string `json:"serviceAccountName"`
}

type replicaSetList struct {
	Items []replicaSet `json:"items"`
}

type replicaSet struct {
	Metadata objectMeta `json:"metadata"`
}

type objectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	OwnerReferences []ownerReference  `json:"ownerReferences"`
}

type ownerReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}
