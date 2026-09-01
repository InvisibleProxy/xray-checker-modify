package web

import (
	"testing"
	"time"
)

func TestRefreshTrackerReportsThePhaseInFlightAndItsTimings(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewSubscriptionRefreshTracker()
	tracker.now = func() time.Time { return now }

	tracker.Begin("manual")
	tracker.Phase(RefreshPhaseFetching)
	now = now.Add(40 * time.Second)

	// Mid-flight the caller needs to know what is taking the time, not a total.
	running := tracker.Snapshot()
	if !running.Running || running.Phase != RefreshPhaseFetching {
		t.Fatalf("in-flight snapshot = %+v", running)
	}
	if running.ElapsedMs != 40_000 {
		t.Errorf("ElapsedMs = %d, want the time spent so far", running.ElapsedMs)
	}
	if len(running.Phases) != 0 {
		t.Errorf("an unfinished phase was already reported as complete: %+v", running.Phases)
	}

	tracker.Phase(RefreshPhaseApplying)
	now = now.Add(2 * time.Second)
	tracker.Done(false)

	finished := tracker.Snapshot()
	if finished.Running || finished.Failed {
		t.Fatalf("finished snapshot = %+v", finished)
	}
	if len(finished.Phases) != 2 {
		t.Fatalf("phases = %+v, want both recorded", finished.Phases)
	}
	if finished.Phases[0].Phase != RefreshPhaseFetching || finished.Phases[0].DurationMs != 40_000 {
		t.Errorf("fetching phase = %+v, want the 40s it actually took", finished.Phases[0])
	}
	if finished.Phases[1].Phase != RefreshPhaseApplying || finished.Phases[1].DurationMs != 2_000 {
		t.Errorf("applying phase = %+v", finished.Phases[1])
	}
	if finished.ElapsedMs != 42_000 {
		t.Errorf("ElapsedMs = %d, want the whole run", finished.ElapsedMs)
	}
}

// A new run must not show the previous run's phases: they would read as if they
// described the refresh currently in flight.
func TestRefreshTrackerDiscardsThePreviousRun(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewSubscriptionRefreshTracker()
	tracker.now = func() time.Time { return now }

	tracker.Begin("scheduled")
	tracker.Phase(RefreshPhaseFetching)
	now = now.Add(time.Second)
	tracker.Done(true)

	tracker.Begin("manual")
	snapshot := tracker.Snapshot()
	if len(snapshot.Phases) != 0 || snapshot.Failed || snapshot.Source != "manual" {
		t.Fatalf("snapshot after a new Begin = %+v", snapshot)
	}
}

func TestRefreshTrackerToleratesANilReceiver(t *testing.T) {
	var tracker *SubscriptionRefreshTracker
	tracker.Begin("manual")
	tracker.Phase(RefreshPhaseFetching)
	tracker.Done(false)
	if got := tracker.Snapshot(); got.Running || len(got.Phases) != 0 {
		t.Fatalf("nil tracker snapshot = %+v", got)
	}
}
