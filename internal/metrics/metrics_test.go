package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/internal/proxy"
)

// Collectors are process-global (promauto default registry), so each test uses
// distinct label values to stay isolated from the others.

func TestRecordRequestCounters(t *testing.T) {
	rec := proxy.RequestRecord{
		Agent:      auth.Agent{Name: "rr-agent", Team: "rr-team", Namespace: "rr-ns"},
		Provider:   "anthropic",
		Model:      "claude-x",
		Status:     200,
		DurationMS: 1500,
		Usage:      meter.Usage{InputTokens: 10, OutputTokens: 5},
		UsageOK:    true,
	}
	RecordRequest(rec, 0.25)
	RecordRequest(rec, 0.25)

	tests := []struct {
		name    string
		counter prometheus.Counter
		want    float64
	}{
		{"requests_total", requestsTotal.WithLabelValues("rr-agent", "rr-team", "rr-ns", "anthropic", "200"), 2},
		{"tokens_input", tokensTotal.WithLabelValues("rr-agent", "rr-team", "rr-ns", "input"), 20},
		{"tokens_output", tokensTotal.WithLabelValues("rr-agent", "rr-team", "rr-ns", "output"), 10},
		{"cost_usd", costUSDTotal.WithLabelValues("rr-agent", "rr-team", "rr-ns"), 0.5},
	}
	for _, tt := range tests {
		if got := testutil.ToFloat64(tt.counter); got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRecordRequestNoUsageStillCountsCost(t *testing.T) {
	rec := proxy.RequestRecord{
		Agent:    auth.Agent{Name: "nousage-agent"},
		Provider: "anthropic",
		Status:   500,
		UsageOK:  false,
	}
	RecordRequest(rec, 0)

	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("nousage-agent", "", "", "anthropic", "500")); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	// A no-usage request records a $0 cost sample so the agent still appears.
	if got := testutil.ToFloat64(costUSDTotal.WithLabelValues("nousage-agent", "", "")); got != 0 {
		t.Errorf("cost_usd_total = %v, want 0", got)
	}
}

func TestRecordDenied(t *testing.T) {
	agent := auth.Agent{Name: "denied-agent", Team: "dt", Namespace: "dn"}
	RecordDenied(agent, "throttled")
	RecordDenied(agent, "blocked_budget")
	RecordDenied(agent, "blocked_budget")

	if got := testutil.ToFloat64(requestsDeniedTotal.WithLabelValues("denied-agent", "dt", "dn", "throttled")); got != 1 {
		t.Errorf("throttled = %v, want 1", got)
	}
	if got := testutil.ToFloat64(requestsDeniedTotal.WithLabelValues("denied-agent", "dt", "dn", "blocked_budget")); got != 2 {
		t.Errorf("blocked_budget = %v, want 2", got)
	}
}

func TestStatusClass(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "2xx"}, {201, "2xx"}, {301, "3xx"}, {404, "4xx"},
		{429, "4xx"}, {500, "5xx"}, {503, "5xx"}, {0, "other"},
	}
	for _, c := range cases {
		if got := statusClass(c.status); got != c.want {
			t.Errorf("statusClass(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestRequestDurationBuckets(t *testing.T) {
	RecordRequest(proxy.RequestRecord{
		Agent: auth.Agent{Name: "hist"}, Provider: "hist-prov", Model: "hist-model",
		Status: 200, DurationMS: 300, // 0.3s
	}, 0)

	h := findHistogram(t, "hist-prov", "hist-model", "2xx")
	if h == nil {
		t.Fatal("no histogram series for hist-prov/hist-model/2xx")
	}
	wantBuckets := []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	if len(h.Bucket) != len(wantBuckets) {
		t.Fatalf("got %d buckets, want %d", len(h.Bucket), len(wantBuckets))
	}
	for i, b := range h.Bucket {
		if b.GetUpperBound() != wantBuckets[i] {
			t.Errorf("bucket %d upper = %v, want %v", i, b.GetUpperBound(), wantBuckets[i])
		}
		// 0.3s lands in cumulative buckets with upper bound >= 0.5.
		want := uint64(0)
		if b.GetUpperBound() >= 0.5 {
			want = 1
		}
		if b.GetCumulativeCount() != want {
			t.Errorf("bucket %v cumulative = %d, want %d", b.GetUpperBound(), b.GetCumulativeCount(), want)
		}
	}
	if h.GetSampleCount() != 1 {
		t.Errorf("sample count = %d, want 1", h.GetSampleCount())
	}
}

func findHistogram(t *testing.T, provider, model, class string) *dto.Histogram {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "tollgate_request_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["provider"] == provider && labels["model"] == model && labels["status_class"] == class {
				return m.Histogram
			}
		}
	}
	return nil
}
