package web

import (
	"context"
	"net/http"
	"strings"

	"xray-checker/reachability"
)

// ReachabilityService is the admin API's view of the sweep. It can read the
// matrix and ask for a pass; it deliberately has no way to change a cell, so
// nothing reachable from HTTP can write a verdict the agents did not produce.
type ReachabilityService interface {
	Enabled() bool
	Snapshot() reachability.View
	SweepOnce(context.Context) reachability.Summary
	SweepNode(context.Context, string) reachability.Summary
}

// reachabilitySweepRequest narrows a sweep to one node. An empty body sweeps
// everything, which is what the plain "Sweep now" button sends.
type reachabilitySweepRequest struct {
	StableID string `json:"stableId"`
}

// AdminReachabilityHandler serves the matrix, and accepts a request to sweep now.
//
// The sweep is started detached rather than awaited: a full pass visits every
// node from every agent and routinely outlives an HTTP request. The response
// carries the snapshot with Sweeping set, which is what the admin UI polls on.
func AdminReachabilityHandler(service ReachabilityService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			writeJSON(w, service.Snapshot())
		case http.MethodPost:
			if !service.Enabled() {
				writeError(w, "Reachability sweep is disabled", http.StatusServiceUnavailable)
				return
			}
			// The body is optional: an empty one is a full sweep. Only a
			// malformed body is worth refusing.
			var input reachabilitySweepRequest
			if request.ContentLength > 0 && !decodeProbeAgentJSON(w, request, &input) {
				return
			}
			stableID := strings.TrimSpace(input.StableID)
			// A second request while a pass is running is harmless: the sweeper
			// returns immediately with Skipped rather than starting a
			// concurrent sweep, so this cannot be used to pile work onto the
			// agents. Repeating a single-node recheck cannot manufacture a
			// confirmation either — the streak only advances once the checker's
			// own sample has moved on.
			go func() {
				if stableID == "" {
					service.SweepOnce(context.Background())
					return
				}
				service.SweepNode(context.Background(), stableID)
			}()
			writeJSON(w, service.Snapshot())
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
