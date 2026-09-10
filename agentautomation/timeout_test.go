package agentautomation

import (
	"testing"

	"xray-checker/diagnostics"
	"xray-checker/speedtest"
)

// A measurement the deadline cut short used to reach the agent as a technical
// failure, because it carried a transport error. That asked the wrong question:
// the run had measured the node, and what settles a slow node is another rate
// to hold against the threshold, not a yes/no on whether the transfer broke.
func TestSpeedAutomationOutcomeReadsShortenedTransfersByTheirRate(t *testing.T) {
	tests := []struct {
		name        string
		result      speedtest.Result
		threshold   float64
		wantOutcome string
		wantStart   bool
	}{
		{
			name:        "shortened transfer below the threshold",
			result:      speedtest.Result{StableID: "node-1", Mbps: 5.79, DownloadedBytes: 21 * 1024 * 1024, TimedOut: true},
			threshold:   100,
			wantOutcome: diagnostics.AutomationOutcomeLowSpeed,
			wantStart:   true,
		},
		{
			// Fast and still unable to deliver the requested amount in time:
			// there is no rate to argue about, so yes/no is all that is left.
			name:        "shortened transfer above the threshold",
			result:      speedtest.Result{StableID: "node-2", Mbps: 500, DownloadedBytes: 21 * 1024 * 1024, TimedOut: true},
			threshold:   100,
			wantOutcome: diagnostics.AutomationOutcomeTechnical,
			wantStart:   true,
		},
		{
			name:        "shortened transfer with no threshold in force",
			result:      speedtest.Result{StableID: "node-3", Mbps: 5.79, DownloadedBytes: 21 * 1024 * 1024, TimedOut: true},
			threshold:   0,
			wantOutcome: diagnostics.AutomationOutcomeTechnical,
			wantStart:   true,
		},
		{
			name:        "transport failure",
			result:      speedtest.Result{StableID: "node-4", Error: "connection reset by peer"},
			threshold:   100,
			wantOutcome: diagnostics.AutomationOutcomeTechnical,
			wantStart:   true,
		},
		{
			name:      "complete transfer above the threshold",
			result:    speedtest.Result{StableID: "node-5", Mbps: 500, DownloadedBytes: 100 * 1024 * 1024},
			threshold: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, start := speedAutomationOutcome(tt.result, tt.threshold)
			if start != tt.wantStart {
				t.Fatalf("start = %v, want %v", start, tt.wantStart)
			}
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
		})
	}
}

// The shortfall orders the queue, and a shortened transfer now competes in it
// with the rate it measured rather than sitting at zero as a technical failure.
func TestSpeedAutomationCandidatesRankShortenedTransfersByShortfall(t *testing.T) {
	candidates := speedAutomationCandidates([]speedtest.Result{
		{StableID: "mild", Mbps: 80, DownloadedBytes: 21 * 1024 * 1024, TimedOut: true},
		{StableID: "severe", Mbps: 5.79, DownloadedBytes: 21 * 1024 * 1024, TimedOut: true},
	}, "schedule", 100)

	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	if candidates[0].stableID != "severe" {
		t.Fatalf("deepest shortfall must come first, got %q", candidates[0].stableID)
	}
	if candidates[0].observedMbps != 5.79 {
		t.Fatalf("observedMbps = %v, want the rate the run measured", candidates[0].observedMbps)
	}
	if candidates[0].outcome != diagnostics.AutomationOutcomeLowSpeed {
		t.Fatalf("outcome = %q, want %q", candidates[0].outcome, diagnostics.AutomationOutcomeLowSpeed)
	}
}
