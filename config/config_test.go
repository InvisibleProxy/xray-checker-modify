package config

import (
	"strings"
	"testing"
)

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
