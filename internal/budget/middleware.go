package budget

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/opslync/tollgate/internal/auth"
)

// Middleware enforces budget decisions between authentication and the proxy.
// Error bodies use the Anthropic error shape so agent SDKs handle them
// natively — in particular, 429 rate_limit_error triggers SDK backoff.
func (e *Engine) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, ok := auth.FromContext(r.Context())
		if !ok {
			// Open mode (no agents configured): budgets target identities,
			// so there is nothing to enforce against.
			next.ServeHTTP(w, r)
			return
		}

		d := e.Check(r.Context(), agent)
		switch d.Kind {
		case Allow:
			next.ServeHTTP(w, r)
		case Throttled:
			seconds := int(math.Ceil(d.RetryAfter.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "rate_limit_error",
				fmt.Sprintf("%s is over its budget (%s) and is throttled; retry in %ds",
					agent.Name, describeBudget(d), seconds))
		case BlockedBudget:
			writeError(w, http.StatusForbidden, "budget_exceeded",
				fmt.Sprintf("%s is blocked: %s; requests resume when spend ages out of the window",
					agent.Name, describeBudget(d)))
		case BlockedKilled:
			writeError(w, http.StatusForbidden, "agent_disabled",
				fmt.Sprintf("%s has been disabled by the kill switch; contact your Tollgate administrator",
					agent.Name))
		}
	})
}

func describeBudget(d Decision) string {
	b := d.Budget
	target := "agent " + b.Agent
	if b.Team != "" {
		target = "team " + b.Team
	}
	s := fmt.Sprintf("%s spent", target)
	if b.LimitUSD > 0 {
		s += fmt.Sprintf(" $%.4f of the $%.2f", d.SpendUSD, b.LimitUSD)
	} else {
		s += fmt.Sprintf(" %d of the %d token", d.SpendTokens, b.LimitTokens)
	}
	return s + fmt.Sprintf(" limit per %s", time.Duration(b.Window))
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // best-effort error body
	fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, errType, message)
}
