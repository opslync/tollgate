package k8s

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/opslync/tollgate/internal/config"
)

// teamLabel on a namespace overrides the static config mapping, letting teams
// self-declare ownership without a Tollgate config change.
const teamLabel = "tollgate.io/team"

// TeamMap resolves a namespace to a team name: the namespace's tollgate.io/team
// label (refreshed by a background poll) wins over the static config mapping.
type TeamMap struct {
	client *Client
	static map[string]string // namespace -> team, from config

	mu     sync.RWMutex
	labels map[string]string // namespace -> team, from namespace labels
}

func NewTeamMap(client *Client, teams []config.Team) *TeamMap {
	static := make(map[string]string)
	for _, t := range teams {
		for _, ns := range t.Namespaces {
			static[ns] = t.Name
		}
	}
	return &TeamMap{client: client, static: static, labels: map[string]string{}}
}

// Team returns the team for a namespace, or "" when unmapped.
func (m *TeamMap) Team(namespace string) string {
	m.mu.RLock()
	label := m.labels[namespace]
	m.mu.RUnlock()
	if label != "" {
		return label
	}
	return m.static[namespace]
}

// Run refreshes the namespace-label cache once immediately then every interval
// until ctx is cancelled.
func (m *TeamMap) Run(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if err := m.refresh(ctx); err != nil {
		logger.Warn("team map initial refresh failed", "error", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.refresh(ctx); err != nil {
				logger.Warn("team map refresh failed", "error", err)
			}
		}
	}
}

func (m *TeamMap) refresh(ctx context.Context) error {
	var nsList namespaceList
	if err := m.client.doRequest(ctx, http.MethodGet, "/api/v1/namespaces", nil, &nsList); err != nil {
		return err
	}
	next := make(map[string]string)
	for _, ns := range nsList.Items {
		if team := ns.Metadata.Labels[teamLabel]; team != "" {
			next[ns.Metadata.Name] = team
		}
	}
	m.mu.Lock()
	m.labels = next
	m.mu.Unlock()
	return nil
}

type namespaceList struct {
	Items []namespace `json:"items"`
}

type namespace struct {
	Metadata objectMeta `json:"metadata"`
}
