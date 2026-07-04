// Package proxy implements Tollgate's provider-transparent reverse proxy.
// Requests are forwarded to the upstream unmodified; responses stream back
// while token usage is parsed on the way through.
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

type Proxy struct {
	rp       *httputil.ReverseProxy
	logger   *slog.Logger
	provider string
}

// reqState carries per-request observations from the ReverseProxy callbacks
// out to the log line written when the request completes.
type reqState struct {
	status int
	err    error
}

type ctxKey struct{}

func New(provider string, upstream *url.URL, logger *slog.Logger) *Proxy {
	p := &Proxy{logger: logger, provider: provider}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Let our transport negotiate gzip and decompress transparently,
			// so the usage parser always sees plaintext.
			pr.Out.Header.Del("Accept-Encoding")
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
	if state, ok := resp.Request.Context().Value(ctxKey{}).(*reqState); ok {
		state.status = resp.StatusCode
	}
	return nil
}

// ServeHTTP forwards the request and, once the response body has fully
// streamed to the client, emits one structured log line.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	state := &reqState{}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, state))

	p.rp.ServeHTTP(w, r)

	attrs := []any{
		"provider", p.provider,
		"method", r.Method,
		"path", r.URL.Path,
		"status", state.status,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if state.err != nil {
		attrs = append(attrs, "error", state.err.Error())
	}
	p.logger.Info("request", attrs...)
}
