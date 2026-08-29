package nodemerge

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/backup"
	"xray-checker/nodearchive"
	"xray-checker/speedtest"
)

const (
	pendingDirName      = ".node-merge-pending"
	appliedDirName      = ".node-merge-applied"
	rollbackDirName     = ".node-merge-rollback"
	instructionFileName = "merge.json"
	commitMarkerName    = ".confirmed"
	nodeRegistryName    = "node_registry.json"
	speedResultsName    = "speedtest_results.json"
)

var (
	transactionMu   sync.Mutex
	stateFileNames  = []string{nodeRegistryName, speedResultsName}
	restoreDirNames = []string{".restore-pending", ".restore-applied", ".restore-rollback"}
)

type speedResultState struct {
	Version   int                           `json:"version"`
	UpdatedAt time.Time                     `json:"updatedAt"`
	LastRun   speedtest.RunInfo             `json:"lastRun"`
	Results   map[string]speedtest.Result   `json:"results"`
	History   map[string][]speedtest.Result `json:"history"`
}

// ApplyPending applies a staged node merge before persisted-state owners load
// their files. The originals remain in a rollback directory until ConfirmApplied
// is called after all owners accept the merged state.
func ApplyPending(dataDir string, activeStableIDs map[string]bool) (bool, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return false, fmt.Errorf("node merge data directory is required")
	}

	transactionMu.Lock()
	defer transactionMu.Unlock()
	if err := ensureNoRestoreTransaction(dataDir); err != nil {
		return false, err
	}
	pendingExists, appliedExists, rollbackExists, err := inspectTransactionDirs(dataDir)
	if err != nil {
		return false, err
	}
	if appliedExists || rollbackExists {
		return false, nil
	}
	if !pendingExists {
		return false, nil
	}

	pendingPath := filepath.Join(dataDir, pendingDirName)
	instruction, err := loadInstruction(pendingPath)
	if err != nil {
		return false, err
	}
	if !activeStableIDs[instruction.TargetStableID] {
		return false, fmt.Errorf("node merge target %s is no longer active", instruction.TargetStableID)
	}
	if activeStableIDs[instruction.SourceStableID] {
		return false, fmt.Errorf("node merge source %s became active again", instruction.SourceStableID)
	}

	registryData, err := readValidatedStateFile(dataDir, nodeRegistryName)
	if err != nil {
		return false, err
	}
	speedData, err := readValidatedStateFile(dataDir, speedResultsName)
	if err != nil {
		return false, err
	}

	var registry nodearchive.StateFile
	if err := json.Unmarshal(registryData, &registry); err != nil {
		return false, fmt.Errorf("decode node registry for merge: %w", err)
	}
	var speedState speedResultState
	if err := json.Unmarshal(speedData, &speedState); err != nil {
		return false, fmt.Errorf("decode speed-test results for merge: %w", err)
	}
	source, ok := registry.Nodes[instruction.SourceStableID]
	if !ok {
		return false, fmt.Errorf("node merge source %s is missing from node registry", instruction.SourceStableID)
	}
	target, ok := registry.Nodes[instruction.TargetStableID]
	if !ok {
		return false, fmt.Errorf("node merge target %s is missing from node registry", instruction.TargetStableID)
	}
	// Store.Load historically repaired an empty record StableID from its map
	// key in memory. Mirror that normalization before comparing the staged
	// candidate so older valid state files can be merged safely.
	source.StableID = instruction.SourceStableID
	target.StableID = instruction.TargetStableID
	registry.Nodes[instruction.SourceStableID] = source
	registry.Nodes[instruction.TargetStableID] = target
	if source.Active {
		return false, fmt.Errorf("node merge source must remain retired")
	}
	if err := validateIdentityMatch(source, target); err != nil {
		return false, err
	}
	if confirmationToken(source, target) != instruction.ConfirmationToken {
		return false, fmt.Errorf("node merge identity changed after confirmation; preview it again")
	}

	now := time.Now().UTC()
	if err := mergeNodeState(&registry, instruction.SourceStableID, instruction.TargetStableID, now); err != nil {
		return false, err
	}
	mergeSpeedState(&speedState, instruction.SourceStableID, instruction.TargetStableID, target, now)

	registryData, err = json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode merged node registry: %w", err)
	}
	registryData = append(registryData, '\n')
	if err := backup.ValidateDataFile(nodeRegistryName, registryData); err != nil {
		return false, fmt.Errorf("validate merged node registry: %w", err)
	}
	speedData, err = json.MarshalIndent(speedState, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode merged speed-test results: %w", err)
	}
	speedData = append(speedData, '\n')
	if err := backup.ValidateDataFile(speedResultsName, speedData); err != nil {
		return false, fmt.Errorf("validate merged speed-test results: %w", err)
	}
	if err := writeDurableStateFile(filepath.Join(pendingPath, nodeRegistryName), registryData); err != nil {
		return false, err
	}
	if err := writeDurableStateFile(filepath.Join(pendingPath, speedResultsName), speedData); err != nil {
		return false, err
	}

	rollbackPath := filepath.Join(dataDir, rollbackDirName)
	if err := os.Mkdir(rollbackPath, 0700); err != nil {
		return false, fmt.Errorf("create node merge rollback directory: %w", err)
	}
	cleanupRollback := true
	defer func() {
		if cleanupRollback {
			_ = os.RemoveAll(rollbackPath)
		}
	}()
	rollback := func(cause error) error {
		if rollbackErr := rollbackTransactionLocked(dataDir, pendingDirName, true); rollbackErr != nil {
			cleanupRollback = false
			return errors.Join(cause, rollbackErr)
		}
		cleanupRollback = false
		return cause
	}

	for _, name := range stateFileNames {
		destination := filepath.Join(dataDir, name)
		if err := os.Rename(destination, filepath.Join(rollbackPath, name)); err != nil {
			return false, rollback(fmt.Errorf("stage original %s for node merge rollback: %w", name, err))
		}
		if err := os.Rename(filepath.Join(pendingPath, name), destination); err != nil {
			return false, rollback(fmt.Errorf("install merged %s: %w", name, err))
		}
		if err := os.Chmod(destination, 0600); err != nil {
			return false, rollback(fmt.Errorf("set merged %s permissions: %w", name, err))
		}
	}

	if err := os.Rename(pendingPath, filepath.Join(dataDir, appliedDirName)); err != nil {
		return false, rollback(fmt.Errorf("finalize applied node merge: %w", err))
	}
	cleanupRollback = false
	return true, nil
}

func ConfirmApplied(dataDir string) error {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return fmt.Errorf("node merge data directory is required")
	}
	transactionMu.Lock()
	defer transactionMu.Unlock()
	pendingExists, appliedExists, rollbackExists, err := inspectTransactionDirs(dataDir)
	if err != nil {
		return err
	}
	if pendingExists && !appliedExists && !rollbackExists {
		return nil
	}
	if !appliedExists || !rollbackExists {
		if !pendingExists && !appliedExists && !rollbackExists {
			return nil
		}
		return fmt.Errorf("node merge transaction is incomplete and cannot be confirmed")
	}
	if err := writeCommitMarker(dataDir); err != nil {
		return err
	}
	return cleanupTransactionLocked(dataDir, appliedExists, rollbackExists)
}

func RollbackApplied(dataDir string) error {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return fmt.Errorf("node merge data directory is required")
	}
	transactionMu.Lock()
	defer transactionMu.Unlock()
	_, appliedExists, rollbackExists, err := inspectTransactionDirs(dataDir)
	if err != nil {
		return err
	}
	if !appliedExists && !rollbackExists {
		return nil
	}
	if !appliedExists || !rollbackExists {
		return fmt.Errorf("node merge transaction is incomplete and cannot be rolled back")
	}
	marked, err := commitMarked(dataDir)
	if err != nil {
		return err
	}
	if marked {
		return fmt.Errorf("node merge transaction is already confirmed")
	}
	return rollbackTransactionLocked(dataDir, appliedDirName, false)
}

// RecoverUnconfirmed rolls back a merge left between installation and commit,
// or completes cleanup when a durable commit marker already exists.
func RecoverUnconfirmed(dataDir string) (bool, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return false, fmt.Errorf("node merge data directory is required")
	}
	transactionMu.Lock()
	defer transactionMu.Unlock()
	pendingExists, appliedExists, rollbackExists, err := inspectTransactionDirs(dataDir)
	if err != nil {
		return false, err
	}
	if !appliedExists && !rollbackExists {
		return false, nil
	}
	if rollbackExists {
		marked, err := commitMarked(dataDir)
		if err != nil {
			return false, err
		}
		if marked {
			if err := cleanupTransactionLocked(dataDir, appliedExists, true); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if appliedExists && rollbackExists {
		if err := rollbackTransactionLocked(dataDir, appliedDirName, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if pendingExists && rollbackExists {
		if err := rollbackTransactionLocked(dataDir, pendingDirName, true); err != nil {
			return false, err
		}
		return true, nil
	}
	if appliedExists {
		if err := os.RemoveAll(filepath.Join(dataDir, appliedDirName)); err != nil {
			return false, fmt.Errorf("remove incomplete applied node merge: %w", err)
		}
		return true, nil
	}
	return false, fmt.Errorf("node merge rollback directory exists without pending or applied state")
}

func mergeNodeState(state *nodearchive.StateFile, sourceID, targetID string, now time.Time) error {
	if state == nil || state.Nodes == nil {
		return fmt.Errorf("node registry is not initialized")
	}
	source, sourceOK := state.Nodes[sourceID]
	target, targetOK := state.Nodes[targetID]
	if !sourceOK || !targetOK {
		return fmt.Errorf("source and target nodes are required for merge")
	}
	if source.Active || !target.Active {
		return fmt.Errorf("node merge requires a retired source and active target")
	}
	if err := validateIdentityMatch(source, target); err != nil {
		return err
	}

	var err error
	target.TotalDowntimeSec, err = addInt64(target.TotalDowntimeSec, source.TotalDowntimeSec)
	if err != nil {
		return fmt.Errorf("merge total downtime: %w", err)
	}
	target.TotalProxyFailureSec, err = addInt64(target.TotalProxyFailureSec, source.TotalProxyFailureSec)
	if err != nil {
		return fmt.Errorf("merge total proxy failure: %w", err)
	}
	target.IncidentCount, err = addInt(target.IncidentCount, source.IncidentCount)
	if err != nil {
		return fmt.Errorf("merge incident count: %w", err)
	}
	target.ProxyFailureCount, err = addInt(target.ProxyFailureCount, source.ProxyFailureCount)
	if err != nil {
		return fmt.Errorf("merge proxy failure count: %w", err)
	}
	target.FirstSeenAt = earliestNonZero(target.FirstSeenAt, source.FirstSeenAt)
	target.LastSeenAt = latestTime(target.LastSeenAt, source.LastSeenAt)
	target.LastOfflineAt = latestTime(target.LastOfflineAt, source.LastOfflineAt)
	target.LastProxyFailureAt = latestTime(target.LastProxyFailureAt, source.LastProxyFailureAt)
	target.LastOnlineAt = latestTime(target.LastOnlineAt, source.LastOnlineAt)
	target.LastStatusAt = latestTime(target.LastStatusAt, source.LastStatusAt)
	if source.LongestDowntimeSec > target.LongestDowntimeSec {
		target.LongestDowntimeSec = source.LongestDowntimeSec
	}
	if source.LongestProxyFailureSec > target.LongestProxyFailureSec {
		target.LongestProxyFailureSec = source.LongestProxyFailureSec
	}
	mergeGeoState(&target, source)
	target.StableID = targetID
	target.Active = true
	target.RetiredAt = time.Time{}
	state.Nodes[targetID] = target
	delete(state.Nodes, sourceID)

	if state.MergedNodes == nil {
		state.MergedNodes = make(map[string][]string)
	}
	lineage := append([]string(nil), state.MergedNodes[targetID]...)
	lineage = append(lineage, sourceID)
	lineage = append(lineage, state.MergedNodes[sourceID]...)
	state.MergedNodes[targetID] = uniqueSorted(lineage, targetID)
	delete(state.MergedNodes, sourceID)

	for index := range state.Incidents {
		rewriteIncident(&state.Incidents[index], sourceID, targetID, target.Name)
	}
	if state.AvailabilityHistory == nil {
		state.AvailabilityHistory = make(map[string][]nodearchive.AvailabilitySample)
	}
	mergedAvailability := mergeAvailabilityHistory(
		state.AvailabilityHistory[targetID],
		state.AvailabilityHistory[sourceID],
	)
	delete(state.AvailabilityHistory, sourceID)
	if len(mergedAvailability) == 0 {
		delete(state.AvailabilityHistory, targetID)
	} else {
		state.AvailabilityHistory[targetID] = mergedAvailability
	}
	state.UpdatedAt = now
	return nil
}

func mergeAvailabilityHistory(first, second []nodearchive.AvailabilitySample) []nodearchive.AvailabilitySample {
	entries := make([]nodearchive.AvailabilitySample, 0, len(first)+len(second))
	entries = append(entries, first...)
	entries = append(entries, second...)
	seen := make(map[int64]bool, len(entries))
	merged := make([]nodearchive.AvailabilitySample, 0, len(entries))
	for _, sample := range entries {
		if sample.CheckedAt.IsZero() {
			continue
		}
		key := sample.CheckedAt.UnixNano()
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, sample)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].CheckedAt.After(merged[j].CheckedAt)
	})
	return merged
}

func mergeSpeedState(state *speedResultState, sourceID, targetID string, target nodearchive.NodeRecord, now time.Time) {
	if state.Results == nil {
		state.Results = make(map[string]speedtest.Result)
	}
	if state.History == nil {
		state.History = make(map[string][]speedtest.Result)
	}
	entries := append([]speedtest.Result(nil), state.History[sourceID]...)
	entries = append(entries, state.History[targetID]...)
	if result, ok := state.Results[sourceID]; ok {
		entries = append(entries, result)
	}
	if result, ok := state.Results[targetID]; ok {
		entries = append(entries, result)
	}
	merged := mergeHistory(entries, nil, target)
	delete(state.History, sourceID)
	delete(state.Results, sourceID)
	if len(merged) == 0 {
		delete(state.History, targetID)
		delete(state.Results, targetID)
	} else {
		state.History[targetID] = merged
		state.Results[targetID] = merged[0]
	}
	state.UpdatedAt = now
}

func mergeHistory(first, second []speedtest.Result, target nodearchive.NodeRecord) []speedtest.Result {
	entries := make([]speedtest.Result, 0, len(first)+len(second))
	entries = append(entries, first...)
	entries = append(entries, second...)
	seen := make(map[string]bool, len(entries))
	merged := make([]speedtest.Result, 0, len(entries))
	for _, entry := range entries {
		entry.StableID = target.StableID
		entry.Name = target.Name
		entry.SubName = target.SubName
		entry.Protocol = target.Protocol
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		key := string(encoded)
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, entry)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].CheckedAt.Equal(merged[j].CheckedAt) {
			left, _ := json.Marshal(merged[i])
			right, _ := json.Marshal(merged[j])
			return string(left) < string(right)
		}
		return merged[i].CheckedAt.After(merged[j].CheckedAt)
	})
	return merged
}

func mergeGeoState(target *nodearchive.NodeRecord, source nodearchive.NodeRecord) {
	if target == nil {
		return
	}
	if target.ClaimedCountry == "" {
		target.ClaimedCountry = source.ClaimedCountry
	}
	if target.ClaimedCountryCode == "" {
		target.ClaimedCountryCode = source.ClaimedCountryCode
	}
	if target.GeoUpdatedAt.IsZero() || source.GeoUpdatedAt.After(target.GeoUpdatedAt) {
		target.GeoIP = source.GeoIP
		target.GeoCountry = source.GeoCountry
		target.GeoCountryCode = source.GeoCountryCode
		target.GeoOrg = source.GeoOrg
		target.GeoUpdatedAt = source.GeoUpdatedAt
		target.GeoError = source.GeoError
	}
	if target.IfconfigUpdatedAt.IsZero() || source.IfconfigUpdatedAt.After(target.IfconfigUpdatedAt) {
		target.IfconfigIP = source.IfconfigIP
		target.IfconfigCountry = source.IfconfigCountry
		target.IfconfigCountryCode = source.IfconfigCountryCode
		target.IfconfigASN = source.IfconfigASN
		target.IfconfigOrg = source.IfconfigOrg
		target.IfconfigUpdatedAt = source.IfconfigUpdatedAt
		target.IfconfigError = source.IfconfigError
	}
}

func rewriteIncident(incident *nodearchive.IncidentRecord, sourceID, targetID, targetName string) {
	if incident == nil {
		return
	}
	if incident.Scope == "node:"+sourceID {
		incident.Scope = "node:" + targetID
		incident.StableIDs = []string{targetID}
		incident.NodeNames = []string{targetName}
		incident.AffectedCount = 1
		incident.TotalCount = 1
		return
	}
	if len(incident.StableIDs) == 0 {
		return
	}
	namesByID := make(map[string]string)
	for index, stableID := range incident.StableIDs {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		name := ""
		if index < len(incident.NodeNames) {
			name = incident.NodeNames[index]
		}
		if stableID == sourceID {
			stableID = targetID
			name = targetName
		}
		if stableID == targetID && name == "" {
			name = targetName
		}
		if _, exists := namesByID[stableID]; !exists || name != "" {
			namesByID[stableID] = name
		}
	}
	stableIDs := make([]string, 0, len(namesByID))
	for stableID := range namesByID {
		stableIDs = append(stableIDs, stableID)
	}
	sort.Strings(stableIDs)
	names := make([]string, 0, len(stableIDs))
	for _, stableID := range stableIDs {
		names = append(names, namesByID[stableID])
	}
	incident.StableIDs = stableIDs
	incident.NodeNames = names
	incident.AffectedCount = len(stableIDs)
}

func readValidatedStateFile(dataDir, name string) ([]byte, error) {
	path := filepath.Join(dataDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect node merge state file %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("node merge state file %s is not regular", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read node merge state file %s: %w", name, err)
	}
	if err := backup.ValidateDataFile(name, data); err != nil {
		return nil, err
	}
	return data, nil
}

func loadInstruction(dir string) (pendingInstruction, error) {
	data, err := os.ReadFile(filepath.Join(dir, instructionFileName))
	if err != nil {
		return pendingInstruction{}, fmt.Errorf("read pending node merge: %w", err)
	}
	var instruction pendingInstruction
	if err := json.Unmarshal(data, &instruction); err != nil {
		return pendingInstruction{}, fmt.Errorf("decode pending node merge: %w", err)
	}
	if instruction.FormatVersion != transactionFormatVersion || instruction.CreatedAt.IsZero() ||
		strings.TrimSpace(instruction.SourceStableID) == "" || strings.TrimSpace(instruction.TargetStableID) == "" ||
		strings.TrimSpace(instruction.ConfirmationToken) == "" {
		return pendingInstruction{}, fmt.Errorf("pending node merge instruction is incomplete or unsupported")
	}
	return instruction, nil
}

func writeDurableStateFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("staged node merge file %s is not regular", filepath.Base(path))
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace staged node merge file %s: %w", filepath.Base(path), err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect staged node merge file %s: %w", filepath.Base(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create staged node merge file %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write staged node merge file %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync staged node merge file %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close staged node merge file %s: %w", filepath.Base(path), err)
	}
	return nil
}

func rollbackTransactionLocked(dataDir, stateDirName string, keepState bool) error {
	statePath := filepath.Join(dataDir, stateDirName)
	rollbackPath := filepath.Join(dataDir, rollbackDirName)
	for _, name := range stateFileNames {
		destination := filepath.Join(dataDir, name)
		staged := filepath.Join(statePath, name)
		original := filepath.Join(rollbackPath, name)
		destinationExists, err := regularFileExists(destination, "node merge target "+name)
		if err != nil {
			return err
		}
		stagedExists, err := regularFileExists(staged, "staged node merge file "+name)
		if err != nil {
			return err
		}
		originalExists, err := regularFileExists(original, "node merge rollback file "+name)
		if err != nil {
			return err
		}
		if !originalExists {
			continue
		}
		if !stagedExists && destinationExists {
			if err := os.Rename(destination, staged); err != nil {
				return fmt.Errorf("return merged %s to staging: %w", name, err)
			}
			destinationExists = false
		}
		if destinationExists {
			return fmt.Errorf("node merge target %s unexpectedly exists while original is staged", name)
		}
		if err := os.Rename(original, destination); err != nil {
			return fmt.Errorf("restore original node merge file %s: %w", name, err)
		}
	}
	if err := os.RemoveAll(rollbackPath); err != nil {
		return fmt.Errorf("remove node merge rollback directory: %w", err)
	}
	if !keepState {
		if err := os.RemoveAll(statePath); err != nil {
			return fmt.Errorf("remove node merge state directory: %w", err)
		}
	}
	return nil
}

func inspectTransactionDirs(dataDir string) (pending, applied, rollback bool, err error) {
	pending, err = inspectStateDir(dataDir, pendingDirName)
	if err != nil {
		return false, false, false, err
	}
	applied, err = inspectStateDir(dataDir, appliedDirName)
	if err != nil {
		return false, false, false, err
	}
	rollback, err = inspectStateDir(dataDir, rollbackDirName)
	return pending, applied, rollback, err
}

func transactionExistsLocked(dataDir string) (bool, error) {
	pending, applied, rollback, err := inspectTransactionDirs(dataDir)
	return pending || applied || rollback, err
}

func ensureNoRestoreTransaction(dataDir string) error {
	for _, name := range restoreDirNames {
		exists, err := inspectStateDir(dataDir, name)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("node merge cannot run while a backup restore is pending or being applied")
		}
	}
	return nil
}

func inspectStateDir(dataDir, name string) (bool, error) {
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

func regularFileExists(path, label string) (bool, error) {
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

func writeCommitMarker(dataDir string) error {
	marker := filepath.Join(dataDir, rollbackDirName, commitMarkerName)
	if marked, err := commitMarked(dataDir); err != nil {
		return err
	} else if marked {
		return nil
	}
	return writeDurableStateFile(marker, []byte("confirmed\n"))
}

func commitMarked(dataDir string) (bool, error) {
	marker := filepath.Join(dataDir, rollbackDirName, commitMarkerName)
	exists, err := regularFileExists(marker, "node merge commit marker")
	if err != nil || !exists {
		return exists, err
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return false, fmt.Errorf("read node merge commit marker: %w", err)
	}
	if string(data) != "confirmed\n" {
		return false, fmt.Errorf("node merge commit marker has invalid contents")
	}
	return true, nil
}

func cleanupTransactionLocked(dataDir string, appliedExists, rollbackExists bool) error {
	if appliedExists {
		if err := os.RemoveAll(filepath.Join(dataDir, appliedDirName)); err != nil {
			return fmt.Errorf("remove applied node merge directory: %w", err)
		}
	}
	if rollbackExists {
		if err := os.RemoveAll(filepath.Join(dataDir, rollbackDirName)); err != nil {
			return fmt.Errorf("remove node merge rollback directory: %w", err)
		}
	}
	return nil
}

func earliestNonZero(first, second time.Time) time.Time {
	if first.IsZero() || (!second.IsZero() && second.Before(first)) {
		return second
	}
	return first
}

func latestTime(first, second time.Time) time.Time {
	if second.After(first) {
		return second
	}
	return first
}

func addInt64(first, second int64) (int64, error) {
	if second > 0 && first > math.MaxInt64-second {
		return 0, fmt.Errorf("integer overflow")
	}
	if second < 0 && first < math.MinInt64-second {
		return 0, fmt.Errorf("integer underflow")
	}
	return first + second, nil
}

func addInt(first, second int) (int, error) {
	if second > 0 && first > int(^uint(0)>>1)-second {
		return 0, fmt.Errorf("integer overflow")
	}
	return first + second, nil
}

func uniqueSorted(values []string, excluded string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == excluded || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
