package telegram

import (
	"strings"
	"testing"

	"xray-checker/speedtest"
)

func shortenedTransfer() speedtest.Result {
	return speedtest.Result{
		StableID:        "de-02",
		Name:            "Германия #2",
		Mbps:            5.79,
		DownloadedBytes: 21705523,
		RequestedBytes:  104857600,
		DurationMs:      30000,
		TTFBMs:          145,
		TimedOut:        true,
	}
}

// A shortened transfer used to arrive here as "❌ context deadline exceeded",
// which said nothing about the node and hid the rate the run had measured.
func TestShortenedTransferReportsItsRateInsteadOfATransportError(t *testing.T) {
	result := shortenedTransfer()

	for name, line := range map[string]string{
		"status":  speedResultStatusHTML(result, 100),
		"rich":    formatSpeedStatusRich(result, 100),
		"report":  formatSpeedResultHTML(result, 100),
		"history": formatSpeedHistoryLine(result, 100),
	} {
		if !strings.Contains(line, "5.79 Mbps") {
			t.Fatalf("%s line hides the measured rate: %q", name, line)
		}
		if !strings.Contains(line, "таймаут") {
			t.Fatalf("%s line does not say the transfer was cut short: %q", name, line)
		}
		if strings.Contains(line, "❌") || strings.Contains(strings.ToLower(line), "deadline") {
			t.Fatalf("%s line still reads as a technical failure: %q", name, line)
		}
	}
}

// Slow or not, it is never one of the nodes that are simply fine.
func TestShortenedTransferIsNeverHealthy(t *testing.T) {
	slow := shortenedTransfer()
	fast := shortenedTransfer()
	fast.StableID = "de-03"
	fast.Mbps = 500

	for _, result := range []speedtest.Result{slow, fast} {
		if class := speedResultClass(result, 100); class != speedClassSlow {
			t.Fatalf("class of a %.2f Mbps shortened transfer = %d, want %d", result.Mbps, class, speedClassSlow)
		}
		if healthy := healthySpeedResults([]speedtest.Result{result}, 100); len(healthy) != 0 {
			t.Fatalf("a shortened transfer must not be listed as a best result: %+v", healthy)
		}
		if issues := speedIssueResults([]speedtest.Result{result}, 100); len(issues) != 1 {
			t.Fatalf("a shortened transfer must need attention: %+v", issues)
		}
	}

	failed, slowCount := countSpeedIssues([]speedtest.Result{fast}, 100)
	if failed != 0 || slowCount != 1 {
		t.Fatalf("counts = failed %d, slow %d; want 0 and 1", failed, slowCount)
	}
}

// The alert waits on what the measurement actually showed: a shortened transfer
// under the threshold is a slowdown to confirm, not a technical failure.
func TestShortenedTransferConfirmsAsASlowdown(t *testing.T) {
	targets := speedConfirmationRetryTargets([]speedtest.Result{shortenedTransfer()}, 100)
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if targets[0].Reason != speedRetryReasonLowSpeed {
		t.Fatalf("reason = %q, want %q", targets[0].Reason, speedRetryReasonLowSpeed)
	}

	fast := shortenedTransfer()
	fast.Mbps = 500
	targets = speedConfirmationRetryTargets([]speedtest.Result{fast}, 100)
	if len(targets) != 1 || targets[0].Reason != speedRetryReasonTechnical {
		t.Fatalf("a shortened transfer above the threshold must wait as a technical failure: %+v", targets)
	}

	// History written before the flag existed still carries the raw error text,
	// and it has to keep behaving as it did.
	legacy := speedtest.Result{StableID: "de-02", Error: "context deadline exceeded"}
	targets = speedConfirmationRetryTargets([]speedtest.Result{legacy}, 100)
	if len(targets) != 1 || targets[0].Reason != speedRetryReasonTechnical {
		t.Fatalf("legacy deadline text must still schedule a technical retry: %+v", targets)
	}
}

func TestShortenedTransferIssueLineSpellsOutTheShortfall(t *testing.T) {
	lines := speedIssuesHTML([]speedtest.Result{shortenedTransfer()}, 100)
	if len(lines) != 1 {
		t.Fatalf("issue lines = %d, want 1: %#v", len(lines), lines)
	}
	for _, want := range []string{"5.79 Mbps", "таймаут", "30.0 с"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("issue line %q does not contain %q", lines[0], want)
		}
	}
}
