package trace

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/meter"
	"github.com/opslync/tollgate/internal/proxy"
)

func attrMap(t *testing.T, attrs []attribute) map[string]attrValue {
	t.Helper()
	m := make(map[string]attrValue, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}

func TestBuildSpanShape(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := proxy.RequestRecord{
		Time:       start,
		Agent:      auth.Agent{Name: "agent-1", Team: "team-a", Namespace: "ns-a"},
		Provider:   "anthropic",
		Method:     "POST",
		Path:       "/v1/messages",
		Model:      "claude-x",
		Status:     200,
		DurationMS: 500,
		Usage:      meter.Usage{InputTokens: 10, OutputTokens: 20},
		UsageOK:    true,
	}

	req, err := buildSpan(rec, 0.0042)
	if err != nil {
		t.Fatalf("buildSpan: %v", err)
	}

	if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("expected exactly one span, got shape %+v", req)
	}
	s := req.ResourceSpans[0].ScopeSpans[0].Spans[0]

	if len(s.TraceID) != 32 {
		t.Errorf("trace id %q: got length %d, want 32", s.TraceID, len(s.TraceID))
	}
	if len(s.SpanID) != 16 {
		t.Errorf("span id %q: got length %d, want 16", s.SpanID, len(s.SpanID))
	}
	if s.Name != "POST /v1/messages" {
		t.Errorf("name = %q, want %q", s.Name, "POST /v1/messages")
	}
	if s.Kind != spanKindServer {
		t.Errorf("kind = %d, want %d", s.Kind, spanKindServer)
	}
	wantStart := strconv.FormatInt(start.UnixNano(), 10)
	if s.StartTimeUnixNano != wantStart {
		t.Errorf("startTimeUnixNano = %q, want %q", s.StartTimeUnixNano, wantStart)
	}
	wantEnd := strconv.FormatInt(start.Add(500*time.Millisecond).UnixNano(), 10)
	if s.EndTimeUnixNano != wantEnd {
		t.Errorf("endTimeUnixNano = %q, want %q", s.EndTimeUnixNano, wantEnd)
	}
	if s.Status != nil {
		t.Errorf("status = %+v, want nil for a 200", s.Status)
	}

	attrs := attrMap(t, s.Attributes)
	wantStr := map[string]string{
		"http.method":          "POST",
		"gen_ai.system":        "anthropic",
		"gen_ai.request.model": "claude-x",
		"tollgate.agent":       "agent-1",
		"tollgate.team":        "team-a",
		"tollgate.namespace":   "ns-a",
	}
	for k, want := range wantStr {
		v, ok := attrs[k]
		if !ok || v.StringValue == nil || *v.StringValue != want {
			t.Errorf("attribute %q = %+v, want stringValue %q", k, v, want)
		}
	}
	wantInt := map[string]string{
		"http.status_code":           "200",
		"gen_ai.usage.input_tokens":  "10",
		"gen_ai.usage.output_tokens": "20",
	}
	for k, want := range wantInt {
		v, ok := attrs[k]
		if !ok || v.IntValue == nil || *v.IntValue != want {
			t.Errorf("attribute %q = %+v, want intValue %q", k, v, want)
		}
	}
	if v := attrs["tollgate.cost_usd"]; v.DoubleValue == nil || *v.DoubleValue != 0.0042 {
		t.Errorf("tollgate.cost_usd = %+v, want doubleValue 0.0042", v)
	}
}

func TestBuildSpanServerError(t *testing.T) {
	rec := proxy.RequestRecord{
		Agent:  auth.Agent{Name: "agent-1"},
		Method: "POST", Path: "/v1/messages", Status: 502,
	}
	req, err := buildSpan(rec, 0)
	if err != nil {
		t.Fatalf("buildSpan: %v", err)
	}
	s := req.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if s.Status == nil || s.Status.Code != statusCodeError {
		t.Errorf("status = %+v, want ERROR for a 502", s.Status)
	}
}

func TestBuildSpanUsageNotOK(t *testing.T) {
	rec := proxy.RequestRecord{
		Agent:  auth.Agent{Name: "agent-1"},
		Method: "POST", Path: "/v1/messages", Status: 200,
		UsageOK: false,
	}
	req, err := buildSpan(rec, 0)
	if err != nil {
		t.Fatalf("buildSpan: %v", err)
	}
	attrs := attrMap(t, req.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes)
	if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
		t.Errorf("expected no token attributes when UsageOK is false, got %+v", attrs)
	}
}

// TestBuildSpanJSONShape guards the exact wire encoding confirmed against a
// real otel-collector: hex ids (not base64) and string-encoded int64 fields.
func TestBuildSpanJSONShape(t *testing.T) {
	rec := proxy.RequestRecord{
		Agent:  auth.Agent{Name: "agent-1"},
		Method: "POST", Path: "/v1/messages", Status: 200,
	}
	req, err := buildSpan(rec, 0)
	if err != nil {
		t.Fatalf("buildSpan: %v", err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	span := decoded["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
	if _, ok := span["startTimeUnixNano"].(string); !ok {
		t.Errorf("startTimeUnixNano must be a JSON string, got %T", span["startTimeUnixNano"])
	}
	if _, ok := span["traceId"].(string); !ok {
		t.Errorf("traceId must be a JSON string, got %T", span["traceId"])
	}
}
