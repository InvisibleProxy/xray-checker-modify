package agentautomation

import (
	"testing"
	"time"

	"xray-checker/speedtest"
)

// Automatic agent work covers the service this deployment runs. A node from a
// subscription an operator added in the panel is measured and reported like any
// other, but it never starts an agent by itself: the only agent that visits it
// is one an operator asked for from the Reachability tab.
func TestSpeedAutomationSkipsPanelAddedNodes(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 4,
		EnvironmentSourced: func(stableID string) bool { return stableID != "node-added" },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-added", Error: "context deadline exceeded"},
		{StableID: "node-own", Error: "context deadline exceeded"},
	}}

	handles := coordinator.StartSpeedDiagnostics(report, 10)

	if len(handles) != 1 || handles["node-own"].SessionID == "" {
		t.Fatalf("handles = %+v, want a session for the environment node only", handles)
	}
	if _, diagnosed := handles["node-added"]; diagnosed {
		t.Fatal("a panel-added node started an automatic diagnostic")
	}
	if len(controller.requests) != 1 || controller.requests[0].StableID != "node-own" {
		t.Fatalf("automatic creates = %+v, want one for the environment node", controller.requests)
	}
}

// Without a gate every node is the environment's, which is what a deployment
// with no panel-added sources has.
func TestSpeedAutomationWithoutAGateDiagnosesEveryNode(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 4,
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded"},
		{StableID: "node-two", Error: "context deadline exceeded"},
	}}

	if handles := coordinator.StartSpeedDiagnostics(report, 10); len(handles) != 2 {
		t.Fatalf("handles = %+v, want a session for both nodes", handles)
	}
}
