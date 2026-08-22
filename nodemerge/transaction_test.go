package nodemerge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/nodearchive"
	"xray-checker/speedtest"
)

const (
	testSourceID = "retired-source"
	testTargetID = "active-target"
)

func TestApplyPendingMergesPersistedStateAndConfirms(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	originalRegistry := writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)
	stageInstructionFixture(t, dataDir, registry.Nodes[testSourceID], registry.Nodes[testTargetID])

	applied, err := ApplyPending(dataDir, map[string]bool{testTargetID: true})
	if err != nil {
		t.Fatalf("apply pending merge: %v", err)
	}
	if !applied {
		t.Fatal("expected pending merge to be applied")
	}
	assertDirExists(t, filepath.Join(dataDir, appliedDirName))
	assertDirExists(t, filepath.Join(dataDir, rollbackDirName))
	if got := mustReadFile(t, filepath.Join(dataDir, rollbackDirName, nodeRegistryName)); !bytes.Equal(got, originalRegistry) {
		t.Fatal("rollback copy does not match the original node registry")
	}

	var mergedRegistry nodearchive.StateFile
	readJSONFile(t, filepath.Join(dataDir, nodeRegistryName), &mergedRegistry)
	if _, ok := mergedRegistry.Nodes[testSourceID]; ok {
		t.Fatal("retired source still exists after merge")
	}
	target := mergedRegistry.Nodes[testTargetID]
	if !target.Active || target.StableID != testTargetID {
		t.Fatalf("target identity changed incorrectly: %#v", target)
	}
	if target.TotalDowntimeSec != 450 || target.IncidentCount != 7 || target.LongestDowntimeSec != 360 {
		t.Fatalf("availability counters were not merged: %#v", target)
	}
	wantFirstSeen := registry.Nodes[testSourceID].FirstSeenAt
	if !target.FirstSeenAt.Equal(wantFirstSeen) {
		t.Fatalf("first seen = %s, want %s", target.FirstSeenAt, wantFirstSeen)
	}
	if target.GeoCountry != "Estonia" || target.GeoIP != "94.156.236.38" {
		t.Fatalf("newer source GeoIP data was not retained: %#v", target)
	}
	if got, want := mergedRegistry.MergedNodes[testTargetID], []string{"earlier-source", "old-key", testSourceID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage = %#v, want %#v", got, want)
	}
	if len(mergedRegistry.Incidents) != 2 {
		t.Fatalf("incident count = %d, want 2", len(mergedRegistry.Incidents))
	}
	if incident := mergedRegistry.Incidents[0]; incident.ID != "node-incident" || incident.Scope != "node:"+testTargetID || !reflect.DeepEqual(incident.StableIDs, []string{testTargetID}) {
		t.Fatalf("node incident was not re-keyed safely: %#v", incident)
	}
	if incident := mergedRegistry.Incidents[1]; incident.ID != "mass-incident" || !reflect.DeepEqual(incident.StableIDs, []string{testTargetID, "another-node"}) || incident.AffectedCount != 2 {
		t.Fatalf("mass incident was not de-duplicated: %#v", incident)
	}

	var mergedSpeed speedResultState
	readJSONFile(t, filepath.Join(dataDir, speedResultsName), &mergedSpeed)
	if _, ok := mergedSpeed.History[testSourceID]; ok {
		t.Fatal("retired source speed history still exists")
	}
	history := mergedSpeed.History[testTargetID]
	if len(history) != 3 {
		t.Fatalf("merged history length = %d, want 3", len(history))
	}
	for _, result := range history {
		if result.StableID != testTargetID || result.Name != "Estonia" || result.SubName != "InvisibleProxy" || result.Protocol != "vless" {
			t.Fatalf("history result was not normalized to active identity: %#v", result)
		}
	}
	if !history[0].CheckedAt.After(history[1].CheckedAt) || !history[1].CheckedAt.After(history[2].CheckedAt) {
		t.Fatalf("merged history is not newest-first: %#v", history)
	}
	if mergedSpeed.Results[testTargetID].CheckedAt != history[0].CheckedAt {
		t.Fatal("latest result does not match newest merged history entry")
	}

	store := nodearchive.NewStore(filepath.Join(dataDir, nodeRegistryName), nil)
	if err := store.Load(); err != nil {
		t.Fatalf("load merged node registry: %v", err)
	}
	summaries := store.Summaries(nil)
	if len(summaries) != 1 || !reflect.DeepEqual(summaries[0].MergedFromStableIDs, []string{"earlier-source", "old-key", testSourceID}) {
		t.Fatalf("loaded merge lineage = %#v", summaries)
	}

	if err := ConfirmApplied(dataDir); err != nil {
		t.Fatalf("confirm applied merge: %v", err)
	}
	assertPathMissing(t, filepath.Join(dataDir, appliedDirName))
	assertPathMissing(t, filepath.Join(dataDir, rollbackDirName))
	if recovered, err := RecoverUnconfirmed(dataDir); err != nil || recovered {
		t.Fatalf("recover confirmed merge = %v, %v; want false, nil", recovered, err)
	}
}

func TestRecoverUnconfirmedRestoresExactOriginalFiles(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	originalRegistry := writeJSONFile(t, dataDir, nodeRegistryName, registry)
	originalSpeed := writeJSONFile(t, dataDir, speedResultsName, speedState)
	stageInstructionFixture(t, dataDir, registry.Nodes[testSourceID], registry.Nodes[testTargetID])

	if applied, err := ApplyPending(dataDir, map[string]bool{testTargetID: true}); err != nil || !applied {
		t.Fatalf("apply pending merge = %v, %v", applied, err)
	}
	recovered, err := RecoverUnconfirmed(dataDir)
	if err != nil {
		t.Fatalf("recover unconfirmed merge: %v", err)
	}
	if !recovered {
		t.Fatal("expected unconfirmed merge recovery")
	}
	if got := mustReadFile(t, filepath.Join(dataDir, nodeRegistryName)); !bytes.Equal(got, originalRegistry) {
		t.Fatal("node registry was not restored byte-for-byte")
	}
	if got := mustReadFile(t, filepath.Join(dataDir, speedResultsName)); !bytes.Equal(got, originalSpeed) {
		t.Fatal("speed history was not restored byte-for-byte")
	}
	assertPathMissing(t, filepath.Join(dataDir, appliedDirName))
	assertPathMissing(t, filepath.Join(dataDir, rollbackDirName))
}

func TestRecoverCommittedMergeFinishesInterruptedCleanup(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)
	stageInstructionFixture(t, dataDir, registry.Nodes[testSourceID], registry.Nodes[testTargetID])
	if applied, err := ApplyPending(dataDir, map[string]bool{testTargetID: true}); err != nil || !applied {
		t.Fatalf("apply pending merge = %v, %v", applied, err)
	}
	if err := writeCommitMarker(dataDir); err != nil {
		t.Fatalf("write commit marker: %v", err)
	}

	recovered, err := RecoverUnconfirmed(dataDir)
	if err != nil || !recovered {
		t.Fatalf("recover committed merge = %v, %v", recovered, err)
	}
	var merged nodearchive.StateFile
	readJSONFile(t, filepath.Join(dataDir, nodeRegistryName), &merged)
	if _, ok := merged.Nodes[testSourceID]; ok {
		t.Fatal("committed merge was incorrectly rolled back")
	}
	assertPathMissing(t, filepath.Join(dataDir, appliedDirName))
	assertPathMissing(t, filepath.Join(dataDir, rollbackDirName))
}

func TestApplyPendingRejectsChangedActiveScopeWithoutTouchingState(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	originalRegistry := writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)
	stageInstructionFixture(t, dataDir, registry.Nodes[testSourceID], registry.Nodes[testTargetID])

	applied, err := ApplyPending(dataDir, map[string]bool{"some-other-node": true})
	if err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("error = %v, want target no longer active", err)
	}
	if applied {
		t.Fatal("merge unexpectedly applied")
	}
	if got := mustReadFile(t, filepath.Join(dataDir, nodeRegistryName)); !bytes.Equal(got, originalRegistry) {
		t.Fatal("node registry changed after rejected merge")
	}
	assertDirExists(t, filepath.Join(dataDir, pendingDirName))
}

func TestApplyPendingNormalizesLegacyEmptyRecordStableIDs(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	source := registry.Nodes[testSourceID]
	target := registry.Nodes[testTargetID]
	source.StableID = ""
	target.StableID = ""
	registry.Nodes[testSourceID] = source
	registry.Nodes[testTargetID] = target
	writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)
	source.StableID = testSourceID
	target.StableID = testTargetID
	stageInstructionFixture(t, dataDir, source, target)

	applied, err := ApplyPending(dataDir, map[string]bool{testTargetID: true})
	if err != nil || !applied {
		t.Fatalf("apply legacy node registry merge = %v, %v", applied, err)
	}
	var merged nodearchive.StateFile
	readJSONFile(t, filepath.Join(dataDir, nodeRegistryName), &merged)
	if got := merged.Nodes[testTargetID].StableID; got != testTargetID {
		t.Fatalf("normalized target StableID = %q, want %q", got, testTargetID)
	}
}

func TestCoordinatorRequiresPreviewTokenAndBlocksRestore(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)

	coordinator := testCoordinator(t, dataDir)
	preview, err := coordinator.Preview(testSourceID, testTargetID)
	if err != nil {
		t.Fatalf("preview merge: %v", err)
	}
	if preview.Source.ResultCount != 2 || preview.Target.ResultCount != 1 || preview.MergedResultCount != 3 || preview.ConfirmationToken == "" {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	if _, err := coordinator.Stage(testSourceID, testTargetID, "wrong-token"); err == nil || !strings.Contains(err.Error(), "candidate changed") {
		t.Fatalf("stage with wrong token error = %v", err)
	}
	assertPathMissing(t, filepath.Join(dataDir, pendingDirName))

	result, err := coordinator.Stage(testSourceID, testTargetID, preview.ConfirmationToken)
	if err != nil {
		t.Fatalf("stage merge: %v", err)
	}
	if !result.RestartRequired || result.SourceStableID != testSourceID || result.TargetStableID != testTargetID {
		t.Fatalf("unexpected stage result: %#v", result)
	}
	if _, err := coordinator.AcquireRestoreGuard(); err == nil || !strings.Contains(err.Error(), "backup restore") {
		t.Fatalf("restore guard error = %v", err)
	}
	if repeated, err := coordinator.Stage(testSourceID, testTargetID, preview.ConfirmationToken); err != nil || !repeated.RestartRequired {
		t.Fatalf("idempotent second stage = %#v, %v", repeated, err)
	}
}

func TestRestoreGuardSerializesMergeStaging(t *testing.T) {
	dataDir := t.TempDir()
	registry, speedState := testStateFixture()
	writeJSONFile(t, dataDir, nodeRegistryName, registry)
	writeJSONFile(t, dataDir, speedResultsName, speedState)
	coordinator := testCoordinator(t, dataDir)
	preview, err := coordinator.Preview(testSourceID, testTargetID)
	if err != nil {
		t.Fatalf("preview merge: %v", err)
	}

	release, err := coordinator.AcquireRestoreGuard()
	if err != nil {
		t.Fatalf("acquire restore guard: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, stageErr := coordinator.Stage(testSourceID, testTargetID, preview.ConfirmationToken)
		done <- stageErr
	}()
	select {
	case stageErr := <-done:
		release()
		t.Fatalf("merge staging crossed the held restore guard: %v", stageErr)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case stageErr := <-done:
		if stageErr != nil {
			t.Fatalf("stage merge after releasing restore guard: %v", stageErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("merge staging did not resume after releasing restore guard")
	}
}

func testStateFixture() (nodearchive.StateFile, speedResultState) {
	now := time.Now().UTC().Truncate(time.Second)
	source := nodearchive.NodeRecord{
		StableID:           testSourceID,
		Name:               "Estonia",
		SubName:            "InvisibleProxy",
		Server:             "94.156.236.38",
		Port:               3128,
		Protocol:           "vless",
		Active:             false,
		FirstSeenAt:        now.Add(-72 * time.Hour),
		LastSeenAt:         now.Add(-2 * time.Hour),
		RetiredAt:          now.Add(-90 * time.Minute),
		GeoIP:              "94.156.236.38",
		GeoCountry:         "Estonia",
		GeoCountryCode:     "EE",
		GeoUpdatedAt:       now.Add(-time.Hour),
		TotalDowntimeSec:   420,
		IncidentCount:      5,
		LongestDowntimeSec: 360,
	}
	target := nodearchive.NodeRecord{
		StableID:           testTargetID,
		Name:               "Estonia",
		SubName:            "InvisibleProxy",
		Server:             "94.156.236.38",
		Port:               3128,
		Protocol:           "vless",
		Active:             true,
		FirstSeenAt:        now.Add(-time.Hour),
		LastSeenAt:         now,
		GeoCountry:         "Unknown",
		GeoUpdatedAt:       now.Add(-2 * time.Hour),
		TotalDowntimeSec:   30,
		IncidentCount:      2,
		LongestDowntimeSec: 20,
	}
	registry := nodearchive.StateFile{
		Version:   1,
		UpdatedAt: now,
		Nodes: map[string]nodearchive.NodeRecord{
			testSourceID: source,
			testTargetID: target,
		},
		MergedNodes: map[string][]string{
			testSourceID: {"earlier-source"},
			testTargetID: {"old-key"},
		},
		Incidents: []nodearchive.IncidentRecord{
			{
				ID: "node-incident", Kind: "node", Status: "resolved", Scope: "node:" + testSourceID,
				StableIDs: []string{testSourceID}, NodeNames: []string{"Estonia"}, AffectedCount: 1, TotalCount: 1,
				CauseCode: "tcp_refused", CauseSummary: "TCP refused", StartedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-23 * time.Hour),
			},
			{
				ID: "mass-incident", Kind: "mass", Status: "resolved", Scope: "subscription:InvisibleProxy", Subscription: "InvisibleProxy",
				StableIDs: []string{testSourceID, testTargetID, "another-node"}, NodeNames: []string{"Estonia", "Estonia", "Other"}, AffectedCount: 3, TotalCount: 3,
				CauseCode: "timeout", CauseSummary: "Timeout", StartedAt: now.Add(-12 * time.Hour), UpdatedAt: now.Add(-11 * time.Hour),
			},
		},
	}
	result := func(id string, checkedAt time.Time, mbps float64) speedtest.Result {
		return speedtest.Result{
			StableID: id, Name: "Estonia", SubName: "InvisibleProxy", Protocol: "vless",
			URL: "https://speed.example/test", StatusCode: 200, DownloadedBytes: 1024, DurationMs: 1000,
			Mbps: mbps, CheckedAt: checkedAt, Source: "schedule",
		}
	}
	sourceLatest := result(testSourceID, now.Add(-2*time.Hour), 400)
	sourceOlder := result(testSourceID, now.Add(-3*time.Hour), 350)
	targetLatest := result(testTargetID, now.Add(-time.Hour), 450)
	speedState := speedResultState{
		Version:   1,
		UpdatedAt: now,
		Results: map[string]speedtest.Result{
			testSourceID: sourceLatest,
			testTargetID: targetLatest,
		},
		History: map[string][]speedtest.Result{
			testSourceID: {sourceLatest, sourceOlder},
			testTargetID: {targetLatest},
		},
	}
	return registry, speedState
}

func stageInstructionFixture(t *testing.T, dataDir string, source, target nodearchive.NodeRecord) {
	t.Helper()
	pendingPath := filepath.Join(dataDir, pendingDirName)
	if err := os.Mkdir(pendingPath, 0700); err != nil {
		t.Fatalf("create pending merge directory: %v", err)
	}
	instruction := pendingInstruction{
		FormatVersion:     transactionFormatVersion,
		CreatedAt:         time.Now().UTC(),
		SourceStableID:    source.StableID,
		TargetStableID:    target.StableID,
		ConfirmationToken: confirmationToken(source, target),
	}
	writeJSONFile(t, pendingPath, instructionFileName, instruction)
}

func testCoordinator(t *testing.T, dataDir string) *Coordinator {
	t.Helper()
	store := nodearchive.NewStore(filepath.Join(dataDir, nodeRegistryName), nil)
	if err := store.Load(); err != nil {
		t.Fatalf("load node registry: %v", err)
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{{
		StableID: testTargetID,
		Name:     "Estonia",
		SubName:  "InvisibleProxy",
		Server:   "94.156.236.38",
		Port:     3128,
		Protocol: "vless",
	}}, 10000, "", 1, "", "", 1, 1, "")
	manager := speedtest.NewManager(proxyChecker, 10000, filepath.Join(dataDir, "speedtest_schedule.json"), speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatalf("load speed manager: %v", err)
	}
	return NewCoordinator(dataDir, store, manager)
}

func writeJSONFile(t *testing.T, dir, name string, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return data
}

func readJSONFile(t *testing.T, path string, destination any) {
	t.Helper()
	data := mustReadFile(t, path)
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertDirExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory %s does not exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path %s unexpectedly exists (err=%v)", path, err)
	}
}
