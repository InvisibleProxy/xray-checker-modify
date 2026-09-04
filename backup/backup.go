package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const formatVersion = 1

var dataFileNames = []string{
	"node_alert_state.json",
	"node_registry.json",
	"project_state.json",
	"remnawave_announce_config.json",
	"speedtest_results.json",
	"speedtest_schedule.json",
	"telegram_config.json",
}

var excludedData = []string{
	"environment variables",
	"geo files",
	"xray_config.json",
	"Telegram credentials and administrator IDs",
	"Remnawave API token and announce ownership runtime",
	"diagnostic agent registry, enrollment state and controller-bound public identities",
	"subscription sources, whose URLs carry the subscription's own access token",
}

type FileInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	FormatVersion int        `json:"formatVersion"`
	CreatedAt     time.Time  `json:"createdAt"`
	AppVersion    string     `json:"appVersion,omitempty"`
	Files         []FileInfo `json:"files"`
	Excluded      []string   `json:"excluded"`
}

type Result struct {
	Filename string
	Manifest Manifest
}

type Creator struct {
	dataDir    string
	appVersion string
	now        func() time.Time
}

type archiveFile struct {
	path string
	data []byte
}

func NewCreator(dataDir, appVersion string) *Creator {
	return &Creator{
		dataDir:    dataDir,
		appVersion: appVersion,
		now:        time.Now,
	}
}

func (c *Creator) Create(w io.Writer) (Result, error) {
	if c == nil {
		return Result{}, fmt.Errorf("backup creator is nil")
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return c.createAt(w, now().UTC())
}

func (c *Creator) createAt(w io.Writer, createdAt time.Time) (Result, error) {
	if w == nil {
		return Result{}, fmt.Errorf("backup writer is nil")
	}
	if c.dataDir == "" {
		return Result{}, fmt.Errorf("backup data directory is required")
	}

	createdAt = createdAt.UTC()
	files, manifestFiles, err := c.collectFiles()
	if err != nil {
		return Result{}, err
	}

	manifest := Manifest{
		FormatVersion: formatVersion,
		CreatedAt:     createdAt,
		AppVersion:    c.appVersion,
		Files:         manifestFiles,
		Excluded:      append([]string(nil), excludedData...),
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	zw := zip.NewWriter(w)
	for _, file := range files {
		if err := writeArchiveFile(zw, file.path, file.data, createdAt); err != nil {
			_ = zw.Close()
			return Result{}, err
		}
	}
	if err := writeArchiveFile(zw, "manifest.json", manifestData, createdAt); err != nil {
		_ = zw.Close()
		return Result{}, err
	}
	if err := zw.Close(); err != nil {
		return Result{}, fmt.Errorf("finalize backup archive: %w", err)
	}

	return Result{
		Filename: fmt.Sprintf("xray-checker-backup-%s.zip", createdAt.Format("20060102T150405Z")),
		Manifest: manifest,
	}, nil
}

func (c *Creator) collectFiles() ([]archiveFile, []FileInfo, error) {
	files := make([]archiveFile, 0, len(dataFileNames))
	manifestFiles := make([]FileInfo, 0, len(dataFileNames))

	for _, name := range dataFileNames {
		filePath := filepath.Join(c.dataDir, name)
		info, err := os.Lstat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("inspect backup file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("backup file %s is not a regular file", name)
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, nil, fmt.Errorf("read backup file %s: %w", name, err)
		}
		data, err = prepareDataFile(name, data)
		if err != nil {
			return nil, nil, err
		}

		archivePath := path.Join("data", name)
		digest := sha256.Sum256(data)
		files = append(files, archiveFile{path: archivePath, data: data})
		manifestFiles = append(manifestFiles, FileInfo{
			Path:   archivePath,
			Size:   int64(len(data)),
			SHA256: hex.EncodeToString(digest[:]),
		})
	}

	return files, manifestFiles, nil
}

func prepareDataFile(name string, data []byte) ([]byte, error) {
	config, err := decodeJSONObjectUnique(data)
	if err != nil {
		return nil, fmt.Errorf("backup file %s: %w", name, err)
	}
	if name != "telegram_config.json" {
		if err := validateDataFile(name, data); err != nil {
			return nil, err
		}
		return data, nil
	}

	safeFields := map[string]string{
		"enabled":                      "enabled",
		"commandpollingenabled":        "commandPollingEnabled",
		"speedreportsenabled":          "speedReportsEnabled",
		"speedreportmode":              "speedReportMode",
		"lowspeedthresholdmbps":        "lowSpeedThresholdMbps",
		"speedreportlimit":             "speedReportLimit",
		"nodealertsenabled":            "nodeAlertsEnabled",
		"alertcheckminutes":            "alertCheckMinutes",
		"alertafterfailures":           "alertAfterFailures",
		"alertrepeatminutes":           "alertRepeatMinutes",
		"alertdiagnosticsminutes":      "alertDiagnosticsMinutes",
		"alertreminderscheduleminutes": "alertReminderScheduleMinutes",
		"alertmaxreminderminutes":      "alertMaxReminderMinutes",
		"groupofflinereminders":        "groupOfflineReminders",
		"notifyrecovery":               "notifyRecovery",
		"mutednodeids":                 "mutedNodeIds",
		"mutedspeednodeids":            "mutedSpeedNodeIds",
		"mutedalertnodeids":            "mutedAlertNodeIds",
		"timeoutsec":                   "timeoutSec",
	}
	sanitizedConfig := make(map[string]json.RawMessage)
	for key, value := range config {
		if canonical, ok := safeFields[strings.ToLower(key)]; ok {
			sanitizedConfig[canonical] = value
		}
	}

	sanitized, err := json.MarshalIndent(sanitizedConfig, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("sanitize backup file %s: %w", name, err)
	}
	sanitized = append(sanitized, '\n')
	if err := validateDataFile(name, sanitized); err != nil {
		return nil, err
	}
	return sanitized, nil
}

func writeArchiveFile(zw *zip.Writer, name string, data []byte, modifiedAt time.Time) error {
	header := &zip.FileHeader{
		Name:   name,
		Method: zip.Deflate,
	}
	header.SetModTime(modifiedAt)
	header.SetMode(0600)

	w, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create backup entry %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write backup entry %s: %w", name, err)
	}
	return nil
}
