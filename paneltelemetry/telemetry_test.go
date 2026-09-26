package paneltelemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSource struct {
	nodes []Node
	err   error
	calls int
}

func (f *fakeSource) GetNodes(context.Context) ([]Node, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]Node(nil), f.nodes...), nil
}

func testNode(online int) Node {
	return Node{
		UUID: "uuid-de2", Name: "Germany-02", Address: "31.59.178.215", IsConnected: true, UsersOnline: online, XrayUptime: 3600,
		System: &NodeSystem{
			Info: NodeSystemInfo{CPUs: 1, MemoryTotal: 2000},
			Stats: NodeSystemStats{
				MemoryUsed: 1760, LoadAvg: []float64{1.25},
				Interface: &NodeInterface{RxBytesPerSec: 12_500_000, TxBytesPerSec: 25_000_000},
			},
		},
	}
}

// The status is found by the address the subscription publishes, and it carries
// what the node looked like before the moment an alert asks about.
func TestNodeStatusMatchesByAddressAndRemembersTheOnlineCountBeforeAnOutage(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	source := &fakeSource{nodes: []Node{testNode(41)}}
	poller := NewPoller(source, Config{Now: func() time.Time { return now }})
	poller.Refresh(context.Background())

	outage := now.Add(30 * time.Second)
	now = now.Add(time.Minute)
	source.nodes = []Node{testNode(3)}
	poller.Refresh(context.Background())

	status, ok := poller.NodeStatus("31.59.178.215", outage)
	if !ok {
		t.Fatal("no status for the node's own address")
	}
	if status.UsersOnline != 3 || !status.HasBefore || status.UsersOnlineBefore != 41 {
		t.Fatalf("online = %d (before %d, %v), want 3 now and 41 before the outage", status.UsersOnline, status.UsersOnlineBefore, status.HasBefore)
	}
	if status.MemoryUsedPercent < 87.9 || status.MemoryUsedPercent > 88.1 || status.Load1 != 1.25 || status.CPUs != 1 ||
		status.RxMbps != 100 || status.TxMbps != 200 || status.XrayUptime != time.Hour {
		t.Fatalf("status = %+v", status)
	}
	if _, ok := poller.NodeStatus("198.51.100.1", outage); ok {
		t.Fatal("a node the panel does not know was matched")
	}
}

// A published IP the panel lists among the node's addresses matches too, even
// when the panel reaches the node under another address.
func TestNodeStatusMatchesAnyOfTheNodeAddresses(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	node := testNode(5)
	node.Address = "de2.internal.example"
	node.IPs = []NodeIP{{IP: "31.59.178.215"}}
	poller := NewPoller(&fakeSource{nodes: []Node{node}}, Config{Now: func() time.Time { return now }})
	poller.Refresh(context.Background())
	if _, ok := poller.NodeStatus("31.59.178.215", time.Time{}); !ok {
		t.Fatal("the node's listed IP did not match")
	}
}

// A snapshot too old to describe the node now is not quoted as if it were.
func TestAStaleSnapshotIsNotQuoted(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	poller := NewPoller(&fakeSource{nodes: []Node{testNode(5)}}, Config{Now: func() time.Time { return now }})
	poller.Refresh(context.Background())
	now = now.Add(time.Hour)
	if _, ok := poller.NodeStatus("31.59.178.215", time.Time{}); ok {
		t.Fatal("an hour-old snapshot was quoted")
	}
}

// A refused token is reported once and not retried every minute: the missing
// scope does not fix itself between two polls.
func TestAForbiddenTokenIsReportedOnceAndBackedOff(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	source := &fakeSource{err: ErrForbidden}
	var reported []error
	poller := NewPoller(source, Config{Now: func() time.Time { return now }, OnError: func(err error) { reported = append(reported, err) }})
	poller.Refresh(context.Background())
	now = now.Add(time.Minute)
	poller.Refresh(context.Background())
	if source.calls != 1 || len(reported) != 1 || !errors.Is(reported[0], ErrForbidden) {
		t.Fatalf("calls = %d, reported = %v; want one call and one report", source.calls, reported)
	}
	if state := poller.State(); state.Error == "" {
		t.Fatalf("state = %+v, want the error visible", state)
	}
	now = now.Add(31 * time.Minute)
	poller.Refresh(context.Background())
	if source.calls != 2 {
		t.Fatalf("calls after the backoff = %d, want a retry", source.calls)
	}
}
