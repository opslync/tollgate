package budget

import (
	"context"
	"testing"
	"time"

	"github.com/opslync/tollgate/internal/config"
)

func stateFor(states []GaugeState, dim, target string) (GaugeState, bool) {
	for _, s := range states {
		if s.Dimension == dim && s.Target == target {
			return s, true
		}
	}
	return GaugeState{}, false
}

func TestStatesUnderBudget(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 1.0) // 10% of $10

	s, ok := stateFor(e.States(), "agent", "support-bot")
	if !ok {
		t.Fatal("no state for support-bot")
	}
	if s.State != StateOK {
		t.Errorf("state = %v, want StateOK", s.State)
	}
	if !s.HasRatio || s.Ratio < 0.09 || s.Ratio > 0.11 {
		t.Errorf("ratio = %v (hasRatio %v), want ~0.1", s.Ratio, s.HasRatio)
	}
}

func TestStatesAlert(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 8.5) // 85% -> over alert_at 0.8, under limit

	s, _ := stateFor(e.States(), "agent", "support-bot")
	if s.State != StateAlert {
		t.Errorf("state = %v, want StateAlert", s.State)
	}
}

func TestStatesBlocked(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 11)

	s, _ := stateFor(e.States(), "agent", "support-bot")
	if s.State != StateBlocked {
		t.Errorf("state = %v, want StateBlocked", s.State)
	}
	if s.Ratio < 1 {
		t.Errorf("ratio = %v, want >= 1", s.Ratio)
	}
}

func TestStatesThrottled(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "throttle")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 11)

	s, _ := stateFor(e.States(), "agent", "support-bot")
	if s.State != StateThrottled {
		t.Errorf("state = %v, want StateThrottled", s.State)
	}
}

func TestStatesKilledWithBudget(t *testing.T) {
	e, _, _ := newTestEngine(t, []config.Budget{usdBudget("support-bot", 10, "block")})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 1.0)
	if err := e.Kill(context.Background(), "support-bot"); err != nil {
		t.Fatal(err)
	}

	s, _ := stateFor(e.States(), "agent", "support-bot")
	if s.State != StateBlocked {
		t.Errorf("state = %v, want StateBlocked (kill overrides an under-budget agent)", s.State)
	}
	if !s.HasRatio {
		t.Error("a killed agent that has a budget should still report its ratio")
	}
	count := 0
	for _, gs := range e.States() {
		if gs.Target == "support-bot" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("support-bot series = %d, want exactly 1 (no double-emit)", count)
	}
}

func TestStatesKilledWithoutBudget(t *testing.T) {
	e, _, _ := newTestEngine(t, nil)
	if err := e.Kill(context.Background(), "rogue-bot"); err != nil {
		t.Fatal(err)
	}

	s, ok := stateFor(e.States(), "agent", "rogue-bot")
	if !ok {
		t.Fatal("no state for killed rogue-bot")
	}
	if s.State != StateBlocked || s.HasRatio {
		t.Errorf("state = %v hasRatio = %v, want StateBlocked with no ratio", s.State, s.HasRatio)
	}
}

func TestStatesTeamDimension(t *testing.T) {
	b := config.Budget{Team: "support", Window: config.Duration(time.Hour), LimitUSD: 10, AlertAt: 0.8, Action: "block"}
	e, _, _ := newTestEngine(t, []config.Budget{b})
	prime(t, e, supportBot)
	e.Record(supportBot, 0, 1.0)

	s, ok := stateFor(e.States(), "team", "support")
	if !ok {
		t.Fatal("no team state")
	}
	if s.State != StateOK {
		t.Errorf("state = %v, want StateOK", s.State)
	}
}
