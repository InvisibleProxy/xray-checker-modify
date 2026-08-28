package backup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreatorBuildsSanitizedBackup(t *testing.T) {
	dataDir := t.TempDir()
	nodeRegistry := []byte(`{"version":1,"nodes":{"node-1":{"name":"test"}}}`)
	telegramConfig := []byte(`{
  "enabled": true,
  "botToken": "secret-token",
  "chatId": "secret-chat",
  "messageThreadId": 42,
  "adminUserIds": [123],
  "speedReportsEnabled": true
}`)
	writeTestFile(t, filepath.Join(dataDir, "node_registry.json"), nodeRegistry)
	writeTestFile(t, filepath.Join(dataDir, "telegram_config.json"), telegramConfig)
	writeTestFile(t, filepath.Join(dataDir, "unrelated.json"), []byte(`{"secret":"not included"}`))

	createdAt := time.Date(2026, time.July, 14, 5, 4, 3, 0, time.FixedZone("test", 7*60*60))
	creator := NewCreator(dataDir, "v1.2.3")
	creator.now = func() time.Time { return createdAt }

	var buf bytes.Buffer
	result, err := creator.Create(&buf)
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if result.Filename != "xray-checker-backup-20260713T220403Z.zip" {
		t.Fatalf("unexpected filename: %s", result.Filename)
	}

	entries := readArchive(t, buf.Bytes())
	if len(entries) != 3 {
		t.Fatalf("expected 3 archive entries, got %d: %v", len(entries), mapKeys(entries))
	}
	if _, ok := entries["data/unrelated.json"]; ok {
		t.Fatal("unrelated data file was included")
	}
	if !bytes.Equal(entries["data/node_registry.json"], nodeRegistry) {
		t.Fatalf("node registry changed in archive: %s", entries["data/node_registry.json"])
	}

	var sanitizedTelegram map[string]json.RawMessage
	if err := json.Unmarshal(entries["data/telegram_config.json"], &sanitizedTelegram); err != nil {
		t.Fatalf("decode archived Telegram config: %v", err)
	}
	for _, field := range []string{"botToken", "chatId", "messageThreadId", "adminUserIds"} {
		if _, ok := sanitizedTelegram[field]; ok {
			t.Fatalf("sensitive Telegram field %q was included", field)
		}
	}
	if _, ok := sanitizedTelegram["speedReportsEnabled"]; !ok {
		t.Fatal("editable Telegram settings were not preserved")
	}

	var manifest Manifest
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.FormatVersion != formatVersion || manifest.AppVersion != "v1.2.3" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if !manifest.CreatedAt.Equal(createdAt.UTC()) {
		t.Fatalf("unexpected manifest timestamp: %s", manifest.CreatedAt)
	}
	if len(manifest.Files) != 2 {
		t.Fatalf("expected 2 manifest files, got %d", len(manifest.Files))
	}
	for _, file := range manifest.Files {
		data, ok := entries[file.Path]
		if !ok {
			t.Fatalf("manifest references missing file %s", file.Path)
		}
		if file.Size != int64(len(data)) {
			t.Fatalf("unexpected size for %s: %d", file.Path, file.Size)
		}
		digest := sha256.Sum256(data)
		if file.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("unexpected digest for %s", file.Path)
		}
	}
}

func TestCreatorCreatesManifestWhenDataFilesAreMissing(t *testing.T) {
	creator := NewCreator(t.TempDir(), "test")
	var buf bytes.Buffer
	result, err := creator.Create(&buf)
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}

	entries := readArchive(t, buf.Bytes())
	if len(entries) != 1 {
		t.Fatalf("expected manifest-only archive, got %v", mapKeys(entries))
	}
	if len(result.Manifest.Files) != 0 {
		t.Fatalf("expected empty file list, got %+v", result.Manifest.Files)
	}
}

func TestCreatorIncludesRemnawaveSettingsButExcludesOwnershipRuntime(t *testing.T) {
	dataDir := t.TempDir()
	config := []byte(`{"version":1,"policy":{"enabled":true,"outageMinutes":15,"minimumFailures":3,"recoveryMinutes":5},"squadPairs":[],"nodeMappings":{}}`)
	writeTestFile(t, filepath.Join(dataDir, "remnawave_announce_config.json"), config)
	writeTestFile(t, filepath.Join(dataDir, "remnawave_announce_state.json"), []byte(`{"version":1,"managed":{"external-1":{"value":"rwEncodeBase64:secret runtime","message":"secret runtime"}}}`))

	var archive bytes.Buffer
	result, err := NewCreator(dataDir, "test").Create(&archive)
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	entries := readArchive(t, archive.Bytes())
	if _, ok := entries["data/remnawave_announce_config.json"]; !ok {
		t.Fatal("Remnawave settings were not included")
	}
	if _, ok := entries["data/remnawave_announce_state.json"]; ok {
		t.Fatal("Remnawave ownership runtime was included")
	}
	foundExclusion := false
	for _, exclusion := range result.Manifest.Excluded {
		if strings.Contains(exclusion, "Remnawave API token") {
			foundExclusion = true
		}
	}
	if !foundExclusion {
		t.Fatalf("manifest does not explain Remnawave exclusions: %+v", result.Manifest.Excluded)
	}
}

func TestCreatorRejectsInvalidJSON(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "speedtest_results.json"), []byte(`{"broken"`))

	var buf bytes.Buffer
	_, err := NewCreator(dataDir, "test").Create(&buf)
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestCreatorSanitizesTelegramSecretsCaseInsensitively(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "telegram_config.json"), []byte(`{
  "Enabled": true,
  "BoTtOkEn": "secret-token",
  "CHATID": "secret-chat",
  "MessageThreadID": 17,
  "ADMINUSERIDS": [123],
  "SpeedReportsEnabled": true,
  "unknownSecret": "drop-me"
}`))

	var archive bytes.Buffer
	if _, err := NewCreator(dataDir, "test").Create(&archive); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	entries := readArchive(t, archive.Bytes())
	var config map[string]json.RawMessage
	if err := json.Unmarshal(entries["data/telegram_config.json"], &config); err != nil {
		t.Fatalf("decode sanitized Telegram config: %v", err)
	}
	if _, ok := config["enabled"]; !ok {
		t.Fatal("mixed-case safe field was not canonicalized")
	}
	if _, ok := config["speedReportsEnabled"]; !ok {
		t.Fatal("mixed-case editable field was not preserved")
	}
	for key := range config {
		switch key {
		case "botToken", "chatId", "messageThreadId", "adminUserIds", "unknownSecret":
			t.Fatalf("secret or unknown Telegram field %q was included", key)
		}
	}
	if len(config) != 2 {
		t.Fatalf("sanitized Telegram config contains unexpected fields: %#v", config)
	}
}

func TestCreatorRejectsCaseFoldedDuplicateTelegramKeys(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "telegram_config.json"), []byte(`{
  "enabled": true,
  "botToken": "first",
  "BOTTOKEN": "second"
}`))

	var archive bytes.Buffer
	if _, err := NewCreator(dataDir, "test").Create(&archive); err == nil {
		t.Fatal("case-folded duplicate keys were accepted")
	}
}

func TestPrepareDataFileRejectsMalformedPersistedSchemas(t *testing.T) {
	tests := []struct {
		name string
		file string
		data string
	}{
		{name: "null object", file: "node_registry.json", data: `null`},
		{name: "wrong nodes type", file: "node_registry.json", data: `{"version":1,"nodes":"bad"}`},
		{name: "wrong schedule config type", file: "speedtest_schedule.json", data: `{"enabled":true,"intervalSec":60,"config":"bad"}`},
		{name: "null schedule config", file: "speedtest_schedule.json", data: `{"enabled":true,"intervalSec":60,"config":null}`},
		{name: "invalid next schedule deadline", file: "speedtest_schedule.json", data: `{"enabled":true,"intervalSec":60,"config":{},"nextRunAt":"not-a-time"}`},
		{name: "wrong Telegram enabled type", file: "telegram_config.json", data: `{"enabled":"yes"}`},
		{name: "null Telegram enabled", file: "telegram_config.json", data: `{"enabled":null}`},
		{name: "trailing garbage", file: "node_registry.json", data: `{"version":1,"nodes":{}} garbage`},
		{name: "nested duplicate", file: "node_registry.json", data: `{"version":1,"nodes":{"node":{"name":"first","Name":"second"}}}`},
		{name: "unknown speed retry kind", file: "node_alert_state.json", data: `{"version":1,"nodes":{},"speedRetries":[{"kind":"unknown","stableIds":["node-1"],"config":{},"dueAt":"2026-08-22T12:00:00Z"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := prepareDataFile(tt.file, []byte(tt.data)); err == nil {
				t.Fatalf("prepareDataFile(%s) accepted malformed state: %s", tt.file, tt.data)
			}
		})
	}
}

func TestPrepareDataFileAcceptsCurrentAndLegacySpeedRetries(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"nodes":{},"speedRetries":[{"kind":"speed-confirmation","stableIds":["node-1"],"config":{},"dueAt":"2026-08-22T12:00:00Z"}]}`,
		`{"version":1,"nodes":{},"speedRetries":[{"stableIds":["node-1"],"config":{},"dueAt":"2026-08-22T12:00:00Z"}]}`,
		`{"version":1,"nodes":{},"speedRetries":[{"kind":"low-speed","stableIds":["node-1"],"config":{},"dueAt":"2026-08-22T12:00:00Z"},{"kind":"deadline","stableIds":["node-1"],"config":{},"dueAt":"2026-08-22T12:05:00Z"}]}`,
	} {
		if _, err := prepareDataFile("node_alert_state.json", []byte(data)); err != nil {
			t.Fatalf("valid speed retry state was rejected: %v\n%s", err, data)
		}
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}

func readArchive(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open backup archive: %v", err)
	}
	entries := make(map[string][]byte, len(zr.File))
	for _, file := range zr.File {
		r, err := file.Open()
		if err != nil {
			t.Fatalf("open archive entry %s: %v", file.Name, err)
		}
		var content bytes.Buffer
		if _, err := content.ReadFrom(r); err != nil {
			_ = r.Close()
			t.Fatalf("read archive entry %s: %v", file.Name, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close archive entry %s: %v", file.Name, err)
		}
		entries[file.Name] = content.Bytes()
	}
	return entries
}

func mapKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
