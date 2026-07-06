package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func boolp(b bool) *bool { return &b }

// listServer serves fixed JSON for the pod and replicaset list endpoints.
func listServer(t *testing.T, pods podList, rs replicaSetList, ns namespaceList) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pods":
			_ = json.NewEncoder(w).Encode(pods)
		case "/apis/apps/v1/replicasets":
			_ = json.NewEncoder(w).Encode(rs)
		case "/api/v1/namespaces":
			_ = json.NewEncoder(w).Encode(ns)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPodCacheResolvesDeployment(t *testing.T) {
	pods := podList{Items: []pod{
		{
			Metadata: objectMeta{
				Name: "checkout-worker-rs-abc", Namespace: "payments", UID: "pod-uid-1",
				OwnerReferences: []ownerReference{{Kind: "ReplicaSet", Name: "checkout-worker-rs", UID: "rs-uid-1", Controller: boolp(true)}},
			},
			Spec: podSpec{ServiceAccountName: "checkout"},
		},
		{
			Metadata: objectMeta{
				Name: "db-0", Namespace: "payments", UID: "pod-uid-2",
				OwnerReferences: []ownerReference{{Kind: "StatefulSet", Name: "db", UID: "sts-uid-1", Controller: boolp(true)}},
			},
			Spec: podSpec{ServiceAccountName: "db"},
		},
		{
			Metadata: objectMeta{
				Name: "bare-pod", Namespace: "default", UID: "pod-uid-3",
			},
			Spec: podSpec{ServiceAccountName: "default"},
		},
		{
			Metadata: objectMeta{
				Name: "orphan-rs-pod", Namespace: "misc", UID: "pod-uid-4",
				OwnerReferences: []ownerReference{{Kind: "ReplicaSet", Name: "lonely-rs", UID: "rs-uid-2", Controller: boolp(true)}},
			},
		},
	}}
	rs := replicaSetList{Items: []replicaSet{
		{Metadata: objectMeta{
			Name: "checkout-worker-rs", Namespace: "payments", UID: "rs-uid-1",
			OwnerReferences: []ownerReference{{Kind: "Deployment", Name: "checkout-worker", UID: "deploy-uid-1", Controller: boolp(true)}},
		}},
		// rs-uid-2 has no Deployment owner (bare ReplicaSet).
		{Metadata: objectMeta{Name: "lonely-rs", Namespace: "misc", UID: "rs-uid-2"}},
	}}

	srv := listServer(t, pods, rs, namespaceList{})
	c := NewPodCache(fakeClient(t, srv), 0)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	tests := []struct {
		uid  string
		want PodMeta
	}{
		{"pod-uid-1", PodMeta{Namespace: "payments", Pod: "checkout-worker-rs-abc", ServiceAccount: "checkout", WorkloadKind: "Deployment", Workload: "checkout-worker"}},
		{"pod-uid-2", PodMeta{Namespace: "payments", Pod: "db-0", ServiceAccount: "db", WorkloadKind: "StatefulSet", Workload: "db"}},
		{"pod-uid-3", PodMeta{Namespace: "default", Pod: "bare-pod", ServiceAccount: "default", WorkloadKind: "Pod", Workload: "bare-pod"}},
		{"pod-uid-4", PodMeta{Namespace: "misc", Pod: "orphan-rs-pod", WorkloadKind: "ReplicaSet", Workload: "lonely-rs"}},
	}
	for _, tt := range tests {
		got, ok := c.Lookup(tt.uid)
		if !ok {
			t.Errorf("%s: not found", tt.uid)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: meta = %+v, want %+v", tt.uid, got, tt.want)
		}
	}
}

func TestPodCacheUnknownUID(t *testing.T) {
	srv := listServer(t, podList{}, replicaSetList{}, namespaceList{})
	c := NewPodCache(fakeClient(t, srv), 0)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("does-not-exist"); ok {
		t.Error("unknown UID should not be found")
	}
}

func TestPodCacheRefreshReplaces(t *testing.T) {
	// A pod present in one refresh must be gone after a refresh without it:
	// refresh swaps the whole map, so removed pods don't linger.
	var current podList
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pods":
			_ = json.NewEncoder(w).Encode(current)
		case "/apis/apps/v1/replicasets":
			_ = json.NewEncoder(w).Encode(replicaSetList{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewPodCache(fakeClient(t, srv), 0)

	current = podList{Items: []pod{{Metadata: objectMeta{Name: "a", Namespace: "n", UID: "u1"}}}}
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("u1"); !ok {
		t.Fatal("u1 should be present after first refresh")
	}

	current = podList{}
	if err := c.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("u1"); ok {
		t.Error("u1 should be gone after a refresh that omits it")
	}
}
