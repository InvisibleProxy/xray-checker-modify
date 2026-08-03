package backup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestorerStagesAndAppliesBackupOnRestart(t *testing.T) {
	sourceDir := t.TempDir()
	writeTestFile(t, filepath.Join(sourceDir, "node_registry.json"), []byte(`{"version":1,"nodes":{"new":{}}}`))
	writeTestFile(t, filepath.Join(sourceDir, "telegram_config.json"), []byte(`{
  "enabled": true,
  "botToken": "must-not-be-restored",
  "chatId": "must-not-be-restored",
  "speedReportsEnabled": true
}`))

	createdAt := time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC)
	creator := NewCreator(sourceDir, "source-version")
	creator.now = func() time.Time { return createdAt }
	var archive bytes.Buffer
	if _, err := creator.Create(&archive); err != nil {
		t.Fatalf("create source backup: %v", err)
	}

	targetDir := t.TempDir()
	oldRegistry := []byte(`{"version":1,"nodes":{"old":{}}}`)
	writeTestFile(t, filepath.Join(targetDir, "node_registry.json"), oldRegistry)
	writeTestFile(t, filepath.Join(targetDir, "speedtest_schedule.json"), []byte(`{"enabled":true}`))
	if err := os.MkdirAll(filepath.Join(targetDir, "backups"), 0700); err != nil {
		t.Fatalf("create automatic backup directory: %v", err)
	}
	writeTestFile(t, filepath.Join(targetDir, "backups", "keep.zip"), []byte("keep"))

	result, err := NewRestorer(targetDir).Stage(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatalf("stage restore: %v", err)
	}
	if !result.RestartRequired || !result.SourceCreatedAt.Equal(createdAt) || result.SourceAppVersion != "source-version" {
		t.Fatalf("unexpected restore result: %+v", result)
	}
	if current, err := os.ReadFile(filepath.Join(targetDir, "node_registry.json")); err != nil || !bytes.Equal(current, oldRegistry) {
		t.Fatalf("live data changed before restart: %s, %v", current, err)
	}
	writeTestFile(t, filepath.Join(targetDir, pendingRestoreDir, "telegram_config.json"), []byte(`{
  "enabled": true,
  "botToken": "injected-after-validation",
  "speedReportsEnabled": true
}`))

	applied, err := ApplyPendingRestore(targetDir)
	if err != nil {
		t.Fatalf("apply pending restore: %v", err)
	}
	if !applied {
		t.Fatal("expected pending restore to be applied")
	}
	if _, err := os.Stat(filepath.Join(targetDir, "speedtest_schedule.json")); !os.IsNotExist(err) {
		t.Fatalf("file absent from backup was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "backups", "keep.zip")); err != nil {
		t.Fatalf("automatic backups were modified by restore: %v", err)
	}

	registry, err := os.ReadFile(filepath.Join(targetDir, "node_registry.json"))
	if err != nil {
		t.Fatalf("read restored registry: %v", err)
	}
	if !bytes.Contains(registry, []byte(`"new"`)) || bytes.Contains(registry, []byte(`"old"`)) {
		t.Fatalf("unexpected restored registry: %s", registry)
	}
	telegramConfig, err := os.ReadFile(filepath.Join(targetDir, "telegram_config.json"))
	if err != nil {
		t.Fatalf("read restored Telegram config: %v", err)
	}
	if bytes.Contains(telegramConfig, []byte("must-not-be-restored")) || bytes.Contains(telegramConfig, []byte("injected-after-validation")) {
		t.Fatalf("sensitive Telegram values were restored: %s", telegramConfig)
	}

	applied, err = ApplyPendingRestore(targetDir)
	if err != nil || applied {
		t.Fatalf("pending restore was applied twice: applied=%v err=%v", applied, err)
	}
}

func TestRestorerRejectsArchiveWithInvalidDigest(t *testing.T) {
	data := []byte(`{"version":1,"nodes":{}}`)
	manifest := Manifest{
		FormatVersion: formatVersion,
		CreatedAt:     time.Now().UTC(),
		Files: []FileInfo{{
			Path:   "data/node_registry.json",
			Size:   int64(len(data)),
			SHA256: "invalid",
		}},
	}
	archive := buildRestoreTestArchive(t, manifest, map[string][]byte{"data/node_registry.json": data})

	_, err := NewRestorer(t.TempDir()).Stage(bytes.NewReader(archive), int64(len(archive)))
	if err == nil {
		t.Fatal("expected integrity verification error")
	}
}

func TestRestorerRejectsUnsupportedEntry(t *testing.T) {
	data := []byte(`{"value":true}`)
	digest := sha256.Sum256(data)
	manifest := Manifest{
		FormatVersion: formatVersion,
		CreatedAt:     time.Now().UTC(),
		Files: []FileInfo{{
			Path:   "data/unknown.json",
			Size:   int64(len(data)),
			SHA256: hex.EncodeToString(digest[:]),
		}},
	}
	archive := buildRestoreTestArchive(t, manifest, map[string][]byte{"data/unknown.json": data})

	_, err := NewRestorer(t.TempDir()).Stage(bytes.NewReader(archive), int64(len(archive)))
	if err == nil {
		t.Fatal("expected unsupported entry error")
	}
}

func TestRestorerRejectsDuplicateManifestKeys(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"formatVersion":1,"FormatVersion":1,"createdAt":"2026-08-03T00:00:00Z","files":[]}`
	if _, err := entry.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := NewRestorer(t.TempDir()).Stage(bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("manifest with case-folded duplicate keys was accepted")
	}
}

func TestAppliedRestoreCanBeRolledBackAfterLoaderFailure(t *testing.T) {
	archive := buildRegistryBackup(t, `{"version":1,"nodes":{"restored":{}}}`)
	targetDir := t.TempDir()
	original := []byte(`{"version":1,"nodes":{"original":{}}}`)
	writeTestFile(t, filepath.Join(targetDir, "node_registry.json"), original)

	if _, err := NewRestorer(targetDir).Stage(bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("stage restore: %v", err)
	}
	if applied, err := ApplyPendingRestore(targetDir); err != nil || !applied {
		t.Fatalf("ApplyPendingRestore() = %v, %v; want true, nil", applied, err)
	}
	if err := RollbackAppliedRestore(targetDir); err != nil {
		t.Fatalf("RollbackAppliedRestore() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(targetDir, "node_registry.json"))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("rolled-back registry = %s, %v; want original %s", got, err, original)
	}
	assertRestoreTransactionRemoved(t, targetDir)
}

func TestConfirmedRestoreKeepsInstalledState(t *testing.T) {
	restored := []byte(`{"version":1,"nodes":{"restored":{}}}`)
	archive := buildRegistryBackup(t, string(restored))
	targetDir := t.TempDir()
	writeTestFile(t, filepath.Join(targetDir, "node_registry.json"), []byte(`{"version":1,"nodes":{"original":{}}}`))

	if _, err := NewRestorer(targetDir).Stage(bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("stage restore: %v", err)
	}
	if applied, err := ApplyPendingRestore(targetDir); err != nil || !applied {
		t.Fatalf("ApplyPendingRestore() = %v, %v; want true, nil", applied, err)
	}
	if err := ConfirmAppliedRestore(targetDir); err != nil {
		t.Fatalf("ConfirmAppliedRestore() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(targetDir, "node_registry.json"))
	if err != nil || !bytes.Equal(got, restored) {
		t.Fatalf("confirmed registry = %s, %v; want restored %s", got, err, restored)
	}
	assertRestoreTransactionRemoved(t, targetDir)
	if recovered, err := RecoverUnconfirmedRestore(targetDir); err != nil || recovered {
		t.Fatalf("RecoverUnconfirmedRestore() = %v, %v after confirmation; want false, nil", recovered, err)
	}
}

func TestStartupRecoversUnconfirmedRestore(t *testing.T) {
	archive := buildRegistryBackup(t, `{"version":1,"nodes":{"restored":{}}}`)
	targetDir := t.TempDir()
	original := []byte(`{"version":1,"nodes":{"original":{}}}`)
	writeTestFile(t, filepath.Join(targetDir, "node_registry.json"), original)

	if _, err := NewRestorer(targetDir).Stage(bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("stage restore: %v", err)
	}
	if applied, err := ApplyPendingRestore(targetDir); err != nil || !applied {
		t.Fatalf("ApplyPendingRestore() = %v, %v; want true, nil", applied, err)
	}
	if recovered, err := RecoverUnconfirmedRestore(targetDir); err != nil || !recovered {
		t.Fatalf("RecoverUnconfirmedRestore() = %v, %v; want true, nil", recovered, err)
	}
	got, err := os.ReadFile(filepath.Join(targetDir, "node_registry.json"))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("startup-recovered registry = %s, %v; want original %s", got, err, original)
	}
	assertRestoreTransactionRemoved(t, targetDir)
}

func TestStartupFinishesPartialRestoreConfirmationCleanup(t *testing.T) {
	tests := []struct {
		remainingDir string
		confirmed    bool
	}{
		{remainingDir: appliedRestoreDir},
		{remainingDir: rollbackRestoreDir, confirmed: true},
	}
	for _, tt := range tests {
		t.Run(tt.remainingDir, func(t *testing.T) {
			dataDir := t.TempDir()
			remainingPath := filepath.Join(dataDir, tt.remainingDir)
			if err := os.Mkdir(remainingPath, 0700); err != nil {
				t.Fatalf("create partial transaction: %v", err)
			}
			if tt.confirmed {
				writeTestFile(t, filepath.Join(remainingPath, restoreCommitMarker), []byte("confirmed\n"))
			}
			if recovered, err := RecoverUnconfirmedRestore(dataDir); err != nil || !recovered {
				t.Fatalf("RecoverUnconfirmedRestore() = %v, %v; want true, nil", recovered, err)
			}
			assertRestoreTransactionRemoved(t, dataDir)
		})
	}
}

func TestStartupRollsBackInterruptedApplyAndKeepsPendingRestore(t *testing.T) {
	archive := buildRegistryBackup(t, `{"version":1,"nodes":{"restored":{}}}`)
	dataDir := t.TempDir()
	original := []byte(`{"version":1,"nodes":{"original":{}}}`)
	writeTestFile(t, filepath.Join(dataDir, "node_registry.json"), original)
	if _, err := NewRestorer(dataDir).Stage(bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("stage restore: %v", err)
	}

	rollbackPath := filepath.Join(dataDir, rollbackRestoreDir)
	if err := os.Mkdir(rollbackPath, 0700); err != nil {
		t.Fatalf("create rollback directory: %v", err)
	}
	if err := os.Rename(filepath.Join(dataDir, "node_registry.json"), filepath.Join(rollbackPath, "node_registry.json")); err != nil {
		t.Fatalf("stage original file: %v", err)
	}
	if err := os.Rename(filepath.Join(dataDir, pendingRestoreDir, "node_registry.json"), filepath.Join(dataDir, "node_registry.json")); err != nil {
		t.Fatalf("install restored file: %v", err)
	}

	if recovered, err := RecoverUnconfirmedRestore(dataDir); err != nil || !recovered {
		t.Fatalf("RecoverUnconfirmedRestore() = %v, %v; want true, nil", recovered, err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "node_registry.json"))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("recovered registry = %s, %v; want original %s", got, err, original)
	}
	if _, err := os.Stat(filepath.Join(dataDir, pendingRestoreDir, "node_registry.json")); err != nil {
		t.Fatalf("restored payload was not returned to pending state: %v", err)
	}
	if applied, err := ApplyPendingRestore(dataDir); err != nil || !applied {
		t.Fatalf("reapplying recovered pending restore = %v, %v; want true, nil", applied, err)
	}
}

func TestStartupRejectsUnexplainedRollbackDirectory(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, rollbackRestoreDir), 0700); err != nil {
		t.Fatal(err)
	}
	if recovered, err := RecoverUnconfirmedRestore(dataDir); err == nil || recovered {
		t.Fatalf("RecoverUnconfirmedRestore() = %v, %v; want false and an error", recovered, err)
	}
}

func buildRegistryBackup(t *testing.T, registry string) []byte {
	t.Helper()
	sourceDir := t.TempDir()
	writeTestFile(t, filepath.Join(sourceDir, "node_registry.json"), []byte(registry))
	var archive bytes.Buffer
	if _, err := NewCreator(sourceDir, "test").Create(&archive); err != nil {
		t.Fatalf("create registry backup: %v", err)
	}
	return archive.Bytes()
}

func assertRestoreTransactionRemoved(t *testing.T, dataDir string) {
	t.Helper()
	for _, name := range []string{appliedRestoreDir, rollbackRestoreDir} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Fatalf("restore transaction directory %s still exists: %v", name, err)
		}
	}
}

func buildRestoreTestArchive(t *testing.T, manifest Manifest, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create test ZIP entry: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write test ZIP entry: %v", err)
		}
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode test manifest: %v", err)
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatalf("create test manifest entry: %v", err)
	}
	if _, err := w.Write(manifestData); err != nil {
		t.Fatalf("write test manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close test ZIP: %v", err)
	}
	return buf.Bytes()
}
