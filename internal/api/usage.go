// Package api serves Tollgate's own HTTP endpoints (as opposed to proxied
// provider traffic).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/opslync/tollgate/internal/store"
)

type usageResponse struct {
	GroupBy string      `json:"group_by"`
	Since   time.Time   `json:"since"`
	Until   time.Time   `json:"until"`
	Rows    []store.Row `json:"rows"`
}

// UsageHandler answers GET /usage. Query parameters:
//
//	group_by  agent (default) | team | namespace | model | provider
//	since     RFC3339 timestamp or relative duration like 24h (default 24h)
//	until     RFC3339 timestamp (default now)
//	agent     optional exact-match filter
//	model     optional exact-match filter
func UsageHandler(st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		q := r.URL.Query()

		since, err := parseTime(q.Get("since"), now, now.Add(-24*time.Hour))
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid since: %v", err)
			return
		}
		until, err := parseTime(q.Get("until"), now, now)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid until: %v", err)
			return
		}

		opts := store.AggregateOptions{
			GroupBy: q.Get("group_by"),
			Since:   since,
			Until:   until,
			Agent:   q.Get("agent"),
			Model:   q.Get("model"),
		}
		rows, err := st.Aggregate(r.Context(), opts)
		if errors.Is(err, store.ErrInvalidGroupBy) {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, "query failed: %v", err)
			return
		}

		if opts.GroupBy == "" {
			opts.GroupBy = "agent"
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // best-effort response body
		json.NewEncoder(w).Encode(usageResponse{
			GroupBy: opts.GroupBy,
			Since:   since,
			Until:   until,
			Rows:    rows,
		})
	})
}

// parseTime accepts an RFC3339 timestamp or a relative Go duration ("24h",
// "30m") interpreted as now minus that duration.
func parseTime(s string, now, fallback time.Time) (time.Time, error) {
	if s == "" {
		return fallback, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a duration nor RFC3339", s)
	}
	return t, nil
}

func httpError(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // best-effort error body
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}
