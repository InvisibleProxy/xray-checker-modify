package remnawave

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/nodearchive"
)

type fakeAPI struct {
	mu       sync.Mutex
	hosts    []Host
	internal []InternalSquad
	external []ExternalSquad
	updates  []fakeHeaderUpdate
}

type fakeHeaderUpdate struct {
	UUID    string
	Headers map[string]string
}

func (f *fakeAPI) GetHosts(context.Context) ([]Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Host(nil), f.hosts...), nil
}

func (f *fakeAPI) GetInternalSquads(context.Context) ([]InternalSquad, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InternalSquad(nil), f.internal...), nil
}

func (f *fakeAPI) GetExternalSquads(context.Context) ([]ExternalSquad, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneExternalSquads(f.external), nil
}

func (f *fakeAPI) UpdateExternalHeaders(_ context.Context, uuid string, headers map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.external {
		if f.external[index].UUID == uuid {
			f.external[index].ResponseHeadersAdd = cloneStringMap(headers)
			f.updates = append(f.updates, fakeHeaderUpdate{UUID: uuid, Headers: cloneStringMap(headers)})
			return nil
		}
	}
	return fmt.Errorf("unknown external squad %s", uuid)
}

type fakeProxySource struct {
	mu       sync.RWMutex
	proxies  []*models.ProxyConfig
	statuses map[string]checker.ProxyStatusDetails
}

func (f *fakeProxySource) GetProxies() []*models.ProxyConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]*models.ProxyConfig(nil), f.proxies...)
}

func (f *fakeProxySource) GetProxyStatusDetailsByStableID(stableID string) (checker.ProxyStatusDetails, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	details, ok := f.statuses[stableID]
	if !ok {
		return checker.ProxyStatusDetails{}, fmt.Errorf("missing status")
	}
	return details, nil
}

func (f *fakeProxySource) setOnline(stableIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stableID := range stableIDs {
		f.statuses[stableID] = checker.ProxyStatusDetails{Online: true}
	}
}

func (f *fakeProxySource) setOffline(downSince time.Time, stableIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stableID := range stableIDs {
		f.statuses[stableID] = checker.ProxyStatusDetails{Online: false, DownSince: downSince}
	}
}

type fakeIncidentSource struct {
	incidents []nodearchive.IncidentRecord
}

func (f *fakeIncidentSource) Incidents(int) []nodearchive.IncidentRecord {
	return append([]nodearchive.IncidentRecord(nil), f.incidents...)
}

func TestServiceUsesAudienceTopologyAndGroupRedundancy(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{
		hosts: []Host{
			{UUID: "host-a", Remark: "Германия A", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
			{UUID: "host-b", Remark: "Германия B", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
			{UUID: "host-service", Remark: "Service", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-service"}},
		},
		internal: []InternalSquad{
			{UUID: "internal-users", Name: "Users", Inbounds: []InternalInbound{{UUID: "inbound-users"}}},
			{UUID: "internal-service", Name: "Checker", Inbounds: []InternalInbound{{UUID: "inbound-service"}}},
		},
		external: []ExternalSquad{
			{UUID: "external-users", Name: "Plan 1", ResponseHeadersAdd: map[string]string{"x-test": "keep"}},
			{UUID: "external-service", Name: "Checker", ResponseHeadersAdd: map[string]string{}},
		},
	}
	proxies := &fakeProxySource{
		proxies: []*models.ProxyConfig{
			{StableID: "stable-a", Name: "DE A"},
			{StableID: "stable-b", Name: "DE B"},
		},
		statuses: map[string]checker.ProxyStatusDetails{},
	}
	proxies.setOffline(now.Add(-20*time.Minute), "stable-a")
	proxies.setOnline("stable-b")
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = ConfigFile{
		Version: ConfigVersion,
		Policy:  Policy{Enabled: true, OutageMinutes: 15, MinimumFailures: 3, RecoveryMinutes: 5, Messages: defaultMessageScenarios()},
		SquadPairs: []SquadPair{
			{InternalSquadUUID: "internal-users", ExternalSquadUUID: "external-users"},
			{InternalSquadUUID: "internal-service", ExternalSquadUUID: "external-service", MonitoringOnly: true},
		},
		NodeMappings: map[string]NodeMapping{
			"stable-a": {HostUUID: "host-a", GroupKey: "de", PublicLabel: "Германия"},
			"stable-b": {HostUUID: "host-b", GroupKey: "de", PublicLabel: "Германия"},
		},
	}
	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("initial SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("one healthy redundant member produced an update: %+v", api.updates)
	}

	proxies.setOffline(now.Add(-20*time.Minute), "stable-b")
	service.ObserveFullCheck()
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("single-failure ReconcileNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("one full failed check produced an update: %+v", api.updates)
	}
	for range 2 {
		service.ObserveFullCheck()
	}
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("outage ReconcileNow: %v", err)
	}
	if len(api.updates) != 1 || api.updates[0].UUID != "external-users" {
		t.Fatalf("updates = %+v", api.updates)
	}
	if api.updates[0].Headers["x-test"] != "keep" {
		t.Fatalf("unrelated header was lost: %+v", api.updates[0].Headers)
	}
	announce := api.updates[0].Headers[announceHeader]
	if !strings.HasPrefix(announce, announceValuePrefix) || strings.Contains(announce, "http") {
		t.Fatalf("unexpected announce = %q", announce)
	}
	for _, update := range api.updates {
		if update.UUID == "external-service" {
			t.Fatal("monitoring-only external squad was modified")
		}
	}

	proxies.setOnline("stable-a", "stable-b")
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("start recovery reconcile: %v", err)
	}
	if len(api.updates) != 1 {
		t.Fatalf("announce cleared before recovery grace: %+v", api.updates)
	}
	now = now.Add(6 * time.Minute)
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("finish recovery reconcile: %v", err)
	}
	if len(api.updates) != 2 {
		t.Fatalf("expected recovery clear update, got %+v", api.updates)
	}
	if _, present := api.updates[1].Headers[announceHeader]; present {
		t.Fatalf("announce was not cleared: %+v", api.updates[1].Headers)
	}
}

func TestServiceSuppressesProbableCheckEndpointMassIncident(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api, proxies := oneAudienceFixture(now, map[string]string{})
	incidents := &fakeIncidentSource{incidents: []nodearchive.IncidentRecord{{
		Kind: "mass", Status: "active", CauseCode: checker.FailureCodeCheckEndpoint, StableIDs: []string{"stable-a"},
	}}}
	service := testService(t, api, proxies, incidents, &now)
	service.config = audienceConfig("")
	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("probable check endpoint incident produced announce: %+v", api.updates)
	}
}

func TestServicePreservesUnmanagedAnnounce(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api, proxies := oneAudienceFixture(now, map[string]string{announceHeader: "manual-value", "x-test": "keep"})
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("Всё стабильно")
	for range 3 {
		service.ObserveFullCheck()
	}
	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("unmanaged announce was overwritten: %+v", api.updates)
	}
	if len(snapshot.Status.Conflicts) != 1 || !strings.Contains(snapshot.Status.Conflicts[0], "not an appendable") {
		t.Fatalf("ownership conflict was not reported: %+v", snapshot.Status)
	}
}

func TestServiceAppendsStatusToExistingBaseAndRestoresItAfterRecovery(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := "rwEncodeBase64:{{USERNAME}} | Нажми, чтобы продлить подписку →"
	api, proxies := oneAudienceFixture(now, map[string]string{announceHeader: base, "x-test": "keep"})
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("")
	for range 3 {
		service.ObserveFullCheck()
	}

	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("outage SyncNow: %v", err)
	}
	if len(api.updates) != 1 {
		t.Fatalf("outage updates = %+v", api.updates)
	}
	announce := api.updates[0].Headers[announceHeader]
	if !strings.HasPrefix(announce, base+"\n⚠️ ") {
		t.Fatalf("status was not appended on a new line: %q", announce)
	}
	if api.updates[0].Headers["x-test"] != "keep" {
		t.Fatalf("unrelated header was lost: %+v", api.updates[0].Headers)
	}
	managed := service.runtime.Managed["external-users"]
	if !managed.BasePresent || managed.BaseValue != base || managed.Value != announce {
		t.Fatalf("base ownership was not persisted exactly: %+v", managed)
	}
	if len(snapshot.Status.Announcements) != 1 || !snapshot.Status.Announcements[0].Managed || !snapshot.Status.Announcements[0].PreservesBase {
		t.Fatalf("managed suffix status = %+v", snapshot.Status.Announcements)
	}

	proxies.setOnline("stable-a")
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("start recovery ReconcileNow: %v", err)
	}
	if len(api.updates) != 1 {
		t.Fatalf("base was restored before recovery grace: %+v", api.updates)
	}
	now = now.Add(6 * time.Minute)
	snapshot, err = service.ReconcileNow(context.Background())
	if err != nil {
		t.Fatalf("finish recovery ReconcileNow: %v", err)
	}
	if len(api.updates) != 2 || api.updates[1].Headers[announceHeader] != base {
		t.Fatalf("base announce was not restored exactly: %+v", api.updates)
	}
	if _, exists := service.runtime.Managed["external-users"]; exists {
		t.Fatal("suffix ownership remained after restoring the base announce")
	}
	if len(snapshot.Status.Announcements) != 1 || snapshot.Status.Announcements[0].Managed || !snapshot.Status.Announcements[0].PreservesBase {
		t.Fatalf("restored base status = %+v", snapshot.Status.Announcements)
	}
}

func TestServiceLeavesHealthyBaseUnclaimed(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := "rwEncodeBase64:{{USERNAME}} | Нажми, чтобы продлить подписку →"
	api, proxies := oneAudienceFixture(now, map[string]string{announceHeader: base})
	proxies.setOnline("stable-a")
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("")

	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 || len(snapshot.Status.Conflicts) != 0 {
		t.Fatalf("healthy base was claimed or reported as a conflict: updates=%+v status=%+v", api.updates, snapshot.Status)
	}
	if len(snapshot.Status.Announcements) != 1 || snapshot.Status.Announcements[0].Managed || !snapshot.Status.Announcements[0].PreservesBase {
		t.Fatalf("healthy base status = %+v", snapshot.Status.Announcements)
	}
}

func TestServiceDoesNotAdoptUnmanagedMultilineAnnounce(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	value := "rwEncodeBase64:operator base\noperator-owned second line"
	api, proxies := oneAudienceFixture(now, map[string]string{announceHeader: value})
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("")
	for range 3 {
		service.ObserveFullCheck()
	}

	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("unmanaged multiline announce was overwritten: %+v", api.updates)
	}
	if len(snapshot.Status.Conflicts) != 1 || !strings.Contains(snapshot.Status.Conflicts[0], "single-line") {
		t.Fatalf("multiline ownership conflict was not reported: %+v", snapshot.Status)
	}
	if len(snapshot.Status.Announcements) != 1 || snapshot.Status.Announcements[0].PreservesBase {
		t.Fatalf("multiline announce was reported as appendable: %+v", snapshot.Status.Announcements)
	}
}

func TestServiceRelinquishesOwnershipAfterManualAnnounceChange(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api, proxies := oneAudienceFixture(now, map[string]string{announceHeader: "manual-value", "x-test": "keep"})
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("Всё стабильно")
	service.runtime.Managed["external-users"] = ManagedAnnouncement{
		Value:   announceValuePrefix + "old managed value",
		Message: "old managed value",
	}

	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("manually changed announce was overwritten: %+v", api.updates)
	}
	if _, exists := service.runtime.Managed["external-users"]; exists {
		t.Fatal("ownership was retained after the remote announce changed")
	}
	if len(snapshot.Status.Conflicts) != 1 || !strings.Contains(snapshot.Status.Conflicts[0], "changed or removed outside") {
		t.Fatalf("manual-change conflict was not reported: %+v", snapshot.Status)
	}
}

func TestServicePreservesDuplicateCaseInsensitiveAnnounceHeaders(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api, proxies := oneAudienceFixture(now, map[string]string{
		"announce": "first",
		"Announce": "second",
		"x-test":   "keep",
	})
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("Всё стабильно")

	snapshot, err := service.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("ambiguous announce headers were overwritten: %+v", api.updates)
	}
	if len(snapshot.Status.Conflicts) != 1 || !strings.Contains(snapshot.Status.Conflicts[0], "multiple case-insensitive") {
		t.Fatalf("duplicate-header conflict was not reported: %+v", snapshot.Status)
	}
}

func TestServicePublishesHealthyScenarioOnlyAfterHealthyStatus(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api, proxies := oneAudienceFixture(now, map[string]string{})
	delete(proxies.statuses, "stable-a")
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("Всё стабильно")
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("pending SyncNow: %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("unknown status produced stable message: %+v", api.updates)
	}
	proxies.setOnline("stable-a")
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("healthy ReconcileNow: %v", err)
	}
	if len(api.updates) != 1 || api.updates[0].Headers[announceHeader] != announceValuePrefix+"Всё стабильно" {
		t.Fatalf("stable message update = %+v", api.updates)
	}
}

func TestUpdateSettingsPreservesScenarioTemplatesForLegacyClient(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	service := testService(t, &fakeAPI{}, &fakeProxySource{}, &fakeIncidentSource{}, &now)
	service.config = audienceConfig("")
	service.config.Policy.Messages.SingleLocation.Template = "CUSTOM {location}"
	service.config.Policy.Messages.MultipleLocations.Enabled = false

	legacy := configSettings(service.config)
	legacy.Policy.Messages = MessageScenarios{}
	legacy.Policy.NormalMessage = "Стабильно по старому API"
	snapshot, err := service.UpdateSettings(legacy)
	if err != nil {
		t.Fatalf("legacy UpdateSettings: %v", err)
	}
	if snapshot.Settings.Policy.Messages.SingleLocation.Template != "CUSTOM {location}" || snapshot.Settings.Policy.Messages.MultipleLocations.Enabled {
		t.Fatalf("legacy update reset outage scenarios: %+v", snapshot.Settings.Policy.Messages)
	}
	if !snapshot.Settings.Policy.Messages.Healthy.Enabled || snapshot.Settings.Policy.Messages.Healthy.Template != "Стабильно по старому API" {
		t.Fatalf("legacy normalMessage was not migrated: %+v", snapshot.Settings.Policy.Messages.Healthy)
	}

	legacy = snapshot.Settings
	legacy.Policy.Messages = MessageScenarios{}
	legacy.Policy.NormalMessage = ""
	snapshot, err = service.UpdateSettings(legacy)
	if err != nil {
		t.Fatalf("legacy empty UpdateSettings: %v", err)
	}
	if snapshot.Settings.Policy.Messages.Healthy.Enabled || snapshot.Settings.Policy.Messages.SingleLocation.Template != "CUSTOM {location}" {
		t.Fatalf("legacy empty normalMessage changed the wrong scenarios: %+v", snapshot.Settings.Policy.Messages)
	}
}

func TestServiceTargetsOnlyHostAudienceInternalSquad(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{
		hosts: []Host{
			{UUID: "host-1", Remark: "DE", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-1"}},
			{UUID: "host-2", Remark: "NL", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-2"}, ExcludedInternalSquads: []string{"internal-2"}},
		},
		internal: []InternalSquad{
			{UUID: "internal-1", Name: "Users 1", Inbounds: []InternalInbound{{UUID: "inbound-1"}}},
			{UUID: "internal-2", Name: "Users 2", Inbounds: []InternalInbound{{UUID: "inbound-2"}}},
		},
		external: []ExternalSquad{
			{UUID: "external-1", Name: "Plan 1", ResponseHeadersAdd: map[string]string{}},
			{UUID: "external-2", Name: "Plan 2", ResponseHeadersAdd: map[string]string{}},
		},
	}
	proxies := &fakeProxySource{
		proxies:  []*models.ProxyConfig{{StableID: "stable-1", Name: "DE"}, {StableID: "stable-2", Name: "NL"}},
		statuses: map[string]checker.ProxyStatusDetails{},
	}
	proxies.setOffline(now.Add(-20*time.Minute), "stable-1", "stable-2")
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = ConfigFile{
		Version: ConfigVersion,
		Policy:  Policy{Enabled: true, OutageMinutes: 15, MinimumFailures: 3, RecoveryMinutes: 5, Messages: defaultMessageScenarios()},
		SquadPairs: []SquadPair{
			{InternalSquadUUID: "internal-1", ExternalSquadUUID: "external-1"},
			{InternalSquadUUID: "internal-2", ExternalSquadUUID: "external-2"},
		},
		NodeMappings: map[string]NodeMapping{
			"stable-1": {HostUUID: "host-1", PublicLabel: "Германия"},
			"stable-2": {HostUUID: "host-2", PublicLabel: "Нидерланды"},
		},
	}
	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(api.updates) != 1 || api.updates[0].UUID != "external-1" {
		t.Fatalf("audience updates = %+v", api.updates)
	}
}

func TestServiceUpdatesAnnounceAcrossConfiguredImpactScenarios(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{
		hosts: []Host{
			{UUID: "host-de", Remark: "Германия", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
			{UUID: "host-nl", Remark: "Нидерланды", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
			{UUID: "host-us", Remark: "США", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}},
		},
		internal: []InternalSquad{{UUID: "internal-users", Name: "Users", Inbounds: []InternalInbound{{UUID: "inbound-users"}}}},
		external: []ExternalSquad{{UUID: "external-users", Name: "Plan 1", ResponseHeadersAdd: map[string]string{}}},
	}
	proxies := &fakeProxySource{
		proxies: []*models.ProxyConfig{
			{StableID: "stable-de", Name: "DE"},
			{StableID: "stable-nl", Name: "NL"},
			{StableID: "stable-us", Name: "US"},
		},
		statuses: map[string]checker.ProxyStatusDetails{},
	}
	proxies.setOffline(now.Add(-20*time.Minute), "stable-de")
	proxies.setOnline("stable-nl", "stable-us")
	messages := defaultMessageScenarios()
	messages.SingleLocation.Template = "ONE {location}"
	messages.MultipleLocations.Template = "MANY {locations}"
	messages.AllLocations.Template = "ALL {unavailable}/{total}"
	service := testService(t, api, proxies, &fakeIncidentSource{}, &now)
	service.config = ConfigFile{
		Version: ConfigVersion,
		Policy: Policy{
			Enabled: true, OutageMinutes: 15, MinimumFailures: 3, RecoveryMinutes: 5, Messages: messages,
		},
		SquadPairs: []SquadPair{{InternalSquadUUID: "internal-users", ExternalSquadUUID: "external-users"}},
		NodeMappings: map[string]NodeMapping{
			"stable-de": {HostUUID: "host-de", GroupKey: "de", PublicLabel: "Германия"},
			"stable-nl": {HostUUID: "host-nl", GroupKey: "nl", PublicLabel: "Нидерланды"},
			"stable-us": {HostUUID: "host-us", GroupKey: "us", PublicLabel: "США"},
		},
	}

	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.SyncNow(context.Background()); err != nil {
		t.Fatalf("single-location SyncNow: %v", err)
	}
	if len(api.updates) != 1 || api.updates[0].Headers[announceHeader] != announceValuePrefix+"ONE Германия" {
		t.Fatalf("single-location update = %+v", api.updates)
	}
	settings := configSettings(service.config)
	settings.Policy.Messages.SingleLocation.Template = "ONE UPDATED {location}"
	if _, err := service.UpdateSettings(settings); err != nil {
		t.Fatalf("template UpdateSettings: %v", err)
	}
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("template ReconcileNow: %v", err)
	}
	if len(api.updates) != 2 || api.updates[1].Headers[announceHeader] != announceValuePrefix+"ONE UPDATED Германия" {
		t.Fatalf("template update = %+v", api.updates)
	}

	proxies.setOffline(now.Add(-20*time.Minute), "stable-nl")
	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("multiple-location ReconcileNow: %v", err)
	}
	if len(api.updates) != 3 || api.updates[2].Headers[announceHeader] != announceValuePrefix+"MANY «Германия», «Нидерланды»" {
		t.Fatalf("multiple-location update = %+v", api.updates)
	}

	proxies.setOffline(now.Add(-20*time.Minute), "stable-us")
	for range 3 {
		service.ObserveFullCheck()
	}
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("total-outage ReconcileNow: %v", err)
	}
	if len(api.updates) != 4 || api.updates[3].Headers[announceHeader] != announceValuePrefix+"ALL 3/3" {
		t.Fatalf("total-outage update = %+v", api.updates)
	}

	service.config.Policy.Messages.AllLocations.Enabled = false
	if _, err := service.ReconcileNow(context.Background()); err != nil {
		t.Fatalf("disabled total-outage ReconcileNow: %v", err)
	}
	if len(api.updates) != 5 {
		t.Fatalf("disabled scenario did not clear managed announce: %+v", api.updates)
	}
	if _, present := api.updates[4].Headers[announceHeader]; present {
		t.Fatalf("disabled scenario retained announce: %+v", api.updates[4].Headers)
	}
}

func oneAudienceFixture(now time.Time, headers map[string]string) (*fakeAPI, *fakeProxySource) {
	api := &fakeAPI{
		hosts:    []Host{{UUID: "host-a", Remark: "Германия", Inbound: HostInbound{ConfigProfileInboundUUID: "inbound-users"}}},
		internal: []InternalSquad{{UUID: "internal-users", Name: "Users", Inbounds: []InternalInbound{{UUID: "inbound-users"}}}},
		external: []ExternalSquad{{UUID: "external-users", Name: "Plan 1", ResponseHeadersAdd: cloneStringMap(headers)}},
	}
	proxies := &fakeProxySource{
		proxies:  []*models.ProxyConfig{{StableID: "stable-a", Name: "DE"}},
		statuses: map[string]checker.ProxyStatusDetails{},
	}
	proxies.setOffline(now.Add(-20*time.Minute), "stable-a")
	return api, proxies
}

func audienceConfig(normalMessage string) ConfigFile {
	messages := defaultMessageScenarios()
	if normalMessage != "" {
		messages.Healthy = MessageScenario{Enabled: true, Template: normalMessage}
	}
	return ConfigFile{
		Version:      ConfigVersion,
		Policy:       Policy{Enabled: true, OutageMinutes: 15, MinimumFailures: 3, RecoveryMinutes: 5, Messages: messages},
		SquadPairs:   []SquadPair{{InternalSquadUUID: "internal-users", ExternalSquadUUID: "external-users"}},
		NodeMappings: map[string]NodeMapping{"stable-a": {HostUUID: "host-a", GroupKey: "de", PublicLabel: "Германия"}},
	}
}

func testService(t *testing.T, api API, proxies ProxySource, incidents IncidentSource, current *time.Time) *Service {
	t.Helper()
	dir := t.TempDir()
	service := NewService(Options{
		MasterEnabled:      true,
		APIURL:             "https://remnawave.example",
		APITokenConfigured: true,
		ConfigPath:         filepath.Join(dir, "config.json"),
		RuntimePath:        filepath.Join(dir, "runtime.json"),
		API:                api,
		ProxySource:        proxies,
		IncidentSource:     incidents,
		RequestTimeout:     time.Second,
	})
	service.now = func() time.Time { return *current }
	return service
}
