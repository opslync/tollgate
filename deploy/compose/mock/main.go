// mock-anthropic is a deterministic stand-in for api.anthropic.com, used only
// by the docker-compose quickstart so it runs with no API key. Every
// non-streaming response costs exactly $0.000825 on claude-sonnet-5 pricing
// (25 input / 50 output tokens), so budget thresholds in the demo trip at a
// predictable request count.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const providerKey = "sk-ant-demo-provider-key"

const jsonBody = `{
  "id": "msg_01MockDemo0000000000001",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-5",
  "content": [{"type": "text", "text": "Hello from the Tollgate demo upstream."}],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {"input_tokens": 25, "output_tokens": 50}
}`

func main() {
	addr := ":9911"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		if r.Header.Get("x-api-key") != providerKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}

		w.Header().Set("request-id", "req_mock_demo_0000000001")

		if strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`) {
			streamSSE(w)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jsonBody)
	})

	log.Printf("mock-anthropic listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func streamSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_01MockDemoStream00000001", "type": "message", "role": "assistant",
			"model": "claude-sonnet-5", "content": []any{},
			"usage": map[string]any{"input_tokens": 25, "output_tokens": 1},
		},
	})
	send("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, chunk := range []string{"Hello", " from", " the", " demo", " upstream."} {
		time.Sleep(80 * time.Millisecond)
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": chunk},
		})
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 50},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}
