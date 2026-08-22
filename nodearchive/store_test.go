package nodearchive

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

func TestSyncProxiesMarksMissingNodeRetiredAndClosesDowntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node_registry.json")
	store := NewStore(path, nil)
	now := time.Now().Add(-time.Hour)
	store.nodes["old"] = NodeRecord{
		StableID:         "old",
		Name:             "Old",
		Active:           true,
		FirstSeenAt:      now,
		LastSeenAt:       now,
		CurrentDownSince: now,
		IncidentCount:    1,
	}

	if err := store.SyncProxies(nil); err != nil {
		t.Fatal(err)
	}

	store.mu.RLock()
	record := store.nodes["old"]
	store.mu.RUnlock()
	if record.Active {
		t.Fatal("record is still active")
	}
	if record.RetiredAt.IsZero() {
		t.Fatal("retiredAt was not set")
	}
	if !record.CurrentDownSince.IsZero() {
		t.Fatal("current downtime was not closed")
	}
	if record.TotalDowntimeSec <= 0 {
		t.Fatalf("total downtime = %d, want positive value", record.TotalDowntimeSec)
	}
}

func TestSyncSpeedHistoryCreatesArchivedRecordAndStats(t *testing.T) {
	store := NewStore("", nil)
	older := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	history := map[string][]speedtest.Result{
		"nl": {
			{StableID: "nl", Name: "NL-test", SubName: "InvisibleProxy", Mbps: 20, CheckedAt: older},
			{StableID: "nl", Name: "NL-test", SubName: "InvisibleProxy", Mbps: 40, CheckedAt: newer},
			{StableID: "nl", Name: "NL-test", SubName: "InvisibleProxy", Error: "timeout", CheckedAt: newer.Add(time.Minute)},
		},
	}

	if err := store.SyncSpeedHistory(history); err != nil {
		t.Fatal(err)
	}
	summaries := store.Summaries(history)
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.Active {
		t.Fatal("history-only record should be archived")
	}
	if summary.ClaimedCountryCode != "NL" {
		t.Fatalf("claimed country = %q, want NL", summary.ClaimedCountryCode)
	}
	if summary.SuccessfulResults != 2 || summary.FailedResults != 1 {
		t.Fatalf("stats successful=%d failed=%d, want 2/1", summary.SuccessfulResults, summary.FailedResults)
	}
	if summary.AvgMbps != 30 || summary.MinMbps != 20 || summary.MaxMbps != 40 {
		t.Fatalf("speed stats avg/min/max = %.2f/%.2f/%.2f, want 30/20/40", summary.AvgMbps, summary.MinMbps, summary.MaxMbps)
	}
}

func TestApplyAvailabilityUsesExistingDownSince(t *testing.T) {
	downSince := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	record := applyAvailability(NodeRecord{StableID: "de"}, checker.ProxyStatusDetails{
		Online:    false,
		DownSince: downSince,
	}, time.Now())

	if !record.CurrentDownSince.Equal(downSince) {
		t.Fatalf("down since = %s, want %s", record.CurrentDownSince, downSince)
	}
	if record.IncidentCount != 1 {
		t.Fatalf("incident count = %d, want 1", record.IncidentCount)
	}

	record = applyAvailability(record, checker.ProxyStatusDetails{Online: true}, time.Now())
	if !record.CurrentDownSince.IsZero() {
		t.Fatal("current downtime was not closed")
	}
	if record.TotalDowntimeSec <= 0 {
		t.Fatalf("total downtime = %d, want positive value", record.TotalDowntimeSec)
	}
}

func TestNodeIncidentJournalOpensAndResolves(t *testing.T) {
	store := NewStore("", nil)
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node 1", SubName: "Primary"}
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	details := checker.ProxyStatusDetails{
		Online:    false,
		DownSince: startedAt,
		Failure: checker.FailureDetails{
			Code:    checker.FailureCodeTCPRefused,
			Summary: checker.FailureSummary(checker.FailureCodeTCPRefused),
		},
	}
	if !store.updateNodeIncidentLocked(proxy, details, time.Now()) {
		t.Fatal("offline transition did not open an incident")
	}
	if len(store.incidents) != 1 || store.incidents[0].Status != incidentStatusActive {
		t.Fatalf("incidents = %+v", store.incidents)
	}
	if !store.updateNodeIncidentLocked(proxy, checker.ProxyStatusDetails{Online: true}, time.Now()) {
		t.Fatal("recovery did not resolve the incident")
	}
	if store.incidents[0].Status != incidentStatusResolved || store.incidents[0].ResolvedAt.IsZero() {
		t.Fatalf("resolved incident = %+v", store.incidents[0])
	}
}

func TestCorrelateMassIncidentsRequiresSameCauseAndMajority(t *testing.T) {
	proxies := []*models.ProxyConfig{
		{StableID: "one", Name: "One", SubName: "Primary", Server: "one.example"},
		{StableID: "two", Name: "Two", SubName: "Primary", Server: "two.example"},
		{StableID: "three", Name: "Three", SubName: "Primary", Server: "three.example"},
		{StableID: "four", Name: "Four", SubName: "Primary", Server: "four.example"},
	}
	details := make(map[string]checker.ProxyStatusDetails)
	for _, proxy := range proxies[:3] {
		details[proxy.StableID] = checker.ProxyStatusDetails{
			Online:  false,
			Failure: checker.FailureDetails{Code: checker.FailureCodeTCPRefused, Summary: checker.FailureSummary(checker.FailureCodeTCPRefused)},
		}
	}
	details["four"] = checker.ProxyStatusDetails{Online: true}
	groups := correlateMassIncidents(proxies, details)
	if len(groups) != 1 || groups[0].Scope != "global" || len(groups[0].StableIDs) != 3 {
		t.Fatalf("mass groups = %+v", groups)
	}

	details["three"] = checker.ProxyStatusDetails{Online: false, Failure: checker.FailureDetails{Code: checker.FailureCodeTLS}}
	if groups := correlateMassIncidents(proxies, details); len(groups) != 0 {
		t.Fatalf("mixed causes were grouped: %+v", groups)
	}
}

func TestLoadOldNodeRegistryWithoutIncidentJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node_registry.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"nodes":{"one":{"stableId":"one","name":"One"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if len(store.Incidents(10)) != 0 || store.nodes["one"].Name != "One" {
		t.Fatalf("old state was not normalized: nodes=%+v incidents=%+v", store.nodes, store.incidents)
	}
}

func TestRecordAvailabilityPersistsIncidentJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node_registry.json")
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node 1", Protocol: "vless", Server: "node.example", Port: 443}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "https://check.example/status", "", 1, 0, "status")
	store := NewStore(path, proxyChecker)
	if err := store.SyncProxies([]*models.ProxyConfig{proxy}); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	failure := checker.FailureDetails{Code: checker.FailureCodeTCPTimeout, Summary: checker.FailureSummary(checker.FailureCodeTCPTimeout)}
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, startedAt, checker.HostCheckDetails{}, checker.PingCheckDetails{}, failure) {
		t.Fatal("failed to restore offline checker state")
	}
	if err := store.RecordAvailability(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewStore(path, proxyChecker)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	incidents := reloaded.Incidents(10)
	if len(incidents) != 1 || incidents[0].CauseCode != checker.FailureCodeTCPTimeout || incidents[0].Status != incidentStatusActive {
		t.Fatalf("persisted incidents = %+v", incidents)
	}
}

func TestDetectClaimedCountryAvoidsSubstringFalsePositive(t *testing.T) {
	code, _ := detectClaimedCountry("node backup")
	if code != "" {
		t.Fatalf("country code = %q, want empty", code)
	}
	code, _ = detectClaimedCountry("DE Frankfurt")
	if code != "DE" {
		t.Fatalf("country code = %q, want DE", code)
	}
	code, _ = detectClaimedCountry("🇪🇪 Эстония")
	if code != "EE" {
		t.Fatalf("country code = %q, want EE", code)
	}
}

func TestApplyProxyStoresClaimedCountry(t *testing.T) {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Server:   "203.0.113.10",
		Port:     443,
		Name:     "US Los Angeles",
		UUID:     "uuid",
	}
	proxy.StableID = proxy.GenerateStableID()

	record := applyProxy(NodeRecord{}, proxy, time.Now())
	if record.ClaimedCountryCode != "US" {
		t.Fatalf("claimed country = %q, want US", record.ClaimedCountryCode)
	}
}

func TestDeleteArchivedRejectsActiveAndDeletesRetired(t *testing.T) {
	store := NewStore("", nil)
	store.nodes["active"] = NodeRecord{StableID: "active", Active: true}
	store.nodes["retired"] = NodeRecord{StableID: "retired", Active: false}

	if err := store.DeleteArchived("active"); err == nil {
		t.Fatal("expected active node delete to fail")
	}
	if _, ok := store.nodes["active"]; !ok {
		t.Fatal("active node was deleted")
	}

	if err := store.DeleteArchived("retired"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.nodes["retired"]; ok {
		t.Fatal("retired node was not deleted")
	}
}

func TestDeleteArchivedRollsBackMemoryWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(blocker, "node_registry.json"), nil)
	want := NodeRecord{StableID: "retired", Name: "Retired", Active: false}
	store.nodes[want.StableID] = want

	if err := store.DeleteArchived(want.StableID); err == nil {
		t.Fatal("DeleteArchived() succeeded despite persistence failure")
	}
	got, ok := store.nodes[want.StableID]
	if !ok || got != want {
		t.Fatalf("retired record was not restored after persistence failure: %+v, exists=%v", got, ok)
	}
}

func TestArchivedRecordAndRestoreArchivedKeepRetiredInvariant(t *testing.T) {
	store := NewStore("", nil)
	want := NodeRecord{StableID: "retired", Name: "Retired", Active: false}
	store.nodes[want.StableID] = want
	store.mergedNodes[want.StableID] = []string{"old-key"}
	record, err := store.ArchivedRecord(want.StableID)
	if err != nil || record != want {
		t.Fatalf("ArchivedRecord() = %+v, %v; want %+v", record, err, want)
	}
	if err := store.DeleteArchived(want.StableID); err != nil {
		t.Fatalf("DeleteArchived() error = %v", err)
	}
	if err := store.RestoreArchived(record, "old-key"); err != nil {
		t.Fatalf("RestoreArchived() error = %v", err)
	}
	if got := store.nodes[want.StableID]; got != want {
		t.Fatalf("restored record = %+v, want %+v", got, want)
	}
	if got := store.MergedFromStableIDs(want.StableID); !reflect.DeepEqual(got, []string{"old-key"}) {
		t.Fatalf("restored lineage = %#v", got)
	}
	if err := store.RestoreArchived(NodeRecord{StableID: "active", Active: true}); err == nil {
		t.Fatal("RestoreArchived() accepted an active record")
	}
}

func TestCountryMatchUsesMultipleGeoSources(t *testing.T) {
	tests := []struct {
		name   string
		record NodeRecord
		want   string
	}{
		{
			name: "both sources match claimed country",
			record: NodeRecord{
				ClaimedCountryCode:  "NL",
				GeoCountryCode:      "NL",
				IfconfigCountryCode: "NL",
			},
			want: "match",
		},
		{
			name: "single matching source is partial",
			record: NodeRecord{
				ClaimedCountryCode: "NL",
				GeoCountryCode:     "NL",
			},
			want: "partial",
		},
		{
			name: "failed source does not make a full match",
			record: NodeRecord{
				ClaimedCountryCode:  "NL",
				GeoCountryCode:      "NL",
				IfconfigCountryCode: "NL",
				IfconfigError:       "timeout",
			},
			want: "partial",
		},
		{
			name: "sources disagree",
			record: NodeRecord{
				ClaimedCountryCode:  "NL",
				GeoCountryCode:      "NL",
				IfconfigCountryCode: "DE",
			},
			want: "conflict",
		},
		{
			name: "sources agree but differ from claimed country",
			record: NodeRecord{
				ClaimedCountryCode:  "NL",
				GeoCountryCode:      "DE",
				IfconfigCountryCode: "DE",
			},
			want: "mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countryMatch(tt.record); got != tt.want {
				t.Fatalf("countryMatch() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGeoBlacklistHitsUsesSuccessfulSourcesOnly(t *testing.T) {
	hits := geoBlacklistHits([]GeoSource{
		{Source: "ipinfo.io", Country: "Iran", CountryCode: "IR"},
		{Source: "ifconfig.net", Country: "Germany", CountryCode: "DE"},
		{Source: "cached", Country: "Russia", CountryCode: "RU", Error: "timeout"},
	})

	if len(hits) != 1 {
		t.Fatalf("hits length = %d, want 1", len(hits))
	}
	if hits[0].CountryCode != "IR" || hits[0].Source != "ipinfo.io" {
		t.Fatalf("hit = %+v, want ipinfo.io IR", hits[0])
	}
}

func TestMergeGeoFieldsPreservesConcurrentAvailabilityChanges(t *testing.T) {
	downSince := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	current := NodeRecord{
		StableID:         "node-1",
		Server:           "node.example.com",
		Active:           false,
		RetiredAt:        time.Now().Truncate(time.Second),
		CurrentDownSince: downSince,
		IncidentCount:    3,
		TotalDowntimeSec: 120,
	}
	staleGeoResult := NodeRecord{
		StableID:            "node-1",
		Server:              "node.example.com",
		Active:              true,
		IncidentCount:       1,
		GeoIP:               "203.0.113.10",
		GeoCountry:          "Germany",
		GeoCountryCode:      "DE",
		IfconfigCountryCode: "DE",
	}

	merged := mergeGeoFields(current, staleGeoResult)
	if merged.Active || merged.RetiredAt.IsZero() {
		t.Fatalf("availability fields were overwritten: %+v", merged)
	}
	if !merged.CurrentDownSince.Equal(downSince) || merged.IncidentCount != 3 || merged.TotalDowntimeSec != 120 {
		t.Fatalf("downtime fields were overwritten: %+v", merged)
	}
	if merged.GeoCountryCode != "DE" || merged.IfconfigCountryCode != "DE" {
		t.Fatalf("geo fields were not applied: %+v", merged)
	}
}
