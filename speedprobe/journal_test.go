package speedprobe

import (
	"sync"
	"testing"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/speedtest"
	"xray-checker/verdictlog"
)

type fakeJournal struct {
	mu      sync.Mutex
	entries []verdictlog.Entry
}

func (f *fakeJournal) Record(entry verdictlog.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return nil
}

// Only the final answer is journaled, with both rates and both servers, so a
// later reader can see why the verdict came out the way it did. A probe still
// running when the wait ends has no verdict and is left out.
func TestRecorderJournalsTheFinalVerdictOnly(t *testing.T) {
	checkedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	coordinator := &fakeCoordinator{
		enabled: true,
		handles: map[string]agentautomation.Handle{
			"node-1": {StableID: "node-1", SessionID: "diag-one"},
			"node-2": {StableID: "node-2", SessionID: "diag-two"},
		},
		awaited: map[string]speedtest.AgentDiagnostic{
			"node-1": {
				State: speedtest.AgentDiagnosticPathLimited, SessionID: "diag-one", AgentName: "DE-01",
				Task: &speedtest.AgentProbeTask{Kind: "speed_fallback", Outcome: "low_speed", ThresholdMbps: 100, SpeedServerID: "leaseweb-amsterdam-ams1"},
				Observation: &speedtest.AgentProbeObservation{
					Status: "online", SpeedServerID: "leaseweb-amsterdam-ams1",
					Throughput: &speedtest.AgentProbeThroughput{Mbps: 480},
				},
			},
			"node-2": {State: speedtest.AgentDiagnosticRunning, SessionID: "diag-two"},
		},
	}
	journal := &fakeJournal{}
	recorder := New(Config{Journal: journal}, coordinator, &fakeHistory{})
	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Name: "Нидерланды #3", Mbps: 3, CheckedAt: checkedAt},
		{StableID: "node-2", Name: "Германия #2", Mbps: 20, CheckedAt: checkedAt},
	}})

	if len(journal.entries) != 1 {
		t.Fatalf("journal = %+v, want only the finished probe", journal.entries)
	}
	entry := journal.entries[0]
	if entry.Source != verdictlog.SourceSpeed || entry.StableID != "node-1" || entry.Node != "Нидерланды #3" ||
		entry.Verdict != speedtest.AgentDiagnosticPathLimited || entry.LocalMbps != 3 || entry.AgentMbps != 480 ||
		entry.ThresholdMbps != 100 || entry.LocalServerID != "leaseweb-amsterdam-ams1" || entry.AgentServerID != "leaseweb-amsterdam-ams1" ||
		entry.Outcome != "low_speed" || entry.AgentName != "DE-01" || !entry.LocalAt.Equal(checkedAt) {
		t.Fatalf("journal entry = %+v", entry)
	}
}
