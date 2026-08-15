package main

// Group E — identity integrity for Kubernetes ServiceAccount authentication.
//
// Group D proved a *static key* cannot be talked out of its own identity. This
// group covers the identity Tollgate actually differentiates on: a pod
// authenticated by the ServiceAccount token the kubelet projected into it. The
// question a design partner asks is not "can I fake a header" but "can pod A
// bill pod B, or present pod B's identity at all".
//
// These run the full stack — auth -> budget -> proxy -> recorder -> SQLite —
// behind the REAL internal/k8s chain (Authenticator + PodCache + TeamMap +
// Resolver) talking to a fake API server. Nothing here is a stub reimplementing
// what the resolver does; the only thing replaced is the API server itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/k8s"
)

// --- fake API server -------------------------------------------------------
//
// Same conventions as the fake servers in internal/k8s's own tests (a
// token -> status table for TokenReview, fixed JSON for the list endpoints);
// those helpers are unexported, so the wire shapes are restated here as the
// JSON a real API server returns.

// tokenIdentity is the TokenReview verdict for one token. podName/podUID stand
// in for the `authentication.kubernetes.io/pod-*` extras, which only the API
// server sets, and only for a token genuinely bound to that pod.
type tokenIdentity struct {
	authenticated bool
	username      string
	podName       string
	podUID        string
}

// k8sPod is one pod in the fake cluster, owned by a Deployment through a
// ReplicaSet — the owner chain PodCache actually has to walk.
type k8sPod struct {
	namespace      string
	name           string
	uid            string
	serviceAccount string
	deployment     string
}

func (p k8sPod) rsName() string { return p.deployment + "-59d8f7" }
func (p k8sPod) rsUID() string  { return p.uid + "-rs" }

type apiOwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type apiMeta struct {
	Name            string        `json:"name"`
	Namespace       string        `json:"namespace"`
	UID             string        `json:"uid"`
	OwnerReferences []apiOwnerRef `json:"ownerReferences,omitempty"`
}

type apiListItem struct {
	Metadata apiMeta `json:"metadata"`
	Spec     struct {
		ServiceAccountName string `json:"serviceAccountName,omitempty"`
	} `json:"spec"`
}

type apiList struct {
	Items []apiListItem `json:"items"`
}

// fakeK8sAPI serves the three endpoints internal/k8s calls: TokenReview,
// the pod list and the replicaset list.
type fakeK8sAPI struct {
	t      *testing.T
	tokens map[string]tokenIdentity
	pods   []k8sPod

	// reviewed counts TokenReview round trips, so a test can prove a credential
	// never reached the API server at all.
	reviewed atomic.Int64
	// polled is signalled (non-blocking) at the start of each pod-cache poll.
	polled chan struct{}
}

func (a *fakeK8sAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/apis/authentication.k8s.io/v1/tokenreviews":
		a.reviewed.Add(1)
		a.serveTokenReview(w, r)
	case "/api/v1/pods":
		select {
		case a.polled <- struct{}{}:
		default:
		}
		a.servePods(w)
	case "/apis/apps/v1/replicasets":
		a.serveReplicaSets(w)
	case "/api/v1/namespaces":
		_ = json.NewEncoder(w).Encode(apiList{})
	default:
		a.t.Errorf("fake API server got unexpected path %q", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (a *fakeK8sAPI) serveTokenReview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Spec struct {
			Token string `json:"token"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		a.t.Errorf("decode TokenReview request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Unknown tokens are what a forged or foreign-issuer token looks like: the
	// API server validated the signature and rejected it.
	id := a.tokens[in.Spec.Token]
	user := map[string]any{"username": id.username}
	if id.podUID != "" {
		user["extra"] = map[string][]string{
			"authentication.kubernetes.io/pod-name": {id.podName},
			"authentication.kubernetes.io/pod-uid":  {id.podUID},
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"status": map[string]any{
			"authenticated": id.authenticated,
			"user":          user,
		},
	})
}

func (a *fakeK8sAPI) servePods(w http.ResponseWriter) {
	var out apiList
	for _, p := range a.pods {
		item := apiListItem{Metadata: apiMeta{
			Name: p.name, Namespace: p.namespace, UID: p.uid,
			OwnerReferences: []apiOwnerRef{{
				Kind: "ReplicaSet", Name: p.rsName(), UID: p.rsUID(), Controller: true,
			}},
		}}
		item.Spec.ServiceAccountName = p.serviceAccount
		out.Items = append(out.Items, item)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (a *fakeK8sAPI) serveReplicaSets(w http.ResponseWriter) {
	var out apiList
	for _, p := range a.pods {
		out.Items = append(out.Items, apiListItem{Metadata: apiMeta{
			Name: p.rsName(), Namespace: p.namespace, UID: p.rsUID(),
			OwnerReferences: []apiOwnerRef{{
				Kind: "Deployment", Name: p.deployment, UID: p.deployment + "-uid", Controller: true,
			}},
		}})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newK8sReviewer stands up the fake API server and wires the real internal/k8s
// chain behind it, returning the auth.TokenReviewer main.go would inject. The
// pod cache is warm on return.
func newK8sReviewer(t *testing.T, api *fakeK8sAPI, teams []config.Team) auth.TokenReviewer {
	t.Helper()
	api.t = t
	api.polled = make(chan struct{}, 2)

	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	client := k8s.NewClientForURL(srv.URL, srv.Client())
	cache := k8s.NewPodCache(client, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.Run(ctx, discardLogger())
	}()
	// Stop the poller before the server it polls goes away.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Wait for the SECOND poll to begin. PodCache.Run refreshes sequentially, so
	// cycle 2 cannot start until cycle 1 has swapped its map in — which makes
	// this a happens-before edge, not a sleep.
	<-api.polled
	<-api.polled

	return k8s.NewResolver(
		k8s.NewAuthenticator(client, nil), cache, k8s.NewTeamMap(client, teams), discardLogger())
}

// saToken builds a credential shaped like a projected ServiceAccount token:
// three dot-separated segments, which is what auth.looksLikeJWT gates on.
func saToken(name string) string {
	return "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3QifQ." + name + ".c2lnbmF0dXJl"
}

func saUser(namespace, sa string) string {
	return "system:serviceaccount:" + namespace + ":" + sa
}

// boundToken is the happy path: the API server authenticated the token AND
// reported the pod it was issued for.
func boundToken(p k8sPod) tokenIdentity {
	return tokenIdentity{
		authenticated: true,
		username:      saUser(p.namespace, p.serviceAccount),
		podName:       p.name,
		podUID:        p.uid,
	}
}

// neverUpstream fails the test if the proxy ever forwards to it, and also
// records the hit so the assertion is explicit rather than implicit.
func neverUpstream(t *testing.T) (http.Handler, chan string) {
	t.Helper()
	hits := make(chan string, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream was reached by a request that must have been rejected: %s %s",
			r.Method, r.URL.Path)
		select {
		case hits <- r.Method + " " + r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicJSONBody))
	}), hits
}

// --- E1 --------------------------------------------------------------------

// TestCorrectness_UnauthenticatedTokenFailsClosed covers E1.
//
// INVARIANT: a token that does not authenticate (forged, expired, or a
// non-ServiceAccount principal) never produces an identity, never reaches the
// upstream, and nothing is recorded or billed for it.
//
// Tollgate performs no signature validation of its own — the API server does,
// and TokenReview's authenticated:false is the whole answer. What this test
// pins down is that every failure path in that chain ends the request: no
// fallback identity, no unattributed passthrough, no row on disk. The one
// authenticated-but-rejected case (a human user token) matters separately: the
// API server would happily authenticate `alice@example.com`, but that principal
// is not an agent Tollgate can attribute, so it must be refused rather than
// invented into one.
func TestCorrectness_UnauthenticatedTokenFailsClosed(t *testing.T) {
	genuine := k8sPod{
		namespace: "payments", name: "checkout-worker-59d8f7-b2k9x",
		uid: "pod-uid-genuine", serviceAccount: "checkout", deployment: "checkout-worker",
	}
	teams := []config.Team{{Name: "payments-team", Namespaces: []string{"payments"}}}

	newAPI := func() *fakeK8sAPI {
		return &fakeK8sAPI{
			pods: []k8sPod{genuine},
			tokens: map[string]tokenIdentity{
				saToken("genuine"): boundToken(genuine),
				// The API server rejected these outright.
				saToken("expired"): {authenticated: false},
				// Authenticated, but not a ServiceAccount.
				saToken("human"): {authenticated: true, username: "alice@example.com"},
				saToken("node"):  {authenticated: true, username: "system:node:ip-10-0-1-7"},
				saToken("group"): {authenticated: true, username: "system:anonymous"},
			},
		}
	}

	rejected := []struct {
		name string
		key  string
	}{
		{
			// Never seen by the API server: a token minted by another issuer, or
			// one whose payload was edited to claim a different ServiceAccount.
			name: "forged token the API server does not recognise",
			key:  saToken("forged-claims-billing-sa"),
		},
		{
			name: "expired token",
			key:  saToken("expired"),
		},
		{
			name: "authenticated human user, not a ServiceAccount",
			key:  saToken("human"),
		},
		{
			name: "authenticated node identity, not a ServiceAccount",
			key:  saToken("node"),
		},
		{
			name: "authenticated anonymous principal",
			key:  saToken("group"),
		},
		{
			// A genuine token's middle segment on its own: not JWT-shaped, so it
			// must be refused without even asking the API server.
			name: "credential that is not token-shaped",
			key:  "genuine",
		},
	}

	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPI()
			upstream, hits := neverUpstream(t)
			h := newHarness(t, harnessOptions{
				reviewer: newK8sReviewer(t, api, teams),
				upstream: upstream,
			})
			before := api.reviewed.Load()

			resp, body := h.do(t, tc.key, "/v1/messages",
				`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`, nil)

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if !strings.Contains(body, "authentication_error") {
				t.Errorf("body = %q, want an authentication_error", body)
			}
			if len(hits) != 0 {
				t.Errorf("upstream received %q; a rejected token must never be forwarded", <-hits)
			}
			if rows := h.rows(t); len(rows) != 0 {
				t.Errorf("rows = %d, want 0: an unauthenticated request must not be recorded or billed: %+v",
					len(rows), rows)
			}
			if tc.key == "genuine" && api.reviewed.Load() != before {
				t.Errorf("TokenReview calls = %d, want %d: a credential that isn't token-shaped "+
					"must not reach the API server", api.reviewed.Load(), before)
			}
		})
	}

	// Control: the same stack accepts the genuine token. Without this, every
	// assertion above could pass on a harness that rejects everything.
	t.Run("control: the genuine bound token is accepted and attributed", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			reviewer: newK8sReviewer(t, newAPI(), teams),
			upstream: jsonUpstream(anthropicJSONBody),
		})
		resp, _ := h.do(t, saToken("genuine"), "/v1/messages", `{"model":"claude-haiku-4-5"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		h.waitForRecords(1)

		rows := h.rows(t)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		got := rows[0]
		if got.Agent != "payments/checkout-worker" || got.Team != "payments-team" ||
			got.Namespace != "payments" || got.Pod != genuine.name ||
			got.ServiceAccount != "checkout" || got.Workload != "checkout-worker" ||
			got.WorkloadKind != "Deployment" {
			t.Errorf("row = %+v, want the genuine pod's own identity", got)
		}
	})
}

// --- E2 --------------------------------------------------------------------

// podProfile is one pod in the fake cluster plus the traffic it sends. Model and
// usage differ per pod so a mis-attributed request shows up in the totals, not
// just in a label.
type podProfile struct {
	pod    k8sPod
	token  string
	team   string
	model  string
	input  int64
	output int64
}

func (p podProfile) agentName() string { return p.pod.namespace + "/" + p.pod.deployment }

// TestCorrectness_TokenSubstitutionCannotCrossAttribute covers E2.
//
// INVARIANT: a pod authenticated by its own ServiceAccount token is never
// attributed as a different pod, ServiceAccount, or workload — including under
// concurrent, interleaved use of multiple pods' tokens.
//
// This is the property Tollgate's attribution story rests on: the identity is
// bound to the workload by the kubelet and confirmed by the API server, so it
// cannot be copied between pods the way a static key can. The failure mode being
// hunted is per-request identity leaking into shared state — a cached "current
// pod", a struct field reused across requests, a cache keyed on something that
// collides. The four profiles deliberately overlap on every dimension but one:
// two share a namespace, two share a ServiceAccount name across namespaces, and
// all four have distinct pod UIDs. A resolver that conflated on namespace, on
// ServiceAccount name, or on pod name would have to fail here.
func TestCorrectness_TokenSubstitutionCannotCrossAttribute(t *testing.T) {
	profiles := []podProfile{
		{
			pod: k8sPod{namespace: "payments", name: "checkout-worker-59d8f7-aaa",
				uid: "pod-uid-a", serviceAccount: "checkout", deployment: "checkout-worker"},
			token: saToken("pod-a"), team: "payments-team",
			model: "claude-haiku-4-5", input: 1000, output: 100,
		},
		{
			// Same namespace as pod A, different ServiceAccount and Deployment.
			pod: k8sPod{namespace: "payments", name: "reporter-59d8f7-bbb",
				uid: "pod-uid-b", serviceAccount: "reporter", deployment: "reporter"},
			token: saToken("pod-b"), team: "payments-team",
			model: "claude-sonnet-4-5", input: 2000, output: 200,
		},
		{
			// Same ServiceAccount NAME as pod A, different namespace: an identity
			// keyed on the bare SA name would collide here.
			pod: k8sPod{namespace: "analytics", name: "checkout-agg-59d8f7-ccc",
				uid: "pod-uid-c", serviceAccount: "checkout", deployment: "checkout-agg"},
			token: saToken("pod-c"), team: "analytics-team",
			model: "claude-opus-4-5", input: 3000, output: 300,
		},
		{
			pod: k8sPod{namespace: "search", name: "indexer-59d8f7-ddd",
				uid: "pod-uid-d", serviceAccount: "indexer", deployment: "indexer"},
			token: saToken("pod-d"), team: "search-team",
			model: "claude-fable-5", input: 4000, output: 400,
		},
	}

	api := &fakeK8sAPI{tokens: map[string]tokenIdentity{}}
	byModel := map[string]podProfile{}
	for _, p := range profiles {
		api.pods = append(api.pods, p.pod)
		api.tokens[p.token] = boundToken(p.pod)
		byModel[p.model] = p
	}
	teams := []config.Team{
		{Name: "payments-team", Namespaces: []string{"payments"}},
		{Name: "analytics-team", Namespaces: []string{"analytics"}},
		{Name: "search-team", Namespaces: []string{"search"}},
	}

	// The upstream answers with the usage belonging to the requested model, so
	// one pod's response is never a valid response for another.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("upstream got unparseable body: %v", err)
			return
		}
		p, ok := byModel[req.Model]
		if !ok {
			t.Errorf("upstream got unknown model %q", req.Model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			p.model, p.input, p.output)
	})

	h := newHarness(t, harnessOptions{
		reviewer: newK8sReviewer(t, api, teams),
		upstream: upstream,
	})

	const perPod = 30
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, p := range profiles {
		for i := 0; i < perPod; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, body := h.do(t, p.token, "/v1/messages",
					fmt.Sprintf(`{"model":%q}`, p.model), nil)
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s: status = %d, want 200: %s", p.pod.name, resp.StatusCode, body)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	h.waitForRecords(len(profiles) * perPod)

	type tally struct {
		requests      int
		input, output int64
		teams, ns     map[string]bool
		pods, sas     map[string]bool
		workloads     map[string]bool
		models        map[string]bool
	}
	got := map[string]*tally{}
	for _, row := range h.rows(t) {
		tl := got[row.Agent]
		if tl == nil {
			tl = &tally{
				teams: map[string]bool{}, ns: map[string]bool{}, pods: map[string]bool{},
				sas: map[string]bool{}, workloads: map[string]bool{}, models: map[string]bool{},
			}
			got[row.Agent] = tl
		}
		tl.requests++
		tl.input += row.InputTokens
		tl.output += row.OutputTokens
		tl.teams[row.Team] = true
		tl.ns[row.Namespace] = true
		tl.pods[row.Pod] = true
		tl.sas[row.ServiceAccount] = true
		tl.workloads[row.Workload] = true
		tl.models[row.Model] = true
	}

	if len(got) != len(profiles) {
		t.Fatalf("distinct agents in the store = %d (%v), want %d",
			len(got), agentNames(got), len(profiles))
	}
	for _, p := range profiles {
		tl := got[p.agentName()]
		if tl == nil {
			t.Errorf("%s has no rows at all", p.agentName())
			continue
		}
		if tl.requests != perPod {
			t.Errorf("%s: requests = %d, want %d", p.agentName(), tl.requests, perPod)
		}
		if tl.input != p.input*perPod || tl.output != p.output*perPod {
			t.Errorf("%s: tokens = %d/%d, want %d/%d", p.agentName(),
				tl.input, tl.output, p.input*perPod, p.output*perPod)
		}
		for _, check := range []struct {
			field string
			set   map[string]bool
			want  string
		}{
			{"team", tl.teams, p.team},
			{"namespace", tl.ns, p.pod.namespace},
			{"pod", tl.pods, p.pod.name},
			{"service_account", tl.sas, p.pod.serviceAccount},
			{"workload", tl.workloads, p.pod.deployment},
			{"model", tl.models, p.model},
		} {
			if len(check.set) != 1 || !check.set[check.want] {
				t.Errorf("%s: %s = {%s}, want only %q — another pod's identity bled in",
					p.agentName(), check.field, keys(check.set), check.want)
			}
		}
	}
}

func agentNames[T any](m map[string]T) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

// --- E3 --------------------------------------------------------------------

// TestCorrectness_HeadersCannotInjectWorkloadIdentity covers E3.
//
// INVARIANT: no client-supplied header can inject or override pod,
// ServiceAccount, namespace, or workload attribution when identity comes from a
// ServiceAccount token.
//
// D1 proved this for the static-key path. The Kubernetes path has strictly more
// attribution surface — pod, ServiceAccount, workload kind and workload name are
// all recorded — and every one of those fields is a field an attacker would like
// to set. They come from exactly two places: TokenReview's response (which only
// the API server writes) and the pod cache keyed by the UID in that response.
// Neither reads the request.
func TestCorrectness_HeadersCannotInjectWorkloadIdentity(t *testing.T) {
	mine := k8sPod{namespace: "research", name: "cheap-bot-59d8f7-aaa",
		uid: "pod-uid-mine", serviceAccount: "cheap-bot", deployment: "cheap-bot"}
	theirs := k8sPod{namespace: "finance", name: "billing-bot-59d8f7-zzz",
		uid: "pod-uid-theirs", serviceAccount: "billing-bot", deployment: "billing-bot"}

	teams := []config.Team{
		{Name: "research-team", Namespaces: []string{"research"}},
		{Name: "finance-team", Namespaces: []string{"finance"}},
	}
	newReviewer := func(t *testing.T) auth.TokenReviewer {
		return newK8sReviewer(t, &fakeK8sAPI{
			pods: []k8sPod{mine, theirs},
			tokens: map[string]tokenIdentity{
				saToken("mine"):   boundToken(mine),
				saToken("theirs"): boundToken(theirs),
			},
		}, teams)
	}

	// Every header names the other pod, its namespace, its ServiceAccount, its
	// workload, or its UID.
	spoofHeaders := map[string]string{
		"x-tollgate-pod":             theirs.name,
		"x-tollgate-pod-uid":         theirs.uid,
		"x-tollgate-service-account": theirs.serviceAccount,
		"x-tollgate-workload":        theirs.deployment,
		"x-tollgate-workload-kind":   "Deployment",
		"x-tollgate-namespace":       theirs.namespace,
		"x-tollgate-team":            "finance-team",
		"x-tollgate-agent":           "finance/billing-bot",
		"x-pod-name":                 theirs.name,
		"x-pod-uid":                  theirs.uid,
		"x-service-account":          theirs.serviceAccount,
		"x-namespace":                theirs.namespace,
		"x-team":                     "finance-team",
		"x-agent":                    "finance/billing-bot",
		"x-workload":                 theirs.deployment,
		"x-request-id":               "finance/billing-bot",
		"x-forwarded-user":           saUser(theirs.namespace, theirs.serviceAccount),
		"user-agent":                 "billing-bot",
		// The extras the API server sets for a bound token, spelled as legal
		// header names (a real header cannot contain "/"). If any of these were
		// read off the request rather than the TokenReview response, this is
		// where it would show.
		"authentication-kubernetes-io-pod-name":  theirs.name,
		"authentication-kubernetes-io-pod-uid":   theirs.uid,
		"x-authentication-kubernetes-io-pod-uid": theirs.uid,
	}

	// Kubernetes' own impersonation headers, which a proxy that forwarded them
	// naively could turn into a privilege escalation.
	impersonation := map[string]string{
		"Impersonate-User":  saUser(theirs.namespace, theirs.serviceAccount),
		"Impersonate-Uid":   theirs.uid,
		"Impersonate-Group": "system:serviceaccounts:" + theirs.namespace,
	}

	spoofBody := fmt.Sprintf(`{
		"model":"claude-haiku-4-5",
		"pod":%q,"pod_uid":%q,"service_account":%q,"namespace":%q,
		"workload":%q,"team":"finance-team","agent":"finance/billing-bot",
		"tollgate":{"pod":%q,"service_account":%q},
		"metadata":{"user_id":%q},
		"messages":[{"role":"user","content":"hi"}]
	}`, theirs.name, theirs.uid, theirs.serviceAccount, theirs.namespace,
		theirs.deployment, theirs.name, theirs.serviceAccount, theirs.name)

	tests := []struct {
		name    string
		key     string
		headers map[string]string
		body    string
	}{
		{
			name:    "headers and body both claim another pod",
			key:     saToken("mine"),
			headers: spoofHeaders,
			body:    spoofBody,
		},
		{
			name:    "kubernetes impersonation headers",
			key:     saToken("mine"),
			headers: impersonation,
			body:    `{"model":"claude-haiku-4-5"}`,
		},
		{
			// Both credentials are genuine and bound. extractKey reads x-api-key
			// first, so the request is the pod whose token is in x-api-key —
			// never a merge of the two, and never the other pod.
			name:    "a second, genuine pod token in Authorization",
			key:     saToken("mine"),
			headers: map[string]string{"Authorization": "Bearer " + saToken("theirs")},
			body:    `{"model":"claude-haiku-4-5"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				reviewer: newReviewer(t),
				upstream: jsonUpstream(anthropicJSONBody),
			})
			resp, body := h.do(t, tc.key, "/v1/messages", tc.body, tc.headers)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			h.waitForRecords(1)

			rows := h.rows(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			got := rows[0]
			want := recordedRow{
				Agent: "research/cheap-bot", Team: "research-team", Namespace: "research",
				Pod: mine.name, ServiceAccount: mine.serviceAccount,
				Workload: mine.deployment, WorkloadKind: "Deployment",
			}
			if got.Agent != want.Agent || got.Team != want.Team || got.Namespace != want.Namespace ||
				got.Pod != want.Pod || got.ServiceAccount != want.ServiceAccount ||
				got.Workload != want.Workload || got.WorkloadKind != want.WorkloadKind {
				t.Errorf("attributed to agent=%q team=%q namespace=%q pod=%q sa=%q workload=%q/%q;\n"+
					"want the token's own identity agent=%q team=%q namespace=%q pod=%q sa=%q workload=%q/%q",
					got.Agent, got.Team, got.Namespace, got.Pod, got.ServiceAccount,
					got.WorkloadKind, got.Workload,
					want.Agent, want.Team, want.Namespace, want.Pod, want.ServiceAccount,
					want.WorkloadKind, want.Workload)
			}
		})
	}

	// The mirror case: headers alone, with no token at all, authenticate nothing
	// — the workload fields cannot be conjured out of thin air either.
	t.Run("workload headers alone authenticate nothing", func(t *testing.T) {
		upstream, hits := neverUpstream(t)
		h := newHarness(t, harnessOptions{
			reviewer: newReviewer(t),
			upstream: upstream,
		})
		resp, _ := h.do(t, "", "/v1/messages", `{"model":"claude-haiku-4-5"}`, spoofHeaders)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if len(hits) != 0 {
			t.Errorf("upstream received %q", <-hits)
		}
		if rows := h.rows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0", len(rows))
		}
	})
}
