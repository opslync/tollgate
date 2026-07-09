// Package budget enforces rolling-window spend limits and the kill switch.
//
// Spend is tracked in memory: counters are seeded from the store, re-synced
// every refresh interval (which also ages spend out of rolling windows), and
// incremented live as each request completes — so a runaway loop is counted
// request by request, not at the next poll. The bias is fail-closed: brief
// overcounting around a re-sync is possible, undercounting is not.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/store"
)

const defaultRefreshEvery = 5 * time.Second

type Kind int

const (
	Allow Kind = iota
	Throttled
	BlockedBudget
	BlockedKilled
)

// GaugeStateKind is a budget's current enforcement state as a single numeric
// value, so a Grafana panel can render it with a 0-3 value mapping.
type GaugeStateKind int

const (
	StateOK GaugeStateKind = iota
	StateAlert
	StateThrottled
	StateBlocked
)

// GaugeState is a point-in-time snapshot of one budget (or one killed agent
// without a backing budget), emitted by the Prometheus budget collector.
type GaugeState struct {
	Dimension string // "agent" | "team"
	Target    string
	State     GaugeStateKind
	Ratio     float64
	HasRatio  bool // false for a killed agent that has no budget to measure
}

// Decision is the enforcement outcome for one request.
type Decision struct {
	Kind        Kind
	Budget      config.Budget // set for Throttled / BlockedBudget
	RetryAfter  time.Duration // set for Throttled
	SpendUSD    float64
	SpendTokens int64
}

type tracked struct {
	cfg        config.Budget
	dim, value string

	baseUSD     float64 // store totals at lastRefresh
	baseTokens  int64
	deltaUSD    float64 // live increments since lastRefresh
	deltaTokens int64
	lastRefresh time.Time

	alerted           bool
	lastThrottleAllow time.Time
}

func (t *tracked) usd() float64  { return t.baseUSD + t.deltaUSD }
func (t *tracked) tokens() int64 { return t.baseTokens + t.deltaTokens }

// ratio is the fraction of the budget consumed: the max over whichever limits
// are set, so a budget with both a dollar and a token limit reports whichever
// is closer to its cap.
func (t *tracked) ratio() float64 {
	var r float64
	if t.cfg.LimitUSD > 0 {
		r = t.usd() / t.cfg.LimitUSD
	}
	if t.cfg.LimitTokens > 0 {
		if tr := float64(t.tokens()) / float64(t.cfg.LimitTokens); tr > r {
			r = tr
		}
	}
	return r
}

func (t *tracked) matches(a auth.Agent) bool {
	return (t.dim == "agent" && t.value == a.Name) || (t.dim == "team" && t.value == a.Team)
}

func (t *tracked) over() bool {
	return (t.cfg.LimitUSD > 0 && t.usd() >= t.cfg.LimitUSD) ||
		(t.cfg.LimitTokens > 0 && t.tokens() >= t.cfg.LimitTokens)
}

func (t *tracked) nearLimit() bool {
	return (t.cfg.LimitUSD > 0 && t.usd() >= t.cfg.AlertAt*t.cfg.LimitUSD) ||
		(t.cfg.LimitTokens > 0 && t.tokens() >= int64(t.cfg.AlertAt*float64(t.cfg.LimitTokens)))
}

type Engine struct {
	store        *store.Store
	logger       *slog.Logger
	refreshEvery time.Duration
	now          func() time.Time

	mu      sync.Mutex
	budgets []*tracked
	killed  map[string]bool

	// deniedHook, when set, is called once per rejected request. It lets the
	// metrics layer count denials without budget importing it (a cycle).
	deniedHook func(agent auth.Agent, reason string)
}

// SetDeniedHook registers a callback invoked for each request the middleware
// rejects (throttled/blocked/killed). Call before serving traffic.
func (e *Engine) SetDeniedHook(fn func(agent auth.Agent, reason string)) {
	e.deniedHook = fn
}

func New(st *store.Store, budgets []config.Budget, logger *slog.Logger) *Engine {
	e := &Engine{
		store:        st,
		logger:       logger,
		refreshEvery: defaultRefreshEvery,
		now:          time.Now,
		killed:       map[string]bool{},
	}
	for _, b := range budgets {
		t := &tracked{cfg: b, dim: "agent", value: b.Agent}
		if b.Team != "" {
			t.dim, t.value = "team", b.Team
		}
		e.budgets = append(e.budgets, t)
	}
	return e
}

// LoadKills seeds the in-memory kill set from the store at startup.
func (e *Engine) LoadKills(ctx context.Context) error {
	kills, err := e.store.Kills(ctx)
	if err != nil {
		return fmt.Errorf("load kill switch state: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, agent := range kills {
		e.killed[agent] = true
		e.logger.Warn("kill switch active from previous run", "agent", agent)
	}
	return nil
}

// Check decides whether this request may proceed. Called before forwarding.
func (e *Engine) Check(ctx context.Context, agent auth.Agent) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.killed[agent.Name] {
		return Decision{Kind: BlockedKilled}
	}

	now := e.now()
	worst := Decision{Kind: Allow}
	for _, t := range e.budgets {
		if !t.matches(agent) {
			continue
		}
		e.refreshLocked(ctx, t, now)

		if !t.over() {
			switch {
			case t.nearLimit() && !t.alerted:
				t.alerted = true
				e.logger.Warn("budget alert threshold crossed",
					"target", t.dim+":"+t.value,
					"spend_usd", t.usd(), "spend_tokens", t.tokens(),
					"limit_usd", t.cfg.LimitUSD, "limit_tokens", t.cfg.LimitTokens,
					"alert_at", t.cfg.AlertAt)
			case !t.nearLimit():
				t.alerted = false // spend aged out; re-arm the alert
			}
			continue
		}

		d := Decision{Budget: t.cfg, SpendUSD: t.usd(), SpendTokens: t.tokens()}
		if t.cfg.Action == "throttle" {
			interval := time.Duration(t.cfg.ThrottleInterval)
			if since := now.Sub(t.lastThrottleAllow); since >= interval {
				t.lastThrottleAllow = now
				continue // this request is the one allowed per interval
			} else {
				d.Kind = Throttled
				d.RetryAfter = interval - since
			}
		} else {
			d.Kind = BlockedBudget
		}
		if d.Kind > worst.Kind {
			worst = d
		}
	}
	return worst
}

// refreshLocked re-syncs a budget's counters from the store when stale,
// which is also what ages old spend out of the rolling window. On a storage
// error the previous counters stay in force rather than failing the request.
func (e *Engine) refreshLocked(ctx context.Context, t *tracked, now time.Time) {
	if now.Sub(t.lastRefresh) < e.refreshEvery {
		return
	}
	usd, tokens, err := e.store.Spend(ctx, t.dim, t.value, now.Add(-time.Duration(t.cfg.Window)))
	if err != nil {
		e.logger.Error("budget spend refresh failed; enforcing with stale counters",
			"target", t.dim+":"+t.value, "error", err)
		return
	}
	t.baseUSD, t.baseTokens = usd, tokens
	t.deltaUSD, t.deltaTokens = 0, 0
	t.lastRefresh = now
}

// Record adds a completed request's spend to every matching budget.
func (e *Engine) Record(agent auth.Agent, tokens int64, usd float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.budgets {
		if t.matches(agent) {
			t.deltaUSD += usd
			t.deltaTokens += tokens
		}
	}
}

// States returns a point-in-time snapshot of every budget's state plus any
// killed agents that have no backing budget. It is a pure read under the same
// lock the rest of the engine uses; counters are whatever the last refresh or
// live increment left them at (no store round-trip on the scrape path).
func (e *Engine) States() []GaugeState {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]GaugeState, 0, len(e.budgets)+len(e.killed))
	killedWithBudget := map[string]bool{}
	for _, t := range e.budgets {
		s := GaugeState{Dimension: t.dim, Target: t.value, Ratio: t.ratio(), HasRatio: true}
		switch {
		case t.dim == "agent" && e.killed[t.value]:
			s.State = StateBlocked
			killedWithBudget[t.value] = true
		case t.over():
			if t.cfg.Action == "throttle" {
				s.State = StateThrottled
			} else {
				s.State = StateBlocked
			}
		case t.nearLimit():
			s.State = StateAlert
		default:
			s.State = StateOK
		}
		out = append(out, s)
	}
	for agent := range e.killed {
		if killedWithBudget[agent] {
			continue
		}
		out = append(out, GaugeState{Dimension: "agent", Target: agent, State: StateBlocked})
	}
	return out
}

// Kill blocks all requests from the agent immediately and persists the
// state so a restart doesn't revive it.
func (e *Engine) Kill(ctx context.Context, agent string) error {
	if err := e.store.Kill(ctx, agent, e.now()); err != nil {
		return err
	}
	e.mu.Lock()
	e.killed[agent] = true
	e.mu.Unlock()
	e.logger.Warn("kill switch engaged", "agent", agent)
	return nil
}

// Revive lifts the kill switch for the agent.
func (e *Engine) Revive(ctx context.Context, agent string) error {
	if err := e.store.Revive(ctx, agent); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.killed, agent)
	e.mu.Unlock()
	e.logger.Info("kill switch lifted", "agent", agent)
	return nil
}

// Kills lists currently killed agents.
func (e *Engine) Kills() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	agents := make([]string, 0, len(e.killed))
	for a := range e.killed {
		agents = append(agents, a)
	}
	return agents
}
