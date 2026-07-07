// Package trace hand-builds and exports OTLP/HTTP+JSON spans, one per
// proxied request. It deliberately avoids the official OTel SDK: the spec
// surface used here (one root span, a handful of attributes) is small and
// stable, and hand-rolling it keeps Tollgate's dependency footprint at zero
// for tracing, matching the same trade-off M7 made for Kubernetes access.
//
// The OTLP/JSON encoding follows protobuf's canonical JSON mapping: 64-bit
// integer fields (timestamps, int attribute values) are JSON strings, and
// bytes fields (traceId/spanId) are lowercase hex, not base64 — confirmed by
// posting real payloads to an otel-collector during implementation rather
// than trusting the spec description blind.
package trace

import (
	"fmt"
	"strconv"
	"time"

	"github.com/opslync/tollgate/internal/proxy"
)

const (
	spanKindServer  = 2
	statusCodeError = 2
)

// exportRequest mirrors opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest.
type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type resource struct {
	Attributes []attribute `json:"attributes"`
}

type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []span `json:"spans"`
}

type scope struct {
	Name string `json:"name"`
}

type span struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []attribute `json:"attributes"`
	Status            *status     `json:"status,omitempty"`
}

type status struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type attribute struct {
	Key   string    `json:"key"`
	Value attrValue `json:"value"`
}

type attrValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func strAttr(key, value string) attribute {
	return attribute{Key: key, Value: attrValue{StringValue: &value}}
}

func intAttr(key string, value int64) attribute {
	s := strconv.FormatInt(value, 10)
	return attribute{Key: key, Value: attrValue{IntValue: &s}}
}

func floatAttr(key string, value float64) attribute {
	return attribute{Key: key, Value: attrValue{DoubleValue: &value}}
}

// buildSpan turns one completed request into a single-span OTLP export
// request. Every request is a root span: Tollgate does not read or
// propagate an incoming trace context.
func buildSpan(rec proxy.RequestRecord, costUSD float64) (exportRequest, error) {
	traceID, err := newTraceID()
	if err != nil {
		return exportRequest{}, fmt.Errorf("generate trace id: %w", err)
	}
	spanID, err := newSpanID()
	if err != nil {
		return exportRequest{}, fmt.Errorf("generate span id: %w", err)
	}

	start := rec.Time
	end := start.Add(time.Duration(rec.DurationMS) * time.Millisecond)

	attrs := []attribute{
		strAttr("http.method", rec.Method),
		intAttr("http.status_code", int64(rec.Status)),
		strAttr("gen_ai.system", rec.Provider),
		strAttr("gen_ai.request.model", rec.Model),
		strAttr("tollgate.agent", rec.Agent.Name),
		floatAttr("tollgate.cost_usd", costUSD),
	}
	if rec.Agent.Team != "" {
		attrs = append(attrs, strAttr("tollgate.team", rec.Agent.Team))
	}
	if rec.Agent.Namespace != "" {
		attrs = append(attrs, strAttr("tollgate.namespace", rec.Agent.Namespace))
	}
	if rec.UsageOK {
		attrs = append(attrs,
			intAttr("gen_ai.usage.input_tokens", rec.Usage.InputTokens),
			intAttr("gen_ai.usage.output_tokens", rec.Usage.OutputTokens))
	}

	s := span{
		TraceID:           traceID,
		SpanID:            spanID,
		Name:              rec.Method + " " + rec.Path,
		Kind:              spanKindServer,
		StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(end.UnixNano(), 10),
		Attributes:        attrs,
	}
	if rec.Status >= 500 {
		s.Status = &status{Code: statusCodeError}
	}

	return exportRequest{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attributes: []attribute{strAttr("service.name", "tollgate")}},
		ScopeSpans: []scopeSpans{{Scope: scope{Name: "github.com/opslync/tollgate"}, Spans: []span{s}}},
	}}}, nil
}
