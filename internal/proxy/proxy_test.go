package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New("test", u, logger))
	t.Cleanup(srv.Close)
	return srv
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
