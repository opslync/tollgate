// Package metrics exposes Tollgate's Prometheus collectors and the helpers that
// feed them. The request counters and the duration histogram are updated once
// per completed request via RecordRequest; requests denied by budget
// enforcement never reach the proxy, so they are counted separately via
// RecordDenied. Budget gauges are pull-based and live in budget_collector.go.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_requests_total",
		Help: "Total proxied requests by agent identity, provider, and HTTP status.",
	}, []string{"agent", "team", "namespace", "provider", "status"})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_tokens_total",
		Help: "Total tokens metered by agent identity and direction.",
	}, []string{"agent", "team", "namespace", "direction"})

	costUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_cost_usd_total",
		Help: "Total attributed cost in USD by agent identity.",
	}, []string{"agent", "team", "namespace"})

	requestsDeniedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tollgate_requests_denied_total",
		Help: "Total requests denied by budget enforcement, by reason.",
	}, []string{"agent", "team", "namespace", "reason"})

	// Buckets are tuned for LLM request latency (sub-second to minutes), not
	// web latency.
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tollgate_request_duration_seconds",
		Help:    "Proxied request duration in seconds by provider, model, and status class.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"provider", "model", "status_class"})
)
