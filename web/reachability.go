package web

import (
	"context"
	"net/http"

	"xray-checker/reachability"
)

// ReachabilityService is the admin API's view of the sweep. It can read the
// matrix and ask for a pass; it deliberately has no way to change a cell, so
// nothing reachable from HTTP can write a verdict the agents did not produce.
type ReachabilityService interface {
	Enabled() bool
	Snapshot() reachability.View
	SweepOnce(context.Context) reachability.Summary
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
			// A second request while a pass is running is harmless: SweepOnce
			// returns immediately with Skipped rather than starting a
			// concurrent sweep, so this cannot be used to pile work onto the
			// agents.
			go service.SweepOnce(context.Background())
			writeJSON(w, service.Snapshot())
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
