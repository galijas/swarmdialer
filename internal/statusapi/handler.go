// Package statusapi serves an orchestrator.RunState as JSON over HTTP —
// the live-status backend for whatever GUI ends up on top of it. Kept
// deliberately generic (one read-only snapshot endpoint) so it doesn't
// presume anything about page layout or interaction design.
package statusapi

import (
	"encoding/json"
	"net/http"

	"swarmdialer/internal/orchestrator"
)

// Handler serves the current Snapshot of state as JSON on every request.
// Mount it wherever the future GUI's live-status fetch will hit, e.g.:
//
//	http.Handle("/api/status", statusapi.Handler(state))
func Handler(state *orchestrator.RunState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Internal tool, no auth boundary to protect here; allow the future
		// GUI to fetch this even if served from a different port/origin
		// during development.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if err := json.NewEncoder(w).Encode(state.Snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
