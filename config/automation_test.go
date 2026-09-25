package config

import (
	"strings"
	"testing"
)

func validCLI() CLI {
	var cfg CLI
	cfg.Metrics.Username = "admin"
	cfg.Metrics.Password = "secret"
	return cfg
}

// Refused rather than ignored, like the proxy-failure flag: a trigger that is
// on without the automation behind it reads, months later, as a trigger that
// fired and found nothing.
func TestOfflineTriggerRequiresTheAutomation(t *testing.T) {
	cfg := validCLI()
	cfg.RemoteDiagnostics.AutomationOffline = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "probe-automation-offline") {
		t.Fatalf("Validate() error = %v, want the offline flag refused without automation", err)
	}
}

func TestParsePeakHours(t *testing.T) {
	for _, test := range []struct {
		value      string
		start, end int
		wantErr    bool
	}{
		{value: "18-24", start: 18, end: 24},
		{value: " 22 - 2 ", start: 22, end: 2},
		{value: "0-24", wantErr: true},
		{value: "18", wantErr: true},
		{value: "25-2", wantErr: true},
		{value: "evening", wantErr: true},
	} {
		start, end, err := ParsePeakHours(test.value)
		if test.wantErr {
			if err == nil {
				t.Errorf("ParsePeakHours(%q) accepted", test.value)
			}
			continue
		}
		if err != nil || start != test.start || end != test.end {
			t.Errorf("ParsePeakHours(%q) = %d, %d, %v; want %d, %d", test.value, start, end, err, test.start, test.end)
		}
	}
}

// Every new setting keeps its default at zero, so a configuration written before
// they existed still validates.
func TestNewSettingsAcceptTheirZeroValue(t *testing.T) {
	cfg := validCLI()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() of a configuration without the new settings = %v", err)
	}
	cfg.PathQuality.PeakTimeZone = "Mars/Olympus"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown peak time zone was accepted")
	}
}
