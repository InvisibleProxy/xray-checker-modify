package backup

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAutomaticSchedulerKeepsOneBackupPerDayAndLastSevenDays(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	writeTestFile(t, filepath.Join(dataDir, "node_registry.json"), []byte(`{"version":1,"nodes":{}}`))

	current := time.Date(2026, time.July, 1, 0, 5, 0, 0, time.UTC)
	creator := NewCreator(dataDir, "test")
	creator.now = func() time.Time { return current }
	scheduler := NewAutomaticScheduler(creator, backupDir)
	scheduler.now = func() time.Time { return current }

	first, err := scheduler.RunOnce()
	if err != nil {
		t.Fatalf("create first automatic backup: %v", err)
	}
	if !first.Created {
		t.Fatal("first daily backup was not created")
	}
	duplicate, err := scheduler.RunOnce()
	if err != nil {
		t.Fatalf("check duplicate daily backup: %v", err)
	}
	if duplicate.Created {
		t.Fatal("second backup was created on the same UTC day")
	}

	for day := 1; day < 9; day++ {
		current = current.Add(24 * time.Hour)
		result, err := scheduler.RunOnce()
		if err != nil {
			t.Fatalf("create automatic backup for day %d: %v", day, err)
		}
		if !result.Created {
			t.Fatalf("automatic backup for day %d was skipped", day)
		}
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("list automatic backups: %v", err)
	}
	if len(entries) != maxAutomaticFiles {
		t.Fatalf("expected %d automatic backups, got %d", maxAutomaticFiles, len(entries))
	}
	cutoff := current.Add(-automaticMaxAge)
	for _, entry := range entries {
		createdAt, ok := parseAutomaticArchiveName(entry.Name())
		if !ok {
			t.Fatalf("unexpected automatic backup filename: %s", entry.Name())
		}
		if createdAt.Before(cutoff) {
			t.Fatalf("expired automatic backup was retained: %s", entry.Name())
		}
		archivePath := filepath.Join(backupDir, entry.Name())
		archive, err := zip.OpenReader(archivePath)
		if err != nil {
			t.Fatalf("open automatic backup %s: %v", entry.Name(), err)
		}
		if err := archive.Close(); err != nil {
			t.Fatalf("close automatic backup %s: %v", entry.Name(), err)
		}
	}
}

func TestAutomaticSchedulerIgnoresUnrelatedFiles(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatalf("create backup directory: %v", err)
	}
	writeTestFile(t, filepath.Join(dataDir, "node_registry.json"), []byte(`{"version":1,"nodes":{}}`))
	writeTestFile(t, filepath.Join(backupDir, "notes.txt"), []byte("keep"))

	if _, err := NewAutomaticScheduler(NewCreator(dataDir, "test"), backupDir).RunOnce(); err != nil {
		t.Fatalf("create automatic backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "notes.txt")); err != nil {
		t.Fatalf("unrelated backup file was removed: %v", err)
	}
}

func TestAutomaticSchedulerCreatesBackupWhenStarted(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(dataDir, "backups")
	writeTestFile(t, filepath.Join(dataDir, "node_registry.json"), []byte(`{"version":1,"nodes":{}}`))

	scheduler := NewAutomaticScheduler(NewCreator(dataDir, "test"), backupDir)
	scheduler.Start()
	scheduler.Stop()

	archives, err := scheduler.archives()
	if err != nil {
		t.Fatalf("list automatic backups: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected one startup backup, got %d", len(archives))
	}
}
