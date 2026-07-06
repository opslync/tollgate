package k8s

import (
	"context"
	"testing"

	"github.com/opslync/tollgate/internal/config"
)

func TestTeamMapStatic(t *testing.T) {
	m := NewTeamMap(nil, []config.Team{
		{Name: "payments", Namespaces: []string{"payments", "payments-staging"}},
		{Name: "search", Namespaces: []string{"search"}},
	})

	tests := map[string]string{
		"payments":         "payments",
		"payments-staging": "payments",
		"search":           "search",
		"unmapped":         "",
	}
	for ns, want := range tests {
		if got := m.Team(ns); got != want {
			t.Errorf("Team(%q) = %q, want %q", ns, got, want)
		}
	}
}

func TestTeamMapLabelPrecedence(t *testing.T) {
	nsList := namespaceList{Items: []namespace{
		{Metadata: objectMeta{Name: "payments", Labels: map[string]string{teamLabel: "payments-labelled"}}},
		{Metadata: objectMeta{Name: "search", Labels: map[string]string{"unrelated": "x"}}},
	}}
	srv := listServer(t, podList{}, replicaSetList{}, nsList)

	m := NewTeamMap(fakeClient(t, srv), []config.Team{
		{Name: "payments-static", Namespaces: []string{"payments"}},
		{Name: "search", Namespaces: []string{"search"}},
	})
	if err := m.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Namespace label overrides the static config mapping.
	if got := m.Team("payments"); got != "payments-labelled" {
		t.Errorf("Team(payments) = %q, want label value", got)
	}
	// Namespace without the team label falls back to static config.
	if got := m.Team("search"); got != "search" {
		t.Errorf("Team(search) = %q, want static value", got)
	}
	// Still unmapped stays empty.
	if got := m.Team("nope"); got != "" {
		t.Errorf("Team(nope) = %q, want empty", got)
	}
}
