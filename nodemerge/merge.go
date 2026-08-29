package nodemerge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xray-checker/nodearchive"
	"xray-checker/speedtest"
)

const transactionFormatVersion = 1

type NodeSnapshot struct {
	StableID             string    `json:"stableId"`
	Name                 string    `json:"name"`
	SubName              string    `json:"subName"`
	Server               string    `json:"server"`
	Port                 int       `json:"port"`
	Protocol             string    `json:"protocol"`
	Active               bool      `json:"active"`
	FirstSeenAt          time.Time `json:"firstSeenAt,omitempty"`
	LastSeenAt           time.Time `json:"lastSeenAt,omitempty"`
	ResultCount          int       `json:"resultCount"`
	TotalDowntimeSec     int64     `json:"totalDowntimeSec"`
	TotalProxyFailureSec int64     `json:"totalProxyFailureSec"`
	IncidentCount        int       `json:"incidentCount"`
	ProxyFailureCount    int       `json:"proxyFailureCount"`
}

type Preview struct {
	Source              NodeSnapshot `json:"source"`
	Target              NodeSnapshot `json:"target"`
	MergedResultCount   int          `json:"mergedResultCount"`
	ConfirmationToken   string       `json:"confirmationToken"`
	RestartRequired     bool         `json:"restartRequired"`
	IdentityFieldsMatch bool         `json:"identityFieldsMatch"`
	IdentityWarnings    []string     `json:"identityWarnings,omitempty"`
}

type StageResult struct {
	SourceStableID  string `json:"sourceStableId"`
	TargetStableID  string `json:"targetStableId"`
	RestartRequired bool   `json:"restartRequired"`
	Message         string `json:"message"`
}

type Coordinator struct {
	dataDir string
	store   *nodearchive.Store
	manager *speedtest.Manager
	now     func() time.Time
}

type pendingInstruction struct {
	FormatVersion     int       `json:"formatVersion"`
	CreatedAt         time.Time `json:"createdAt"`
	SourceStableID    string    `json:"sourceStableId"`
	TargetStableID    string    `json:"targetStableId"`
	ConfirmationToken string    `json:"confirmationToken"`
}

type confirmationIdentity struct {
	SourceStableID string    `json:"sourceStableId"`
	TargetStableID string    `json:"targetStableId"`
	SourceRetired  time.Time `json:"sourceRetiredAt"`
	Name           string    `json:"name"`
	SubName        string    `json:"subName"`
	Server         string    `json:"server"`
	Port           int       `json:"port"`
	Protocol       string    `json:"protocol"`
}

func NewCoordinator(dataDir string, store *nodearchive.Store, manager *speedtest.Manager) *Coordinator {
	return &Coordinator{
		dataDir: strings.TrimSpace(dataDir),
		store:   store,
		manager: manager,
		now:     time.Now,
	}
}

func (c *Coordinator) Preview(sourceStableID, targetStableID string) (Preview, error) {
	if c == nil || c.store == nil || c.manager == nil {
		return Preview{}, fmt.Errorf("node merge coordinator is not configured")
	}
	sourceStableID = strings.TrimSpace(sourceStableID)
	targetStableID = strings.TrimSpace(targetStableID)
	if sourceStableID == "" || targetStableID == "" {
		return Preview{}, fmt.Errorf("sourceStableId and targetStableId are required")
	}
	if strings.EqualFold(sourceStableID, targetStableID) {
		return Preview{}, fmt.Errorf("source and target nodes must be different")
	}

	source, err := c.store.ArchivedRecord(sourceStableID)
	if err != nil {
		return Preview{}, fmt.Errorf("source node: %w", err)
	}
	target, err := c.store.Record(targetStableID)
	if err != nil {
		return Preview{}, fmt.Errorf("target node: %w", err)
	}
	if !target.Active {
		return Preview{}, fmt.Errorf("target node must be active")
	}
	if err := validateIdentityMatch(source, target); err != nil {
		return Preview{}, err
	}

	sourceHistory := c.manager.ResultHistory(sourceStableID)
	targetHistory := c.manager.ResultHistory(targetStableID)
	mergedHistory := mergeHistory(sourceHistory, targetHistory, target)
	warnings := identityWarnings(source, target)

	return Preview{
		Source:              nodeSnapshot(source, len(sourceHistory)),
		Target:              nodeSnapshot(target, len(targetHistory)),
		MergedResultCount:   len(mergedHistory),
		ConfirmationToken:   confirmationToken(source, target),
		RestartRequired:     true,
		IdentityFieldsMatch: len(warnings) == 0,
		IdentityWarnings:    warnings,
	}, nil
}

func (c *Coordinator) Stage(sourceStableID, targetStableID, confirmation string) (StageResult, error) {
	if c == nil || c.dataDir == "" {
		return StageResult{}, fmt.Errorf("node merge data directory is required")
	}
	preview, err := c.Preview(sourceStableID, targetStableID)
	if err != nil {
		return StageResult{}, err
	}
	confirmation = strings.TrimSpace(confirmation)
	if confirmation == "" || subtle.ConstantTimeCompare([]byte(confirmation), []byte(preview.ConfirmationToken)) != 1 {
		return StageResult{}, fmt.Errorf("node merge candidate changed since preview; review it again")
	}

	transactionMu.Lock()
	defer transactionMu.Unlock()
	if err := ensureNoRestoreTransaction(c.dataDir); err != nil {
		return StageResult{}, err
	}
	pendingExists, appliedExists, rollbackExists, err := inspectTransactionDirs(c.dataDir)
	if err != nil {
		return StageResult{}, err
	}
	if appliedExists || rollbackExists {
		return StageResult{}, fmt.Errorf("another node merge is already pending or being applied")
	}
	if pendingExists {
		existing, err := loadInstruction(filepath.Join(c.dataDir, pendingDirName))
		if err != nil {
			return StageResult{}, err
		}
		if existing.SourceStableID == preview.Source.StableID && existing.TargetStableID == preview.Target.StableID && existing.ConfirmationToken == preview.ConfirmationToken {
			return stageResult(preview), nil
		}
		if err := os.RemoveAll(filepath.Join(c.dataDir, pendingDirName)); err != nil {
			return StageResult{}, fmt.Errorf("replace pending node merge: %w", err)
		}
	}
	if err := os.MkdirAll(c.dataDir, 0700); err != nil {
		return StageResult{}, fmt.Errorf("create node merge data directory: %w", err)
	}

	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	instruction := pendingInstruction{
		FormatVersion:     transactionFormatVersion,
		CreatedAt:         now,
		SourceStableID:    preview.Source.StableID,
		TargetStableID:    preview.Target.StableID,
		ConfirmationToken: preview.ConfirmationToken,
	}
	data, err := json.MarshalIndent(instruction, "", "  ")
	if err != nil {
		return StageResult{}, fmt.Errorf("encode node merge instruction: %w", err)
	}
	data = append(data, '\n')

	stageDir, err := os.MkdirTemp(c.dataDir, ".node-merge-stage-*")
	if err != nil {
		return StageResult{}, fmt.Errorf("create node merge staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stageDir)
		}
	}()
	if err := os.WriteFile(filepath.Join(stageDir, instructionFileName), data, 0600); err != nil {
		return StageResult{}, fmt.Errorf("write node merge instruction: %w", err)
	}
	if err := os.Rename(stageDir, filepath.Join(c.dataDir, pendingDirName)); err != nil {
		return StageResult{}, fmt.Errorf("publish pending node merge: %w", err)
	}
	cleanup = false

	return stageResult(preview), nil
}

func stageResult(preview Preview) StageResult {
	return StageResult{
		SourceStableID:  preview.Source.StableID,
		TargetStableID:  preview.Target.StableID,
		RestartRequired: true,
		Message:         "Node merge staged; restart the application to apply it",
	}
}

// AcquireRestoreGuard serializes backup staging with node-merge staging. The
// returned release function must be held until backup.Restorer.Stage finishes.
func (c *Coordinator) AcquireRestoreGuard() (func(), error) {
	if c == nil || c.dataDir == "" {
		return func() {}, nil
	}
	transactionMu.Lock()
	busy, err := transactionExistsLocked(c.dataDir)
	if err != nil {
		transactionMu.Unlock()
		return nil, err
	}
	if busy {
		transactionMu.Unlock()
		return nil, fmt.Errorf("backup restore cannot be staged while a node merge is pending or being applied")
	}
	return transactionMu.Unlock, nil
}

func validateIdentityMatch(source, target nodearchive.NodeRecord) error {
	if normalizeIdentity(source.SubName) != normalizeIdentity(target.SubName) {
		return fmt.Errorf("source and target subscriptions do not match")
	}
	if normalizeIdentity(source.Protocol) != normalizeIdentity(target.Protocol) {
		return fmt.Errorf("source and target protocols do not match")
	}
	if normalizeIdentity(source.Server) != normalizeIdentity(target.Server) {
		return fmt.Errorf("source and target servers do not match")
	}
	return nil
}

func identityWarnings(source, target nodearchive.NodeRecord) []string {
	var warnings []string
	if normalizeIdentity(source.Name) != normalizeIdentity(target.Name) {
		warnings = append(warnings, "Node name changed")
	}
	if source.Port != target.Port {
		warnings = append(warnings, fmt.Sprintf("Port changed from %d to %d", source.Port, target.Port))
	}
	return warnings
}

func normalizeIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func confirmationToken(source, target nodearchive.NodeRecord) string {
	payload := confirmationIdentity{
		SourceStableID: strings.TrimSpace(source.StableID),
		TargetStableID: strings.TrimSpace(target.StableID),
		SourceRetired:  source.RetiredAt.UTC(),
		Name:           normalizeIdentity(target.Name),
		SubName:        normalizeIdentity(target.SubName),
		Server:         normalizeIdentity(target.Server),
		Port:           target.Port,
		Protocol:       normalizeIdentity(target.Protocol),
	}
	data, _ := json.Marshal(payload)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func nodeSnapshot(record nodearchive.NodeRecord, resultCount int) NodeSnapshot {
	return NodeSnapshot{
		StableID:             record.StableID,
		Name:                 record.Name,
		SubName:              record.SubName,
		Server:               record.Server,
		Port:                 record.Port,
		Protocol:             record.Protocol,
		Active:               record.Active,
		FirstSeenAt:          record.FirstSeenAt,
		LastSeenAt:           record.LastSeenAt,
		ResultCount:          resultCount,
		TotalDowntimeSec:     record.TotalDowntimeSec,
		TotalProxyFailureSec: record.TotalProxyFailureSec,
		IncidentCount:        record.IncidentCount,
		ProxyFailureCount:    record.ProxyFailureCount,
	}
}
