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
	MaxArchiveSize      = int64(512 * 1024 * 1024)
	maxManifestSize     = int64(1024 * 1024)
	pendingRestoreDir   = ".restore-pending"
	pendingRestoreFile  = "restore.json"
	appliedRestoreDir   = ".restore-applied"
	rollbackRestoreDir  = ".restore-rollback"
	restoreCommitMarker = ".confirmed"
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
	appliedExists, rollbackExists, err := inspectRestoreTransactionDirs(dataDir)
	if err != nil {
		return false, err
	}
	if appliedExists || rollbackExists {
		return false, nil
	}

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

	rollbackDir := filepath.Join(dataDir, rollbackRestoreDir)
	if err := os.Mkdir(rollbackDir, 0700); err != nil {
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

	appliedPath := filepath.Join(dataDir, appliedRestoreDir)
	if err := os.Rename(pendingPath, appliedPath); err != nil {
		return false, rollback(fmt.Errorf("finalize pending restore: %w", err))
	}

	cleanupRollback = false
	return true, nil
}

// ConfirmAppliedRestore commits a restore only after every persisted-state
// owner has loaded the installed files successfully.
func ConfirmAppliedRestore(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("restore data directory is required")
	}
	restoreMu.Lock()
	defer restoreMu.Unlock()
	appliedExists, rollbackExists, err := inspectRestoreTransactionDirs(dataDir)
	if err != nil {
		return err
	}
	if !appliedExists && !rollbackExists {
		return nil
	}
	if rollbackExists {
		marked, err := restoreCommitMarked(dataDir)
		if err != nil {
			return err
		}
		if !marked {
			if !appliedExists {
				return fmt.Errorf("restore transaction is incomplete and cannot be confirmed")
			}
			if err := writeRestoreCommitMarker(dataDir); err != nil {
				return err
			}
		}
	}
	// The durable marker makes this cleanup restart-safe: once it exists, a
	// later startup finishes the commit instead of rolling installed files back.
	if err := cleanupPartialRestoreTransactionLocked(dataDir, appliedExists, rollbackExists); err != nil {
		return err
	}
	return nil
}

func cleanupPartialRestoreTransactionLocked(dataDir string, appliedExists bool, rollbackExists bool) error {
	if appliedExists {
		if err := os.RemoveAll(filepath.Join(dataDir, appliedRestoreDir)); err != nil {
			return fmt.Errorf("remove applied restore directory: %w", err)
		}
	}
	if rollbackExists {
		if err := os.RemoveAll(filepath.Join(dataDir, rollbackRestoreDir)); err != nil {
			return fmt.Errorf("remove restore rollback directory: %w", err)
		}
	}
	return nil
}

// RollbackAppliedRestore restores the pre-restore files after a state loader
// rejects any installed file.
func RollbackAppliedRestore(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("restore data directory is required")
	}
	restoreMu.Lock()
	defer restoreMu.Unlock()
	return rollbackAppliedRestoreLocked(dataDir)
}

// RecoverUnconfirmedRestore is called once at process startup before applying
// a new pending restore. A leftover transaction means the previous startup did
// not reach ConfirmAppliedRestore.
func RecoverUnconfirmedRestore(dataDir string) (bool, error) {
	if dataDir == "" {
		return false, fmt.Errorf("restore data directory is required")
	}
	restoreMu.Lock()
	defer restoreMu.Unlock()
	appliedExists, rollbackExists, err := inspectRestoreTransactionDirs(dataDir)
	if err != nil {
		return false, err
	}
	if !appliedExists && !rollbackExists {
		return false, nil
	}
	if rollbackExists {
		marked, err := restoreCommitMarked(dataDir)
		if err != nil {
			return false, err
		}
		if marked {
			if err := cleanupPartialRestoreTransactionLocked(dataDir, appliedExists, true); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if appliedExists && rollbackExists {
		if err := rollbackRestoreTransactionLocked(dataDir, appliedRestoreDir, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if appliedExists {
		// Compatibility with the earlier confirmation order, which could leave
		// only the applied marker after the rollback copy had been removed.
		if err := cleanupPartialRestoreTransactionLocked(dataDir, true, false); err != nil {
			return false, err
		}
		return true, nil
	}
	pendingExists, err := inspectRestoreStateDir(dataDir, pendingRestoreDir)
	if err != nil {
		return false, err
	}
	if !pendingExists {
		return false, fmt.Errorf("restore rollback directory exists without pending or applied state")
	}
	if err := rollbackRestoreTransactionLocked(dataDir, pendingRestoreDir, true); err != nil {
		return false, err
	}
	return true, nil
}

func rollbackAppliedRestoreLocked(dataDir string) error {
	appliedExists, rollbackExists, err := inspectRestoreTransactionDirs(dataDir)
	if err != nil {
		return err
	}
	if !appliedExists && !rollbackExists {
		return nil
	}
	if !appliedExists || !rollbackExists {
		return fmt.Errorf("restore transaction is incomplete and cannot be rolled back")
	}
	marked, err := restoreCommitMarked(dataDir)
	if err != nil {
		return err
	}
	if marked {
		return fmt.Errorf("restore transaction is already confirmed")
	}
	return rollbackRestoreTransactionLocked(dataDir, appliedRestoreDir, false)
}

func rollbackRestoreTransactionLocked(dataDir string, stateDirName string, keepState bool) error {
	statePath := filepath.Join(dataDir, stateDirName)
	rollbackPath := filepath.Join(dataDir, rollbackRestoreDir)
	_, included, err := readPendingRestoreState(statePath)
	if err != nil {
		return err
	}
	if err := validateRestoreTargets(dataDir); err != nil {
		return err
	}

	for _, name := range dataFileNames {
		destination := filepath.Join(dataDir, name)
		stagedRestored := filepath.Join(statePath, name)
		original := filepath.Join(rollbackPath, name)
		destinationExists, err := regularFileExists(destination, "restore target "+name)
		if err != nil {
			return err
		}
		stagedExists, err := regularFileExists(stagedRestored, "staged restore file "+name)
		if err != nil {
			return err
		}
		originalExists, err := regularFileExists(original, "original restore file "+name)
		if err != nil {
			return err
		}

		if originalExists {
			if included[name] && !stagedExists && destinationExists {
				if err := os.Rename(destination, stagedRestored); err != nil {
					return fmt.Errorf("return restored file %s to staging: %w", name, err)
				}
				destinationExists = false
				stagedExists = true
			}
			if destinationExists {
				return fmt.Errorf("restore target %s unexpectedly exists while its original is staged", name)
			}
			if err := os.Rename(original, destination); err != nil {
				return fmt.Errorf("restore original file %s: %w", name, err)
			}
			continue
		}

		if included[name] && !stagedExists && destinationExists {
			if err := os.Rename(destination, stagedRestored); err != nil {
				return fmt.Errorf("remove restored file %s during rollback: %w", name, err)
			}
		}
	}

	if err := os.RemoveAll(rollbackPath); err != nil {
		return fmt.Errorf("remove restore rollback directory: %w", err)
	}
	if !keepState {
		if err := os.RemoveAll(statePath); err != nil {
			return fmt.Errorf("remove %s: %w", stateDirName, err)
		}
	}
	return nil
}

func regularFileExists(path string, label string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not regular", label)
	}
	return true, nil
}

func writeRestoreCommitMarker(dataDir string) error {
	markerPath := filepath.Join(dataDir, rollbackRestoreDir, restoreCommitMarker)
	if marked, err := restoreCommitMarked(dataDir); err != nil {
		return err
	} else if marked {
		return nil
	}
	file, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create restore commit marker: %w", err)
	}
	if _, err := file.WriteString("confirmed\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write restore commit marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync restore commit marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close restore commit marker: %w", err)
	}
	return nil
}

func restoreCommitMarked(dataDir string) (bool, error) {
	markerPath := filepath.Join(dataDir, rollbackRestoreDir, restoreCommitMarker)
	exists, err := regularFileExists(markerPath, "restore commit marker")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false, fmt.Errorf("read restore commit marker: %w", err)
	}
	if string(data) != "confirmed\n" {
		return false, fmt.Errorf("restore commit marker has invalid contents")
	}
	return true, nil
}

func inspectRestoreStateDir(dataDir string, name string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dataDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is not a regular directory", name)
	}
	return true, nil
}

func inspectRestoreTransactionDirs(dataDir string) (appliedExists bool, rollbackExists bool, err error) {
	for _, entry := range []struct {
		name   string
		exists *bool
	}{
		{name: appliedRestoreDir, exists: &appliedExists},
		{name: rollbackRestoreDir, exists: &rollbackExists},
	} {
		info, statErr := os.Lstat(filepath.Join(dataDir, entry.name))
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return false, false, fmt.Errorf("inspect %s: %w", entry.name, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, false, fmt.Errorf("%s is not a regular directory", entry.name)
		}
		*entry.exists = true
	}
	return appliedExists, rollbackExists, nil
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
	if _, err := decodeJSONObjectUnique(manifestData); err != nil {
		return nil, Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
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
	if manifest.Files == nil {
		return nil, Manifest{}, fmt.Errorf("backup manifest does not contain a files array")
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
	state, included, err := readPendingRestoreState(pendingPath)
	if err != nil {
		return pendingRestore{}, nil, err
	}

	for name := range included {
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

func readPendingRestoreState(pendingPath string) (pendingRestore, map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(pendingPath, pendingRestoreFile))
	if err != nil {
		return pendingRestore{}, nil, fmt.Errorf("read pending restore state: %w", err)
	}
	if _, err := decodeJSONObjectUnique(data); err != nil {
		return pendingRestore{}, nil, fmt.Errorf("decode pending restore state: %w", err)
	}
	var state pendingRestore
	if err := json.Unmarshal(data, &state); err != nil {
		return pendingRestore{}, nil, fmt.Errorf("decode pending restore state: %w", err)
	}
	if state.FormatVersion != formatVersion {
		return pendingRestore{}, nil, fmt.Errorf("unsupported pending restore format version %d", state.FormatVersion)
	}
	if state.SourceCreatedAt.IsZero() || state.Files == nil {
		return pendingRestore{}, nil, fmt.Errorf("pending restore state is incomplete")
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
