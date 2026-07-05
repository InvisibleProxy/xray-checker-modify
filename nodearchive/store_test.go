package nodearchive

import (
	"path/filepath"
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
