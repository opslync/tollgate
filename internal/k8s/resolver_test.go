package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/opslync/tollgate/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestResolverFullWorkload(t *testing.T) {
	trSrv := tokenReviewServer(t, map[string]tokenReviewStatus{
		"bound": {Authenticated: true, User: userInfo{
			Username: "system:serviceaccount:payments:checkout",
			Extra: map[string][]string{
				"authentication.kubernetes.io/pod-name": {"checkout-worker-abc"},
				"authentication.kubernetes.io/pod-uid":  {"pod-uid-1"},
			},
		}},
	})
	authn := NewAuthenticator(fakeClient(t, trSrv), nil)

	listSrv := listServer(t,
		podList{Items: []pod{{
			Metadata: objectMeta{Name: "checkout-worker-abc", Namespace: "payments", UID: "pod-uid-1",
				OwnerReferences: []ownerReference{{Kind: "ReplicaSet", Name: "checkout-worker-rs", UID: "rs-1", Controller: boolp(true)}}},
			Spec: podSpec{ServiceAccountName: "checkout"},
		}}},
		replicaSetList{Items: []replicaSet{{Metadata: objectMeta{Name: "checkout-worker-rs", UID: "rs-1",
			OwnerReferences: []ownerReference{{Kind: "Deployment", Name: "checkout-worker", UID: "d-1", Controller: boolp(true)}}}}}},
		namespaceList{},
	)
	cache := NewPodCache(fakeClient(t, listSrv), 0)
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	teams := NewTeamMap(nil, []config.Team{{Name: "payments-team", Namespaces: []string{"payments"}}})

	r := NewResolver(authn, cache, teams, quietLogger())
	agent, wl, ok := r.Review(context.Background(), "bound")
	if !ok {
		t.Fatal("expected authenticated")
	}
	if agent.Name != "payments/checkout-worker" || agent.Team != "payments-team" || agent.Namespace != "payments" {
		t.Errorf("agent = %+v", agent)
	}
	if wl.Workload != "checkout-worker" || wl.WorkloadKind != "Deployment" || wl.Pod != "checkout-worker-abc" || wl.ServiceAccount != "checkout" {
		t.Errorf("workload = %+v", wl)
	}
}

func TestResolverUnboundToken(t *testing.T) {
	// No pod-uid extra: attribute by ServiceAccount, no workload.
	trSrv := tokenReviewServer(t, map[string]tokenReviewStatus{
		"unbound": {Authenticated: true, User: userInfo{Username: "system:serviceaccount:search:indexer"}},
	})
	r := NewResolver(NewAuthenticator(fakeClient(t, trSrv), nil),
		NewPodCache(nil, 0), NewTeamMap(nil, nil), quietLogger())

	agent, wl, ok := r.Review(context.Background(), "unbound")
	if !ok {
		t.Fatal("expected authenticated")
	}
	if agent.Name != "search/indexer" || agent.Namespace != "search" {
		t.Errorf("agent = %+v, want SA-level naming", agent)
	}
	if wl.Workload != "" || wl.WorkloadKind != "" {
		t.Errorf("workload = %+v, want empty for unbound token", wl)
	}
	if wl.ServiceAccount != "indexer" {
		t.Errorf("workload SA = %q, want indexer", wl.ServiceAccount)
	}
}

func TestResolverPodNotCached(t *testing.T) {
	// Bound token but the pod isn't in the cache yet: fall back to SA-level.
	trSrv := tokenReviewServer(t, map[string]tokenReviewStatus{
		"bound": {Authenticated: true, User: userInfo{
			Username: "system:serviceaccount:payments:checkout",
			Extra: map[string][]string{
				"authentication.kubernetes.io/pod-name": {"checkout-abc"},
				"authentication.kubernetes.io/pod-uid":  {"uid-missing"},
			},
		}},
	})
	listSrv := listServer(t, podList{}, replicaSetList{}, namespaceList{})
	cache := NewPodCache(fakeClient(t, listSrv), 0)
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(NewAuthenticator(fakeClient(t, trSrv), nil), cache, NewTeamMap(nil, nil), quietLogger())

	agent, wl, ok := r.Review(context.Background(), "bound")
	if !ok {
		t.Fatal("expected authenticated")
	}
	if agent.Name != "payments/checkout" {
		t.Errorf("agent = %+v, want SA-level fallback", agent)
	}
	if wl.Pod != "checkout-abc" {
		t.Errorf("workload pod = %q, want pod name from token", wl.Pod)
	}
}

func TestResolverRejectsForgedToken(t *testing.T) {
	// authenticated:false is how the API server reports a forged/foreign token.
	trSrv := tokenReviewServer(t, map[string]tokenReviewStatus{})
	r := NewResolver(NewAuthenticator(fakeClient(t, trSrv), nil),
		NewPodCache(nil, 0), NewTeamMap(nil, nil), quietLogger())

	if _, _, ok := r.Review(context.Background(), "forged"); ok {
		t.Error("forged token must be rejected")
	}
}
