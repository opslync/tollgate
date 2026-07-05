package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
)

// logBuffer is a goroutine-safe sink for the proxy's slog output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestProxyLogged(t *testing.T, upstream string) (*httptest.Server, *logBuffer) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	srv := httptest.NewServer(New("test", u, "", logger))
	t.Cleanup(srv.Close)
	return srv, logs
}

func newTestProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	srv, _ := newTestProxyLogged(t, upstream)
	return srv
}

// waitForLog polls for the proxy's request log line, which is written just
// after the response body finishes streaming to the client.
func waitForLog(t *testing.T, logs *logBuffer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := logs.String(); strings.Contains(s, "msg=request") {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for request log line")
	return ""
}

func TestPassthrough(t *testing.T) {
	var gotPath, gotQuery, gotAPIKey, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_123")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", "sk-ant-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/messages" || gotQuery != "beta=true" {
		t.Errorf("upstream saw path=%q query=%q", gotPath, gotQuery)
	}
	if gotAPIKey != "sk-ant-test" {
		t.Errorf("upstream saw x-api-key=%q", gotAPIKey)
	}
	if gotBody != `{"model":"m"}` {
		t.Errorf("upstream saw body=%q", gotBody)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("request-id"); got != "req_123" {
		t.Errorf("request-id = %q", got)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

// TestStreamingFlush verifies SSE events are forwarded as they arrive rather
// than buffered: the upstream withholds its second event until the client has
// observed the first one through the proxy.
func TestStreamingFlush(t *testing.T) {
	firstReceived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "event: one\ndata: {}\n\n")
		f.Flush()
		select {
		case <-firstReceived:
		case <-time.After(5 * time.Second):
			t.Error("client never observed first event; proxy is buffering the stream")
			return
		}
		io.WriteString(w, "event: two\ndata: {}\n\n")
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL)

	resp, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			sb.WriteString(line)
			if err != nil || line == "\n" {
				return sb.String()
			}
		}
	}

	if got := readEvent(); !strings.Contains(got, "event: one") {
		t.Fatalf("first event = %q", got)
	}
	close(firstReceived)
	if got := readEvent(); !strings.Contains(got, "event: two") {
		t.Fatalf("second event = %q", got)
	}
}

func TestUsageLoggedNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":25,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyLogged(t, upstream.URL)

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := waitForLog(t, logs)
	for _, want := range []string{"model=claude-sonnet-5", "input_tokens=25", "output_tokens=50", "stream=false", "status=200"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestUsageLoggedStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":472,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":91}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyLogged(t, upstream.URL)

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := waitForLog(t, logs)
	for _, want := range []string{"model=claude-sonnet-5", "input_tokens=472", "output_tokens=91", "stream=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestUsageUnknownOnErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer upstream.Close()

	proxy, logs := newTestProxyLogged(t, upstream.URL)

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := waitForLog(t, logs)
	if !strings.Contains(got, "usage=unknown") || !strings.Contains(got, "status=400") {
		t.Errorf("log missing usage=unknown/status=400:\n%s", got)
	}
}

// TestProviderKeyInjection covers the M2 credential swap: the agent's
// Tollgate key is terminated at the proxy and the provider key goes upstream.
func TestProviderKeyInjection(t *testing.T) {
	var gotAPIKey, gotAuthz string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuthz = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New("test", u, "sk-ant-provider-key", logger))
	defer srv.Close()

	for _, header := range []string{"x-api-key", "Authorization"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
		if header == "x-api-key" {
			req.Header.Set("x-api-key", "tg_agent_key_0123456789abcdef")
		} else {
			req.Header.Set("Authorization", "Bearer tg_agent_key_0123456789abcdef")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if gotAPIKey != "sk-ant-provider-key" {
			t.Errorf("agent key via %s: upstream x-api-key = %q, want provider key", header, gotAPIKey)
		}
		if gotAuthz != "" {
			t.Errorf("agent key via %s: Authorization leaked upstream: %q", header, gotAuthz)
		}
	}
}

func TestAttributionLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	authn := auth.New([]config.Agent{
		{Name: "support-bot", Key: "tg_agent_key_0123456789abcdef", Team: "support", Namespace: "prod"},
	})
	srv := httptest.NewServer(authn.Middleware(New("test", u, "sk-real", logger)))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "tg_agent_key_0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := waitForLog(t, logs)
	for _, want := range []string{"agent=support-bot", "team=support", "namespace=prod"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestRecorderReceivesCompletedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-sonnet-5","usage":{"input_tokens":25,"output_tokens":50}}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New("test", u, "sk-real", logger)

	recorded := make(chan RequestRecord, 1)
	p.SetRecorder(func(rec RequestRecord) { recorded <- rec })

	authn := auth.New([]config.Agent{
		{Name: "support-bot", Key: "tg_agent_key_0123456789abcdef", Team: "support", Namespace: "prod"},
	})
	srv := httptest.NewServer(authn.Middleware(p))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "tg_agent_key_0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case rec := <-recorded:
		if rec.Agent.Name != "support-bot" || rec.Agent.Team != "support" {
			t.Errorf("agent = %+v", rec.Agent)
		}
		if !rec.UsageOK || rec.Model != "claude-sonnet-5" ||
			rec.Usage.InputTokens != 25 || rec.Usage.OutputTokens != 50 {
			t.Errorf("usage = %+v ok=%v model=%q", rec.Usage, rec.UsageOK, rec.Model)
		}
		if rec.Status != 200 || rec.Provider != "test" || rec.Path != "/v1/messages" {
			t.Errorf("record = %+v", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recorder was never called")
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	// A closed port: reserve one with a listener, then shut it down.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	proxy := newTestProxy(t, dead.URL)

	resp, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}
