package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xray-checker/speedtest"
)

func muteTestService(t *testing.T, dir string) (*Service, string) {
	t.Helper()
	proxies := testProxies("alpha", "beta")
	statePath := filepath.Join(dir, "telegram_config.json")
	service := NewService(statePath, testChecker(proxies), nil, 10000)
	service.setConfig(Config{
		Enabled:               true,
		BotToken:              "token",
		ChatID:                "1",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "always",
		NodeAlertsEnabled:     true,
		LowSpeedThresholdMbps: 10,
		TimeoutSec:            5,
	})
	return service, proxies[0].StableID
}

func TestTemporaryMuteSilencesBothScopesUntilItExpires(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, muteScopeAll, time.Hour); err != nil {
		t.Fatalf("failed to mute node: %v", err)
	}

	cfg := service.Config()
	if !service.alertMuteSet(cfg)[stableID] {
		t.Fatal("an all-scope mute must cover availability alerts")
	}
	if !service.speedMuteSet(cfg)[stableID] {
		t.Fatal("an all-scope mute must cover speed reports")
	}
	// A temporary mute is runtime state, not an operator preference in the
	// admin UI, so the editable config must stay untouched.
	if len(cfg.MutedNodeIDs) != 0 || len(cfg.MutedAlertNodeIDs) != 0 || len(cfg.MutedSpeedNodeIDs) != 0 {
		t.Fatalf("a timed mute must not be written to the editable config: %+v", cfg)
	}

	service.mu.Lock()
	service.mutes[stableID] = nodeMute{Scope: muteScopeAll, Until: time.Now().Add(-time.Minute)}
	service.mu.Unlock()
	if service.alertMuteSet(service.Config())[stableID] {
		t.Fatal("an expired mute must stop silencing the node")
	}
}

func TestScopedTemporaryMuteLeavesTheOtherChannelAlone(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, muteScopeSpeed, time.Hour); err != nil {
		t.Fatalf("failed to mute node: %v", err)
	}

	cfg := service.Config()
	if !service.speedMuteSet(cfg)[stableID] {
		t.Fatal("a speed-scoped mute must silence speed reports")
	}
	if service.alertMuteSet(cfg)[stableID] {
		t.Fatal("a speed-scoped mute must leave availability alerts audible")
	}
}

func TestTemporaryMuteRemovesNodeFromSpeedReport(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)
	if err := service.muteNodeFor(stableID, muteScopeSpeed, time.Hour); err != nil {
		t.Fatalf("failed to mute node: %v", err)
	}

	cfg := service.Config()
	report := speedtest.RunReport{
		Source: "schedule",
		Results: []speedtest.Result{
			{StableID: stableID, Name: "alpha", Mbps: 1},
			{StableID: "other", Name: "beta", Mbps: 1},
		},
	}
	filtered := filterRunReportByMuteSet(report, service.speedMuteSet(cfg))
	if len(filtered.Results) != 1 || filtered.Results[0].StableID != "other" {
		t.Fatalf("muted node still present in the report: %+v", filtered.Results)
	}
	if _, _, _, shouldSend := speedReportDecisionWithMutes(report, cfg, service.speedMuteSet(cfg)); !shouldSend {
		t.Fatal("the remaining node should still produce a report")
	}
}

func TestPermanentMuteFromBotIsVisibleToAdminConfig(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, muteScopeAll, 0); err != nil {
		t.Fatalf("failed to mute node permanently: %v", err)
	}
	// A permanent mute belongs in the editable config so the admin UI shows it
	// next to the mutes set there and can lift it.
	if got := service.AdminConfig().MutedNodeIDs; len(got) != 1 || got[0] != stableID {
		t.Fatalf("permanent mute is not visible in the admin config: %v", got)
	}

	status := service.nodeMuteStatusFor(stableID, service.Config())
	if !status.Muted() || !status.Permanent {
		t.Fatalf("expected a permanent mute status, got %+v", status)
	}

	if err := service.unmuteNode(stableID); err != nil {
		t.Fatalf("failed to unmute node: %v", err)
	}
	if got := service.AdminConfig().MutedNodeIDs; len(got) != 0 {
		t.Fatalf("unmute must clear the config entry, got %v", got)
	}
	if service.nodeMuteStatusFor(stableID, service.Config()).Muted() {
		t.Fatal("node must not stay muted after unmute")
	}
}

func TestPermanentMuteSupersedesRunningCountdown(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, muteScopeAll, time.Hour); err != nil {
		t.Fatalf("failed to mute node: %v", err)
	}
	if err := service.muteNodeFor(stableID, muteScopeAll, 0); err != nil {
		t.Fatalf("failed to mute node permanently: %v", err)
	}

	service.mu.RLock()
	_, stillTemporary := service.mutes[stableID]
	service.mu.RUnlock()
	if stillTemporary {
		t.Fatal("a permanent mute must replace the running countdown, not race it")
	}
	if !service.alertMuteSet(service.Config())[stableID] {
		t.Fatal("the node must stay silenced after switching to a permanent mute")
	}
}

func TestTemporaryMuteSurvivesRestartAndDropsExpiredEntries(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)
	until := time.Now().Add(time.Hour).Truncate(time.Second)

	service.mu.Lock()
	service.mutes[stableID] = nodeMute{Scope: muteScopeAlerts, Until: until}
	service.mutes["expired"] = nodeMute{Scope: muteScopeAll, Until: time.Now().Add(-time.Hour)}
	service.mu.Unlock()
	if err := service.saveAlertState(); err != nil {
		t.Fatalf("failed to persist mutes: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "node_alert_state.json"))
	if err != nil {
		t.Fatalf("failed to read persisted state: %v", err)
	}
	var stateFile nodeAlertStateFile
	if err := json.Unmarshal(raw, &stateFile); err != nil {
		t.Fatalf("failed to decode persisted state: %v", err)
	}
	if len(stateFile.Mutes) != 1 || stateFile.Mutes[0].StableID != stableID {
		t.Fatalf("expected only the live mute to be persisted, got %+v", stateFile.Mutes)
	}

	restored, _ := muteTestService(t, dir)
	if err := restored.loadAlertState(); err != nil {
		t.Fatalf("failed to reload alert state: %v", err)
	}
	status := restored.nodeMuteStatusFor(stableID, restored.Config())
	if status.Scope != muteScopeAlerts || !status.Until.Equal(until) {
		t.Fatalf("timed mute did not survive restart: %+v", status)
	}
}

func TestExpiredMuteIsNotRestored(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	stateFile := nodeAlertStateFile{
		Version: 1,
		Nodes:   map[string]persistedNodeAlertState{},
		Mutes: []persistedNodeMute{
			{StableID: stableID, Scope: muteScopeAll, Until: time.Now().Add(-time.Minute)},
		},
	}
	data, err := json.Marshal(stateFile)
	if err != nil {
		t.Fatalf("failed to encode state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_alert_state.json"), data, 0600); err != nil {
		t.Fatalf("failed to write state: %v", err)
	}

	if err := service.loadAlertState(); err != nil {
		t.Fatalf("failed to load alert state: %v", err)
	}
	// Silence must never outlive its deadline, including across a restart.
	if service.nodeMuteStatusFor(stableID, service.Config()).Muted() {
		t.Fatal("an expired mute must not come back after a restart")
	}
}

func TestUnknownMuteScopeIsRejected(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, "everything", time.Hour); err == nil {
		t.Fatal("an unknown mute scope must be rejected")
	}
	if service.nodeMuteStatusFor(stableID, service.Config()).Muted() {
		t.Fatal("a rejected mute must not silence the node")
	}
}

func TestPruneInactiveMutedNodesDropsGoneNodes(t *testing.T) {
	dir := t.TempDir()
	service, stableID := muteTestService(t, dir)

	if err := service.muteNodeFor(stableID, muteScopeAll, time.Hour); err != nil {
		t.Fatalf("failed to mute node: %v", err)
	}
	service.mu.Lock()
	service.mutes["retired-node"] = nodeMute{Scope: muteScopeAll, Until: time.Now().Add(time.Hour)}
	service.mu.Unlock()

	if err := service.PruneInactiveMutedNodes(); err != nil {
		t.Fatalf("prune failed: %v", err)
	}

	service.mu.RLock()
	_, retiredKept := service.mutes["retired-node"]
	_, activeKept := service.mutes[stableID]
	service.mu.RUnlock()
	// A StableID that left the subscription must not leave silence behind for
	// whoever inherits it.
	if retiredKept {
		t.Fatal("a mute for a node that is gone must be dropped")
	}
	if !activeKept {
		t.Fatal("the mute of an active node must survive pruning")
	}
}
