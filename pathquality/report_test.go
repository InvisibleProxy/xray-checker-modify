package pathquality

import (
	"testing"
	"time"

	"xray-checker/speedtest"
)

func at(day, hour int) time.Time {
	return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC)
}

// Hours are read in the report's zone and the peak in the clients' zone, so an
// operator seven hours east still sees the Moscow evening called the peak.
func TestBuildBucketsByHourAndSeparatesThePeak(t *testing.T) {
	moscow := time.FixedZone("MSK", 3*3600)
	krasnoyarsk := time.FixedZone("KRAT", 7*3600)
	results := []speedtest.Result{
		// 17:00 UTC is 20:00 in Moscow: the peak. Two bad, one fine.
		{StableID: "node", CheckedAt: at(20, 17), Mbps: 3, LowSpeedThresholdMbps: 100, URL: "http://speedtest.ams1.nl.leaseweb.net/100mb.bin",
			AgentDiagnostic: &speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticPathLimited}},
		{StableID: "node", CheckedAt: at(21, 17), Error: "timeout"},
		{StableID: "node", CheckedAt: at(22, 17), Mbps: 300, LowSpeedThresholdMbps: 100},
		// 05:00 UTC is 08:00 in Moscow: off-peak, fine.
		{StableID: "node", CheckedAt: at(20, 5), Mbps: 400, LowSpeedThresholdMbps: 100},
		// A confirmation re-run after the bad one is left out.
		{StableID: "node", CheckedAt: at(20, 17).Add(30 * time.Minute), Mbps: 2, LowSpeedThresholdMbps: 100, Source: speedtest.ConfirmationRetrySource},
		// Outside the window.
		{StableID: "node", CheckedAt: at(1, 17), Mbps: 1, LowSpeedThresholdMbps: 100},
	}
	report := Build([]NodeInput{{StableID: "node", Name: "Нидерланды #3", Results: results}}, Options{
		From: at(10, 0), To: at(25, 0), Location: krasnoyarsk, PeakLocation: moscow, PeakStart: 18, PeakEnd: 24,
	})
	if len(report.Nodes) != 1 {
		t.Fatalf("nodes = %d", len(report.Nodes))
	}
	row := report.Nodes[0]
	// 17:00 UTC is midnight in Krasnoyarsk.
	if cell := row.Hours[0]; cell.Runs != 3 || cell.Bad != 2 || cell.MedianMbps != 151.5 {
		t.Fatalf("midnight KRAT cell = %+v, want 3 runs, 2 bad, median of the two rates", cell)
	}
	if row.Peak.Runs != 3 || row.Peak.Bad != 2 || row.OffPeak.Runs != 1 || row.OffPeak.Bad != 0 {
		t.Fatalf("peak = %+v, off-peak = %+v", row.Peak, row.OffPeak)
	}
	if row.WorstHour != 0 {
		t.Fatalf("worst hour = %d, want the one with the bad runs", row.WorstHour)
	}
	if row.Verdicts[speedtest.AgentDiagnosticPathLimited] != 1 || row.Servers["speedtest.ams1.nl.leaseweb.net"] != 1 {
		t.Fatalf("verdicts = %+v, servers = %+v", row.Verdicts, row.Servers)
	}
	if report.Excluded != 1 {
		t.Fatalf("excluded = %d, want the one confirmation re-run", report.Excluded)
	}
	if report.Fleet[0].Runs != 3 || report.Fleet[0].Bad != 2 {
		t.Fatalf("fleet midnight = %+v", report.Fleet[0])
	}
}

// A window that wraps past midnight still counts the hours after it.
func TestPeakWindowMayWrapPastMidnight(t *testing.T) {
	for hour, want := range map[int]bool{21: true, 23: true, 0: true, 1: true, 2: false, 12: false} {
		if got := inPeak(hour, 21, 2); got != want {
			t.Errorf("inPeak(%d, 21, 2) = %v, want %v", hour, got, want)
		}
	}
	if !inPeak(23, 18, 24) || inPeak(0, 18, 24) {
		t.Fatal("an end of 24 must mean midnight")
	}
}

// The ranking puts the worst evening first and a node without evening
// measurements last, rather than calling it perfect.
func TestRankedOrdersByPeakBadShare(t *testing.T) {
	report := Report{Nodes: []NodeReport{
		{Name: "fine", Peak: Window{Runs: 10, Bad: 0}},
		{Name: "unmeasured"},
		{Name: "worst", Peak: Window{Runs: 10, Bad: 6, BadShare: 0.6}},
		{Name: "middle", Peak: Window{Runs: 10, Bad: 2, BadShare: 0.2}},
	}}
	ranked := report.Ranked()
	order := []string{ranked[0].Name, ranked[1].Name, ranked[2].Name, ranked[3].Name}
	if order[0] != "worst" || order[1] != "middle" || order[2] != "fine" || order[3] != "unmeasured" {
		t.Fatalf("order = %v", order)
	}
}

func TestBadCoversEveryFlaggedMeasurement(t *testing.T) {
	for _, test := range []struct {
		name   string
		result speedtest.Result
		want   bool
	}{
		{"healthy", speedtest.Result{Mbps: 200, LowSpeedThresholdMbps: 100}, false},
		{"slow", speedtest.Result{Mbps: 50, LowSpeedThresholdMbps: 100}, true},
		{"slow against the fallback threshold", speedtest.Result{Mbps: 50}, true},
		{"timed out above the threshold", speedtest.Result{Mbps: 150, LowSpeedThresholdMbps: 100, TimedOut: true}, true},
		{"failed", speedtest.Result{Error: "EOF"}, true},
		{"offline", speedtest.Result{Offline: true}, true},
	} {
		if got := Bad(test.result, 100); got != test.want {
			t.Errorf("%s: Bad = %v, want %v", test.name, got, test.want)
		}
	}
}
