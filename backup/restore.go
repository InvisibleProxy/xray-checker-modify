package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	MaxArchiveSize     = int64(512 * 1024 * 1024)
	maxManifestSize    = int64(1024 * 1024)
	pendingRestoreDir  = ".restore-pending"
	pendingRestoreFile = "restore.json"
)

var restoreMu sync.Mutex

type RestoreResult struct {
	SourceCreatedAt  time.Time `json:"sourceCreatedAt"`
	SourceAppVersion string    `json:"sourceAppVersion,omitempty"`
	Files            []string  `json:"files"`
	RestartRequired  bool      `json:"restartRequired"`
}

type pendingRestore struct {
	FormatVersion    int       `json:"formatVersion"`
	SourceCreatedAt  time.Time `json:"sourceCreatedAt"`
	SourceAppVersion string    `json:"sourceAppVersion,omitempty"`
	Files            []string  `json:"files"`
}

type Restorer struct {
	dataDir string
}

func NewRestorer(dataDir string) *Restorer {
	return &Restorer{dataDir: dataDir}
}

func (r *Restorer) Stage(reader io.ReaderAt, size int64) (RestoreResult, error) {
	if r == nil {
		return RestoreResult{}, fmt.Errorf("backup restorer is nil")
	}
	if reader == nil {
		return RestoreResult{}, fmt.Errorf("backup archive reader is nil")
	}
	if r.dataDir == "" {
		return RestoreResult{}, fmt.Errorf("restore data directory is required")
	}
	if size <= 0 {
		return RestoreResult{}, fmt.Errorf("backup archive is empty")
	}
	if size > MaxArchiveSize {
		return RestoreResult{}, fmt.Errorf("backup archive exceeds %d MiB", MaxArchiveSize/(1024*1024))
	}

	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open backup archive: %w", err)
	}
	files, manifest, err := validateArchive(zr)
	if err != nil {
		return RestoreResult{}, err
	}

	restoreMu.Lock()
	defer restoreMu.Unlock()
	if err := r.stageFiles(files, manifest); err != nil {
		return RestoreResult{}, err
	}

	resultFiles := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		resultFiles = append(resultFiles, file.Path)
	}
	return RestoreResult{
		SourceCreatedAt:  manifest.CreatedAt,
		SourceAppVersion: manifest.AppVersion,
		Files:            resultFiles,
		RestartRequired:  true,
	}, nil
}

func ApplyPendingRestore(dataDir string) (bool, error) {
	if dataDir == "" {
		return false, fmt.Errorf("restore data directory is required")
	}

	restoreMu.Lock()
	defer restoreMu.Unlock()

	pendingPath := filepath.Join(dataDir, pendingRestoreDir)
	info, err := os.Lstat(pendingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect pending restore: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("pending restore path is not a regular directory")
	}

	_, included, err := loadPendingRestore(pendingPath)
	if err != nil {
		return false, err
	}
	if err := validateRestoreTargets(dataDir); err != nil {
		return false, err
	}

	rollbackDir, err := os.MkdirTemp(dataDir, ".restore-rollback-*")
	if err != nil {
		return false, fmt.Errorf("create restore rollback directory: %w", err)
	}
	cleanupRollback := true
	defer func() {
		if cleanupRollback {
			_ = os.RemoveAll(rollbackDir)
		}
	}()

	var movedOld []string
	var installed []string
	rollback := func(cause error) error {
		var rollbackErrors []error
		for i := len(installed) - 1; i >= 0; i-- {
			name := installed[i]
			if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(pendingPath, name)); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("return restored file %s to staging: %w", name, err))
			}
		}
		for i := len(movedOld) - 1; i >= 0; i-- {
			name := movedOld[i]
			if err := os.Rename(filepath.Join(rollbackDir, name), filepath.Join(dataDir, name)); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore original file %s: %w", name, err))
			}
		}
		if len(rollbackErrors) == 0 {
			return cause
		}
		cleanupRollback = false
		return errors.Join(append([]error{cause}, rollbackErrors...)...)
	}

	for _, name := range dataFileNames {
		destination := filepath.Join(dataDir, name)
		if _, err := os.Lstat(destination); err == nil {
			if err := os.Rename(destination, filepath.Join(rollbackDir, name)); err != nil {
				return false, rollback(fmt.Errorf("stage original file %s for rollback: %w", name, err))
			}
			movedOld = append(movedOld, name)
		} else if !os.IsNotExist(err) {
			return false, rollback(fmt.Errorf("inspect restore target %s: %w", name, err))
		}

		if included[name] {
			if err := os.Rename(filepath.Join(pendingPath, name), destination); err != nil {
				return false, rollback(fmt.Errorf("install restored file %s: %w", name, err))
			}
			installed = append(installed, name)
			if err := os.Chmod(destination, 0600); err != nil {
				return false, rollback(fmt.Errorf("set restored file permissions for %s: %w", name, err))
			}
		}
	}

	appliedPath, err := unusedTempPath(dataDir, ".restore-applied-*")
	if err != nil {
		return false, rollback(err)
	}
	if err := os.Rename(pendingPath, appliedPath); err != nil {
		return false, rollback(fmt.Errorf("finalize pending restore: %w", err))
	}

	cleanupRollback = false
	_ = os.RemoveAll(rollbackDir)
	_ = os.RemoveAll(appliedPath)
	return true, nil
}

func validateArchive(zr *zip.Reader) (map[string][]byte, Manifest, error) {
	if len(zr.File) == 0 || len(zr.File) > len(dataFileNames)+1 {
		return nil, Manifest{}, fmt.Errorf("backup archive has an invalid number of entries")
	}

	entries := make(map[string]*zip.File, len(zr.File))
	for _, file := range zr.File {
		name := file.Name
		if name != "manifest.json" && !isAllowedArchivePath(name) {
			return nil, Manifest{}, fmt.Errorf("backup archive contains unsupported entry %q", name)
		}
		if _, exists := entries[name]; exists {
			return nil, Manifest{}, fmt.Errorf("backup archive contains duplicate entry %q", name)
		}
		if file.FileInfo().IsDir() || file.Mode()&os.ModeType != 0 {
			return nil, Manifest{}, fmt.Errorf("backup archive entry %q is not a regular file", name)
		}
		entries[name] = file
	}

	manifestEntry, ok := entries["manifest.json"]
	if !ok {
		return nil, Manifest{}, fmt.Errorf("backup archive does not contain manifest.json")
	}
	manifestData, err := readZipEntry(manifestEntry, maxManifestSize)
	if err != nil {
		return nil, Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.FormatVersion != formatVersion {
		return nil, Manifest{}, fmt.Errorf("unsupported backup format version %d", manifest.FormatVersion)
	}
	if manifest.CreatedAt.IsZero() {
		return nil, Manifest{}, fmt.Errorf("backup manifest does not contain a creation time")
	}
	if len(manifest.Files) > len(dataFileNames) {
		return nil, Manifest{}, fmt.Errorf("backup manifest contains too many data files")
	}

	result := make(map[string][]byte, len(manifest.Files))
	manifestPaths := make(map[string]bool, len(manifest.Files))
	var totalSize int64
	for _, manifestFile := range manifest.Files {
		if !isAllowedArchivePath(manifestFile.Path) {
			return nil, Manifest{}, fmt.Errorf("backup manifest contains unsupported path %q", manifestFile.Path)
		}
		if manifestPaths[manifestFile.Path] {
			return nil, Manifest{}, fmt.Errorf("backup manifest contains duplicate path %q", manifestFile.Path)
		}
		manifestPaths[manifestFile.Path] = true

		entry, ok := entries[manifestFile.Path]
		if !ok {
			return nil, Manifest{}, fmt.Errorf("backup archive is missing %s", manifestFile.Path)
		}
		data, err := readZipEntry(entry, MaxArchiveSize)
		if err != nil {
			return nil, Manifest{}, err
		}
		if manifestFile.Size < 0 || int64(len(data)) != manifestFile.Size {
			return nil, Manifest{}, fmt.Errorf("backup file %s has an invalid size", manifestFile.Path)
		}
		totalSize += int64(len(data))
		if totalSize > MaxArchiveSize {
			return nil, Manifest{}, fmt.Errorf("backup data exceeds %d MiB", MaxArchiveSize/(1024*1024))
		}
		digest := sha256.Sum256(data)
		if !strings.EqualFold(manifestFile.SHA256, hex.EncodeToString(digest[:])) {
			return nil, Manifest{}, fmt.Errorf("backup file %s failed integrity verification", manifestFile.Path)
		}

		name := path.Base(manifestFile.Path)
		prepared, err := prepareDataFile(name, data)
		if err != nil {
			return nil, Manifest{}, err
		}
		result[name] = prepared
	}

	if len(entries) != len(manifestPaths)+1 {
		return nil, Manifest{}, fmt.Errorf("backup archive contains data files missing from the manifest")
	}
	return result, manifest, nil
}

func (r *Restorer) stageFiles(files map[string][]byte, manifest Manifest) error {
	if err := os.MkdirAll(r.dataDir, 0755); err != nil {
		return fmt.Errorf("create restore data directory: %w", err)
	}
	stageDir, err := os.MkdirTemp(r.dataDir, ".restore-stage-*")
	if err != nil {
		return fmt.Errorf("create restore staging directory: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	fileNames := make([]string, 0, len(files))
	for _, name := range dataFileNames {
		data, ok := files[name]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(stageDir, name), data, 0600); err != nil {
			return fmt.Errorf("stage restore file %s: %w", name, err)
		}
		fileNames = append(fileNames, name)
	}

	stateData, err := json.MarshalIndent(pendingRestore{
		FormatVersion:    formatVersion,
		SourceCreatedAt:  manifest.CreatedAt,
		SourceAppVersion: manifest.AppVersion,
		Files:            fileNames,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pending restore: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, pendingRestoreFile), append(stateData, '\n'), 0600); err != nil {
		return fmt.Errorf("write pending restore state: %w", err)
	}

	pendingPath := filepath.Join(r.dataDir, pendingRestoreDir)
	var oldPath string
	if _, err := os.Lstat(pendingPath); err == nil {
		oldPath, err = unusedTempPath(r.dataDir, ".restore-old-*")
		if err != nil {
			return err
		}
		if err := os.Rename(pendingPath, oldPath); err != nil {
			return fmt.Errorf("replace existing pending restore: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing pending restore: %w", err)
	}

	if err := os.Rename(stageDir, pendingPath); err != nil {
		if oldPath != "" {
			_ = os.Rename(oldPath, pendingPath)
		}
		return fmt.Errorf("activate pending restore: %w", err)
	}
	cleanupStage = false
	if oldPath != "" {
		_ = os.RemoveAll(oldPath)
	}
	return nil
}

func loadPendingRestore(pendingPath string) (pendingRestore, map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(pendingPath, pendingRestoreFile))
	if err != nil {
		return pendingRestore{}, nil, fmt.Errorf("read pending restore state: %w", err)
	}
	var state pendingRestore
	if err := json.Unmarshal(data, &state); err != nil {
		return pendingRestore{}, nil, fmt.Errorf("decode pending restore state: %w", err)
	}
	if state.FormatVersion != formatVersion {
		return pendingRestore{}, nil, fmt.Errorf("unsupported pending restore format version %d", state.FormatVersion)
	}
	if len(state.Files) > len(dataFileNames) {
		return pendingRestore{}, nil, fmt.Errorf("pending restore contains too many files")
	}

	included := make(map[string]bool, len(state.Files))
	for _, name := range state.Files {
		if !isDataFileName(name) || included[name] {
			return pendingRestore{}, nil, fmt.Errorf("pending restore contains invalid file %q", name)
		}
		included[name] = true
		filePath := filepath.Join(pendingPath, name)
		info, err := os.Lstat(filePath)
		if err != nil {
			return pendingRestore{}, nil, fmt.Errorf("inspect pending restore file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return pendingRestore{}, nil, fmt.Errorf("pending restore file %s is not regular", name)
		}
		fileData, err := os.ReadFile(filePath)
		if err != nil {
			return pendingRestore{}, nil, fmt.Errorf("read pending restore file %s: %w", name, err)
		}
		prepared, err := prepareDataFile(name, fileData)
		if err != nil {
			return pendingRestore{}, nil, err
		}
		if err := os.WriteFile(filePath, prepared, 0600); err != nil {
			return pendingRestore{}, nil, fmt.Errorf("sanitize pending restore file %s: %w", name, err)
		}
	}
	return state, included, nil
}

func validateRestoreTargets(dataDir string) error {
	for _, name := range dataFileNames {
		info, err := os.Lstat(filepath.Join(dataDir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect restore target %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore target %s is not a regular file", name)
		}
	}
	return nil
}

func readZipEntry(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("backup entry %s is too large", file.Name)
	}
	r, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open backup entry %s: %w", file.Name, err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read backup entry %s: %w", file.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("backup entry %s is too large", file.Name)
	}
	return data, nil
}

func isAllowedArchivePath(name string) bool {
	if path.Clean(name) != name || strings.Contains(name, "\\") {
		return false
	}
	if path.Dir(name) != "data" {
		return false
	}
	return isDataFileName(path.Base(name))
}

func isDataFileName(name string) bool {
	for _, allowed := range dataFileNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func unusedTempPath(dir, pattern string) (string, error) {
	tempPath, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("reserve restore path: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return "", fmt.Errorf("reserve restore path: %w", err)
	}
	return tempPath, nil
}
