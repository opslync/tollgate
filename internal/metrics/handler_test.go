package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/internal/proxy"
)

// TestMetricsHandler exercises the same promhttp.Handler() main mounts at
// GET /metrics: 200, the Prometheus text exposition content type, and our
// metric names present in the body.
func TestMetricsHandler(t *testing.T) {
	RecordRequest(proxy.RequestRecord{
		Agent: auth.Agent{Name: "handler-agent"}, Provider: "anthropic", Status: 200,
		Usage: meter.Usage{InputTokens: 3, OutputTokens: 1}, UsageOK: true,
	}, 0.01)
	RecordDenied(auth.Agent{Name: "handler-agent"}, "blocked_budget")

	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"tollgate_requests_total",
		"tollgate_tokens_total",
		"tollgate_cost_usd_total",
		"tollgate_requests_denied_total",
		"tollgate_request_duration_seconds",
	} {
		if !strings.Contains(string(body), name) {
			t.Errorf("metrics body missing %q", name)
		}
	}
}
