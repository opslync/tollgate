package main

// Group D — attribution integrity. Can a caller talk its way into someone
// else's line item, or out of its own bill?
//
// These run the real stack (auth -> budget -> proxy -> recorder -> SQLite),
// because attribution is a property of the whole path, not of any one package.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

var (
	cheapAgent   = agentKey("cheap-bot", "k-cheap-bot-00000001", "research", "dev")
	billingAgent = agentKey("billing-bot", "k-billing-bot-000001", "finance", "prod")
	twoAgents    = []config.Agent{cheapAgent, billingAgent}
)

// TestCorrectness_ClientCannotOverrideAttribution covers D1.
//
// INVARIANT: agent identity comes from the resolved key alone — no header, body
// field, or query parameter can move a request onto another agent's bill.
//
// The attack this blocks: a cheap, unbudgeted agent charging its spend to
// another team, or an agent under a hard block relabelling itself to get past
// enforcement.
func TestCorrectness_ClientCannotOverrideAttribution(t *testing.T) {
	// Every one of these is sent alongside cheap-bot's real key, and every one
	// names billing-bot / finance / prod.
	adversarialHeaders := map[string]string{
		"x-tollgate-agent":     "billing-bot",
		"x-tollgate-team":      "finance",
		"x-tollgate-namespace": "prod",
		"x-agent":              "billing-bot",
		"x-agent-name":         "billing-bot",
		"x-team":               "finance",
		"x-namespace":          "prod",
		"x-request-id":         "billing-bot",
		"x-forwarded-user":     "billing-bot",
		"user-agent":           "billing-bot",
		"anthropic-beta":       "agent=billing-bot",
	}
	adversarialBody := `{
		"model":"claude-haiku-4-5",
		"agent":"billing-bot",
		"team":"finance",
		"namespace":"prod",
		"metadata":{"user_id":"billing-bot","team":"finance"},
		"tollgate":{"agent":"billing-bot"},
		"messages":[{"role":"user","content":"hi"}]
	}`

	tests := []struct {
		name    string
		key     string
		headers map[string]string
		body    string
		path    string
	}{
		{
			name:    "headers and body both claim another agent",
			key:     cheapAgent.Key,
			headers: adversarialHeaders,
			body:    adversarialBody,
			path:    "/v1/messages",
		},
		{
			// extractKey reads x-api-key first, so a Bearer token naming a
			// different agent must be ignored, not merged.
			name:    "second credential in Authorization",
			key:     cheapAgent.Key,
			headers: map[string]string{"Authorization": "Bearer " + billingAgent.Key},
			body:    `{"model":"claude-haiku-4-5"}`,
			path:    "/v1/messages",
		},
		{
			name: "identity smuggled in the query string",
			key:  cheapAgent.Key,
			body: `{"model":"claude-haiku-4-5"}`,
			path: "/v1/messages?agent=billing-bot&team=finance&namespace=prod",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				agents:   twoAgents,
				upstream: jsonUpstream(anthropicJSONBody),
			})
			resp, _ := h.do(t, tc.key, tc.path, tc.body, tc.headers)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			h.waitForRecords(1)

			rows := h.rows(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			got := rows[0]
			if got.Agent != "cheap-bot" || got.Team != "research" || got.Namespace != "dev" {
				t.Errorf("attributed to agent=%q team=%q namespace=%q; want the key's own "+
					"identity cheap-bot/research/dev", got.Agent, got.Team, got.Namespace)
			}
		})
	}

	// An unauthenticated caller cannot conjure an identity out of headers alone.
	t.Run("headers alone authenticate nothing", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			agents:   twoAgents,
			upstream: jsonUpstream(anthropicJSONBody),
		})
		resp, _ := h.do(t, "", "/v1/messages", `{"model":"claude-haiku-4-5"}`, adversarialHeaders)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if rows := h.rows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0: a rejected request must not be attributed to anyone", len(rows))
		}
	})
}

// TestCorrectness_ClientCannotLowerRecordedCost covers D2.
//
// INVARIANT: cost is computed server-side from the usage parsed out of the
// upstream RESPONSE and the embedded pricing table. Nothing in the request can
// change it.
//
// The attack this blocks: an agent declaring its own (tiny, zero, or negative)
// usage so its spend never reaches a budget limit.
func TestCorrectness_ClientCannotLowerRecordedCost(t *testing.T) {
	// The upstream always reports the same real usage, whatever the client asked.
	const realUsage = `{"model":"claude-haiku-4-5","usage":{"input_tokens":1000,` +
		`"output_tokens":500,"cache_creation_input_tokens":2000,"cache_read_input_tokens":4000}}`
	// 1000@1.00 + 500@5.00 + 2000@1.25 + 4000@0.10 per MTok = $0.0064
	const wantCost = 0.0064

	spoofBodies := []struct {
		name string
		body string
	}{
		{
			name: "request declares zero usage",
			body: `{"model":"claude-haiku-4-5","usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "request declares negative usage",
			body: `{"model":"claude-haiku-4-5","usage":{"input_tokens":-1000000,"output_tokens":-1000000}}`,
		},
		{
			name: "request declares a cheaper model than the response",
			body: `{"model":"gpt-5-nano","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name: "request declares everything as cache reads",
			body: `{"model":"claude-haiku-4-5","usage":{"cache_read_input_tokens":7000}}`,
		},
	}

	spoofHeaders := map[string]string{
		"x-tollgate-cost":     "0",
		"x-cost-usd":          "0",
		"x-tollgate-tokens":   "0",
		"x-input-tokens":      "0",
		"x-output-tokens":     "0",
		"x-tollgate-model":    "gpt-5-nano",
		"x-tollgate-priced":   "false",
		"x-tollgate-no-meter": "true",
	}

	for _, sb := range spoofBodies {
		t.Run(sb.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				agents:   twoAgents,
				upstream: jsonUpstream(realUsage),
			})
			h.do(t, cheapAgent.Key, "/v1/messages", sb.body, spoofHeaders)
			h.waitForRecords(1)

			rows := h.rows(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			got := rows[0]
			if cents6(got.CostUSD) != cents6(wantCost) {
				t.Errorf("cost = $%.6f, want $%.6f (computed from the response, not the request)",
					got.CostUSD, wantCost)
			}
			if got.Model != "claude-haiku-4-5" {
				t.Errorf("model = %q, want the model the upstream reported", got.Model)
			}
			if got.InputTokens != 1000 || got.OutputTokens != 500 || got.CacheRead != 4000 {
				t.Errorf("usage = %d/%d/%d, want 1000/500/4000 from the response",
					got.InputTokens, got.OutputTokens, got.CacheRead)
			}
			if got.UsageStatus != store.UsageOK {
				t.Errorf("usage_status = %q, want %q", got.UsageStatus, store.UsageOK)
			}
		})
	}

	// The mirror case: a request that declares usage the response does not have
	// must be recorded as unpriced, never as the client's own figure.
	t.Run("spoofed usage cannot substitute for missing response usage", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			agents:   twoAgents,
			upstream: jsonUpstream(`{"model":"claude-haiku-4-5","content":[]}`),
		})
		h.do(t, cheapAgent.Key, "/v1/messages",
			`{"model":"claude-haiku-4-5","usage":{"input_tokens":42,"output_tokens":42}}`, spoofHeaders)
		h.waitForRecords(1)

		rows := h.rows(t)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].InputTokens != 0 || rows[0].OutputTokens != 0 {
			t.Errorf("usage = %d/%d, want 0/0: the request's own numbers must never be believed",
				rows[0].InputTokens, rows[0].OutputTokens)
		}
		if rows[0].UsageStatus != store.UsageUnparsed {
			t.Errorf("usage_status = %q, want %q", rows[0].UsageStatus, store.UsageUnparsed)
		}
	})
}

// TestCorrectness_ConcurrentAgentsAttributeIndependently covers D3.
//
// INVARIANT: concurrent requests from different agents never bleed into each
// other's totals — each agent's stored spend is exactly its own.
//
// The failure being guarded against is per-request identity leaking into shared
// state (a cached "current user", a struct field reused across requests), which
// pins every later request to whoever arrived first.
func TestCorrectness_ConcurrentAgentsAttributeIndependently(t *testing.T) {
	// Each agent uses a distinct model with distinct usage, so a mis-attributed
	// request is visible in the totals rather than hiding in an average.
	type profile struct {
		agent  config.Agent
		model  string
		input  int64
		output int64
		cost   float64 // per request, from pricing.yaml
	}
	profiles := []profile{
		// claude-haiku-4-5: $1.00 in / $5.00 out per MTok
		{agentKey("haiku-bot", "k-haiku-bot-00000001", "research", "dev"),
			"claude-haiku-4-5", 1000, 100, 1000*1.00/1e6 + 100*5.00/1e6},
		// claude-sonnet-4-5: $3.00 / $15.00
		{agentKey("sonnet-bot", "k-sonnet-bot-0000001", "support", "prod"),
			"claude-sonnet-4-5", 2000, 200, 2000*3.00/1e6 + 200*15.00/1e6},
		// claude-opus-4-5: $5.00 / $25.00
		{agentKey("opus-bot", "k-opus-bot-000000001", "platform", "prod"),
			"claude-opus-4-5", 3000, 300, 3000*5.00/1e6 + 300*25.00/1e6},
		// claude-fable-5: $10.00 / $50.00
		{agentKey("fable-bot", "k-fable-bot-00000001", "platform", "staging"),
			"claude-fable-5", 4000, 400, 4000*10.00/1e6 + 400*50.00/1e6},
	}

	byModel := map[string]profile{}
	agents := make([]config.Agent, 0, len(profiles))
	for _, p := range profiles {
		byModel[p.model] = p
		agents = append(agents, p.agent)
	}

	// The upstream answers with the usage that belongs to the requested model,
	// so the response for one agent is never valid for another.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("upstream got unparseable body: %v", err)
			return
		}
		p, ok := byModel[req.Model]
		if !ok {
			t.Errorf("upstream got unknown model %q", req.Model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			p.model, p.input, p.output)
	})

	const perAgent = 40
	h := newHarness(t, harnessOptions{agents: agents, upstream: upstream})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, p := range profiles {
		for i := 0; i < perAgent; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, _ := h.do(t, p.agent.Key, "/v1/messages",
					fmt.Sprintf(`{"model":%q}`, p.model), nil)
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s: status = %d, want 200", p.agent.Name, resp.StatusCode)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	h.waitForRecords(len(profiles) * perAgent)

	// Tally what landed on disk, per agent.
	type tally struct {
		requests          int
		input, output     int64
		cost              float64
		teams, namespaces map[string]bool
		models            map[string]bool
	}
	got := map[string]*tally{}
	for _, row := range h.rows(t) {
		tl := got[row.Agent]
		if tl == nil {
			tl = &tally{
				teams: map[string]bool{}, namespaces: map[string]bool{}, models: map[string]bool{},
			}
			got[row.Agent] = tl
		}
		tl.requests++
		tl.input += row.InputTokens
		tl.output += row.OutputTokens
		tl.cost += row.CostUSD
		tl.teams[row.Team] = true
		tl.namespaces[row.Namespace] = true
		tl.models[row.Model] = true
	}

	if len(got) != len(profiles) {
		t.Fatalf("distinct agents in the store = %d, want %d", len(got), len(profiles))
	}
	for _, p := range profiles {
		tl := got[p.agent.Name]
		if tl == nil {
			t.Errorf("%s has no rows at all", p.agent.Name)
			continue
		}
		if tl.requests != perAgent {
			t.Errorf("%s: requests = %d, want %d", p.agent.Name, tl.requests, perAgent)
		}
		if tl.input != p.input*perAgent || tl.output != p.output*perAgent {
			t.Errorf("%s: tokens = %d/%d, want %d/%d", p.agent.Name,
				tl.input, tl.output, p.input*perAgent, p.output*perAgent)
		}
		if cents6(tl.cost) != cents6(p.cost*perAgent) {
			t.Errorf("%s: cost = $%.6f, want $%.6f", p.agent.Name, tl.cost, p.cost*perAgent)
		}
		if len(tl.teams) != 1 || !tl.teams[p.agent.Team] {
			t.Errorf("%s: teams = %v, want only %q", p.agent.Name, keys(tl.teams), p.agent.Team)
		}
		if len(tl.namespaces) != 1 || !tl.namespaces[p.agent.Namespace] {
			t.Errorf("%s: namespaces = %v, want only %q", p.agent.Name, keys(tl.namespaces), p.agent.Namespace)
		}
		if len(tl.models) != 1 || !tl.models[p.model] {
			t.Errorf("%s: models = %v, want only %q", p.agent.Name, keys(tl.models), p.model)
		}
	}
}

func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}
