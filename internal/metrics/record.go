package metrics

import (
	"strconv"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/proxy"
)

// RecordRequest updates the request counters and the duration histogram from
// one completed request. costUSD is the already-priced cost (0 when usage was
// unavailable or the model is unpriced); it is added unconditionally so every
// active agent shows up in spend-by-agent, even at $0.
func RecordRequest(rec proxy.RequestRecord, costUSD float64) {
	a := rec.Agent
	requestsTotal.WithLabelValues(a.Name, a.Team, a.Namespace, rec.Provider, strconv.Itoa(rec.Status)).Inc()
	costUSDTotal.WithLabelValues(a.Name, a.Team, a.Namespace).Add(costUSD)
	if rec.UsageOK {
		tokensTotal.WithLabelValues(a.Name, a.Team, a.Namespace, "input").Add(float64(rec.Usage.InputTokens))
		tokensTotal.WithLabelValues(a.Name, a.Team, a.Namespace, "output").Add(float64(rec.Usage.OutputTokens))
	}
	requestDuration.WithLabelValues(rec.Provider, rec.Model, statusClass(rec.Status)).
		Observe(float64(rec.DurationMS) / 1000)
}

// RecordDenied counts one request rejected by budget enforcement. reason is one
// of "throttled", "blocked_budget", "blocked_killed".
func RecordDenied(agent auth.Agent, reason string) {
	requestsDeniedTotal.WithLabelValues(agent.Name, agent.Team, agent.Namespace, reason).Inc()
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "other"
	}
}
