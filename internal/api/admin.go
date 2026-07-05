package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/opslync/tollgate/internal/budget"
)

// Admin serves the kill-switch endpoints, guarded by the admin key:
//
//	POST   /admin/agents/{agent}/kill   engage the kill switch
//	DELETE /admin/agents/{agent}/kill   lift it
//	GET    /admin/kills                 list killed agents
//
// knownAgents guards against killing a typo instead of a runaway agent.
func Admin(e *budget.Engine, adminKey string, knownAgents []string) http.Handler {
	known := make(map[string]bool, len(knownAgents))
	for _, a := range knownAgents {
		known[a] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/agents/{agent}/kill", func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		if !known[agent] {
			httpError(w, http.StatusNotFound, "unknown agent %q", agent)
			return
		}
		if err := e.Kill(r.Context(), agent); err != nil {
			httpError(w, http.StatusInternalServerError, "kill failed: %v", err)
			return
		}
		writeJSON(w, map[string]string{"agent": agent, "status": "killed"})
	})
	mux.HandleFunc("DELETE /admin/agents/{agent}/kill", func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		if err := e.Revive(r.Context(), agent); err != nil {
			httpError(w, http.StatusInternalServerError, "revive failed: %v", err)
			return
		}
		writeJSON(w, map[string]string{"agent": agent, "status": "active"})
	})
	mux.HandleFunc("GET /admin/kills", func(w http.ResponseWriter, r *http.Request) {
		kills := e.Kills()
		if kills == nil {
			kills = []string{}
		}
		writeJSON(w, map[string][]string{"killed": kills})
	})

	return adminAuth(adminKey, mux)
}

// adminAuth accepts the admin key via x-admin-key or Authorization: Bearer,
// compared in constant time.
func adminAuth(adminKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get("x-admin-key")
		if presented == "" {
			presented, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(adminKey)) != 1 {
			httpError(w, http.StatusUnauthorized, "invalid or missing admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck // best-effort response body
	json.NewEncoder(w).Encode(v)
}
