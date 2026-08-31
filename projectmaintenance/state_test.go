package projectmaintenance

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerPersistsProjectMaintenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project_state.json")
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	manager := NewManager(path)
	manager.now = func() time.Time { return now }

	snapshot, err := manager.Set(true)
	if err != nil {
		t.Fatalf("Set(true) error = %v", err)
	}
	if !snapshot.Enabled || !snapshot.Since.Equal(now) {
		t.Fatalf("Set(true) snapshot = %+v", snapshot)
	}

	reloaded := NewManager(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := reloaded.Snapshot(); !got.Enabled || !got.Since.Equal(now) {
		t.Fatalf("reloaded snapshot = %+v", got)
	}

	if _, err := reloaded.Set(false); err != nil {
		t.Fatalf("Set(false) error = %v", err)
	}
	final := NewManager(path)
	if err := final.Load(); err != nil {
		t.Fatalf("final Load() error = %v", err)
	}
	if got := final.Snapshot(); got.Enabled || !got.Since.IsZero() {
		t.Fatalf("final snapshot = %+v", got)
	}
}

func TestManagerRejectsUnsupportedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project_state.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"enabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewManager(path).Load(); err == nil {
		t.Fatal("Load() accepted unsupported state")
	}
}
