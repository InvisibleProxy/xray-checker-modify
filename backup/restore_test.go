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
