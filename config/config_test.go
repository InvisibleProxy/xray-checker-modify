package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestSubscriptionURLsFromCommaSeparatedEnvironment(t *testing.T) {
	t.Setenv("SUBSCRIPTION_URL", "https://one.example/sub,https://two.example/sub")
	t.Setenv("RUN_ONCE", "true")

	var cfg CLI
	parser, err := kong.New(&cfg, kong.Vars{"version": "test"})
	if err != nil {
		t.Fatalf("kong.New() error = %v", err)
	}
	if _, err := parser.Parse(nil); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []string{"https://one.example/sub", "https://two.example/sub"}
	if !reflect.DeepEqual(cfg.Subscription.URLs, want) {
		t.Fatalf("subscription URLs = %#v, want %#v", cfg.Subscription.URLs, want)
	}
}

func TestValidateRequiresExplicitAdminCredentials(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{name: "both missing", wantErr: true},
		{name: "password missing", username: "admin", wantErr: true},
		{name: "username missing", password: "secret", wantErr: true},
		{name: "whitespace is missing", username: "  ", password: "secret", wantErr: true},
		{name: "both configured", username: "admin", password: "secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg CLI
			cfg.Metrics.Username = tt.username
			cfg.Metrics.Password = tt.password
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "/api/v1/admin/") {
				t.Fatalf("credential error does not explain protected endpoints: %v", err)
			}
		})
	}
}

func TestValidateAllowsRunOnceWithoutHTTPAdminCredentials(t *testing.T) {
	var cfg CLI
	cfg.RunOnce = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v for run-once mode", err)
	}
}

func TestValidateStillRequiresProtectedMetricsForPublicDashboard(t *testing.T) {
	var cfg CLI
	cfg.RunOnce = true
	cfg.Web.Public = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("public dashboard without protected metrics was accepted")
	}
}

func TestValidateRemnawaveAnnounceConfiguration(t *testing.T) {
	base := func() CLI {
		var cfg CLI
		cfg.RunOnce = true
		cfg.Remnawave.Enabled = true
		cfg.Remnawave.APIURL = "https://panel.example"
		cfg.Remnawave.APIToken = "token"
		cfg.Remnawave.TimeoutSeconds = 10
		cfg.Remnawave.ReconcileIntervalSeconds = 60
		cfg.Remnawave.TopologyIntervalSeconds = 300
		return cfg
	}

	valid := base()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid Remnawave config rejected: %v", err)
	}
	missingToken := base()
	missingToken.Remnawave.APIToken = " "
	if err := missingToken.Validate(); err == nil {
		t.Fatal("missing Remnawave API token was accepted")
	}
	credentialedURL := base()
	credentialedURL.Remnawave.APIURL = "https://user:password@panel.example"
	if err := credentialedURL.Validate(); err == nil {
		t.Fatal("credentialed Remnawave API URL was accepted")
	}
}

func TestValidateRemoteDiagnosticsConfiguration(t *testing.T) {
	var cfg CLI
	cfg.RunOnce = true
	cfg.RemoteDiagnostics.Enabled = true
	cfg.RemoteDiagnostics.RegistryPath = "data/diagnostic_agents.json"
	cfg.RemoteDiagnostics.AgentImage = "xray-checker-probe-agent:local"
	cfg.RemoteDiagnostics.EnrollmentTTLMinutes = 15
	cfg.RemoteDiagnostics.HeartbeatIntervalSeconds = 30
	cfg.RemoteDiagnostics.HeartbeatMaxSkewSeconds = 120
	cfg.RemoteDiagnostics.TrustedProxySecret = "0123456789abcdef0123456789abcdef"
	cfg.RemoteDiagnostics.AutomationCooldownMinutes = 30
	cfg.RemoteDiagnostics.AutomationAlertWaitSeconds = 90
	cfg.RemoteDiagnostics.AutomationMaxConcurrent = 2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid remote diagnostics config rejected: %v", err)
	}

	invalidInterval := cfg
	invalidInterval.RemoteDiagnostics.HeartbeatIntervalSeconds = 4
	if err := invalidInterval.Validate(); err == nil {
		t.Fatal("too-short probe heartbeat interval was accepted")
	}

	weakSecret := cfg
	weakSecret.RemoteDiagnostics.TrustedProxySecret = "too-short"
	if err := weakSecret.Validate(); err == nil {
		t.Fatal("weak trusted-proxy secret was accepted")
	}

	automationWithoutDiagnostics := cfg
	automationWithoutDiagnostics.RemoteDiagnostics.Enabled = false
	automationWithoutDiagnostics.RemoteDiagnostics.AutomationEnabled = true
	if err := automationWithoutDiagnostics.Validate(); err == nil {
		t.Fatal("agent automation without Remote Diagnostics was accepted")
	}

	invalidAutomation := cfg
	invalidAutomation.RemoteDiagnostics.AutomationMaxConcurrent = 0
	if err := invalidAutomation.Validate(); err == nil {
		t.Fatal("zero automatic diagnostic concurrency was accepted")
	}
}
