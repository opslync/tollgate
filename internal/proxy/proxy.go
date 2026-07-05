// Package proxy implements Tollgate's provider-transparent reverse proxy.
// Requests are forwarded to the upstream unmodified; responses stream back
// while token usage is parsed on the way through.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/meter"
)

// Options describes one upstream provider.
type Options struct {
	Name     string
	Type     string // "anthropic" or "openai" — selects usage parser and credential header
	Upstream *url.URL
	// APIKey, when non-empty, terminates the caller's credentials here and
	// is injected upstream in the provider's native header; when empty,
	// credentials pass through.
	APIKey string
}

type Proxy struct {
	rp       *httputil.ReverseProxy
	logger   *slog.Logger
	provider string
	ptype    string
	recorder Recorder
}

// RequestRecord is everything observed about one completed request, handed
// to the Recorder after the response body has streamed to the client.
type RequestRecord struct {
	Time       time.Time
	Agent      auth.Agent
	Provider   string
	Method     string
	Path       string
	Model      string
	Status     int
	DurationMS int64
	Stream     bool
	Usage      meter.Usage
	UsageOK    bool
}

// Recorder receives completed requests (e.g. to persist them). It runs on
// the request goroutine after the response finished, so it must not block
// for long.
type Recorder func(RequestRecord)

// SetRecorder installs the recorder; call before serving traffic.
func (p *Proxy) SetRecorder(r Recorder) { p.recorder = r }

// reqState carries per-request observations from the ReverseProxy callbacks
// out to the log line written when the request completes.
type reqState struct {
	status int
	err    error
	stream bool
	parser meter.Parser
}

type ctxKey struct{}

// New builds a proxy for one upstream provider.
func New(opts Options, logger *slog.Logger) *Proxy {
	if opts.Type == "" {
		opts.Type = "anthropic"
	}
	p := &Proxy{logger: logger, provider: opts.Name, ptype: opts.Type}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(opts.Upstream)
			// Let our transport negotiate gzip and decompress transparently,
			// so the usage parser always sees plaintext.
			pr.Out.Header.Del("Accept-Encoding")
			if opts.APIKey != "" {
				// The agent key must not leak upstream, whichever header it
				// arrived in; the provider key goes in its native header.
				if opts.Type == "openai" {
					pr.Out.Header.Set("Authorization", "Bearer "+opts.APIKey)
					pr.Out.Header.Del("x-api-key")
				} else {
					pr.Out.Header.Set("x-api-key", opts.APIKey)
					pr.Out.Header.Del("Authorization")
				}
			}
		},
		// Flush immediately: SSE events must reach the agent as they arrive.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if state, ok := r.Context().Value(ctxKey{}).(*reqState); ok {
				state.status = http.StatusBadGateway
				state.err = err
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	state, ok := resp.Request.Context().Value(ctxKey{}).(*reqState)
	if !ok {
		return nil
	}
	state.status = resp.StatusCode

	contentType := resp.Header.Get("Content-Type")
	state.stream = strings.HasPrefix(contentType, "text/event-stream")
	if parser := meter.ForResponse(p.ptype, contentType); parser != nil {
		state.parser = parser
		resp.Body = &meteringBody{rc: resp.Body, parser: parser}
	}
	return nil
}

// meteringBody tees response body bytes into the usage parser as the
// ReverseProxy copies them to the client. The stream itself is untouched.
type meteringBody struct {
	rc     io.ReadCloser
	parser meter.Parser
}

func (b *meteringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.parser.Feed(p[:n])
	}
	return n, err
}

func (b *meteringBody) Close() error { return b.rc.Close() }

// ServeHTTP forwards the request and, once the response body has fully
// streamed to the client, emits one structured log line.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	state := &reqState{}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, state))

	p.rp.ServeHTTP(w, r)

	rec := RequestRecord{
		Time:       start,
		Provider:   p.provider,
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     state.status,
		DurationMS: time.Since(start).Milliseconds(),
		Stream:     state.stream,
	}
	if agent, ok := auth.FromContext(r.Context()); ok {
		rec.Agent = agent
	}
	if state.parser != nil {
		rec.Usage, rec.UsageOK = state.parser.Finish()
		rec.Model = rec.Usage.Model
	}
	if p.recorder != nil {
		p.recorder(rec)
	}

	attrs := []any{
		"provider", p.provider,
		"method", r.Method,
		"path", r.URL.Path,
		"status", state.status,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if agent, ok := auth.FromContext(r.Context()); ok {
		attrs = append(attrs, "agent", agent.Name)
		if agent.Team != "" {
			attrs = append(attrs, "team", agent.Team)
		}
		if agent.Namespace != "" {
			attrs = append(attrs, "namespace", agent.Namespace)
		}
	}
	if state.err != nil {
		attrs = append(attrs, "error", state.err.Error())
	}
	if state.parser != nil {
		if rec.UsageOK {
			attrs = append(attrs,
				"model", rec.Usage.Model,
				"stream", state.stream,
				"input_tokens", rec.Usage.InputTokens,
				"output_tokens", rec.Usage.OutputTokens,
				"cache_creation_input_tokens", rec.Usage.CacheCreationInputTokens,
				"cache_read_input_tokens", rec.Usage.CacheReadInputTokens,
			)
		} else {
			attrs = append(attrs, "usage", "unknown")
		}
	}
	p.logger.Info("request", attrs...)
}
