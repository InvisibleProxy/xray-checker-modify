package paneltelemetry

import (
	"testing"
	"time"
)

// The rules both surfaces follow: a fact the panel has no figure for is left
// out, "before" only when it says something, the reason only while the panel
// cannot reach the node, and uptime as a coarse age.
func TestViewAppliesTheSharedDisplayRules(t *testing.T) {
	view := Status{
		Name: "Germany-02", Connected: true, UsersOnline: 3, UsersOnlineBefore: 41, HasBefore: true,
		MemoryUsedPercent: 87.6, Load1: 1.254, CPUs: 2, RxMbps: 99.6, TxMbps: 0.4,
		XrayUptime: 12*24*time.Hour + 5*time.Hour, Message: "stale reason",
	}.View()
	if view.Link != LinkConnected || view.Message != "" {
		t.Fatalf("link = %q, message = %q", view.Link, view.Message)
	}
	if view.UsersOnlineBefore == nil || *view.UsersOnlineBefore != 41 {
		t.Fatalf("before = %v, want 41", view.UsersOnlineBefore)
	}
	if view.MemoryUsedPercent != 88 || view.Load1 != 1.25 || view.CPUs != 2 || view.RxMbps != 100 || view.TxMbps != 0 {
		t.Fatalf("rounded facts = %+v", view)
	}
	if view.XrayUptime == nil || *view.XrayUptime != (Uptime{Value: 12, Unit: UptimeDays}) {
		t.Fatalf("uptime = %+v", view.XrayUptime)
	}

	quiet := Status{Connected: true, UsersOnline: 5, UsersOnlineBefore: 5, HasBefore: true, CPUs: 2, XrayUptime: 90 * time.Minute}.View()
	if quiet.UsersOnlineBefore != nil || quiet.CPUs != 0 || quiet.MemoryUsedPercent != 0 {
		t.Fatalf("facts without a figure were kept: %+v", quiet)
	}
	if *quiet.XrayUptime != (Uptime{Value: 1, Unit: UptimeHours}) {
		t.Fatalf("uptime = %+v", quiet.XrayUptime)
	}

	for _, test := range []struct {
		status Status
		link   string
		reason string
	}{
		{Status{Disabled: true, Connected: true}, LinkDisabled, ""},
		{Status{Connecting: true}, LinkConnecting, ""},
		{Status{Message: "connect ECONNREFUSED"}, LinkDisconnected, "connect ECONNREFUSED"},
	} {
		view := test.status.View()
		if view.Link != test.link || view.Message != test.reason {
			t.Errorf("%+v: link %q (%q), want %q (%q)", test.status, view.Link, view.Message, test.link, test.reason)
		}
	}
}
