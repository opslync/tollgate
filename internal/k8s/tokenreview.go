package k8s

import (
	"context"
	"net/http"
	"strings"
)

// Identity is the authenticated result of a TokenReview. PodName/PodUID are
// populated only for bound projected tokens (kubelet default since k8s 1.20);
// their presence is the anti-spoofing anchor — the API server sets them only
// when the token was genuinely issued for that pod.
type Identity struct {
	Username       string // system:serviceaccount:<namespace>:<name>
	Namespace      string
	ServiceAccount string
	PodName        string
	PodUID         string
}

// Authenticator validates ServiceAccount tokens via the TokenReview API.
type Authenticator struct {
	client    *Client
	audiences []string // empty accepts the API server's default audience
}

func NewAuthenticator(client *Client, audiences []string) *Authenticator {
	return &Authenticator{client: client, audiences: audiences}
}

type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec"`
	Status     tokenReviewStatus `json:"status,omitempty"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewStatus struct {
	Authenticated bool     `json:"authenticated"`
	User          userInfo `json:"user"`
	Error         string   `json:"error,omitempty"`
}

type userInfo struct {
	Username string              `json:"username"`
	UID      string              `json:"uid"`
	Groups   []string            `json:"groups"`
	Extra    map[string][]string `json:"extra"`
}

// ReviewToken POSTs a TokenReview and returns the authenticated identity. The
// API server validates the token's signature against the cluster issuer, so a
// forged, expired, or foreign token comes back authenticated:false → ok=false.
func (a *Authenticator) ReviewToken(ctx context.Context, token string) (Identity, bool) {
	review := tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token, Audiences: a.audiences},
	}
	var out tokenReview
	if err := a.client.doRequest(ctx, http.MethodPost,
		"/apis/authentication.k8s.io/v1/tokenreviews", review, &out); err != nil {
		return Identity{}, false
	}
	if !out.Status.Authenticated {
		return Identity{}, false
	}
	ns, sa, ok := parseServiceAccountUsername(out.Status.User.Username)
	if !ok {
		// Authenticated but not a ServiceAccount (e.g. a human user token):
		// nothing to attribute, so reject rather than invent an identity.
		return Identity{}, false
	}
	return Identity{
		Username:       out.Status.User.Username,
		Namespace:      ns,
		ServiceAccount: sa,
		PodName:        firstExtra(out.Status.User.Extra, "authentication.kubernetes.io/pod-name"),
		PodUID:         firstExtra(out.Status.User.Extra, "authentication.kubernetes.io/pod-uid"),
	}, true
}

func parseServiceAccountUsername(u string) (namespace, name string, ok bool) {
	rest, ok := strings.CutPrefix(u, "system:serviceaccount:")
	if !ok {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

func firstExtra(extra map[string][]string, key string) string {
	if v := extra[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}
