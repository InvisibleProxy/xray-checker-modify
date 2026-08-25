package remnawave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeConfigMigratesUnversionedSparseState(t *testing.T) {
	config, err := DecodeConfig([]byte(`{"policy":{"enabled":false},"squadPairs":null,"nodeMappings":null}`))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if config.Version != ConfigVersion {
		t.Fatalf("version = %d", config.Version)
	}
	if config.Policy.OutageMinutes != defaultOutageMinutes || config.Policy.MinimumFailures != defaultMinFailures || config.Policy.RecoveryMinutes != defaultRecoveryMins {
		t.Fatalf("policy was not normalized: %+v", config.Policy)
	}
	if config.SquadPairs == nil || config.NodeMappings == nil {
		t.Fatalf("nil collections were not normalized: %+v", config)
	}
}

func TestDecodeConfigRejectsCaseInsensitiveStableIDCollision(t *testing.T) {
	data := []byte(`{"version":1,"policy":{"outageMinutes":15,"minimumFailures":3,"recoveryMinutes":5},"squadPairs":[],"nodeMappings":{"Stable-A":{"hostUuid":"host-a"},"stable-a":{"hostUuid":"host-b"}}}`)
	if _, err := DecodeConfig(data); err == nil || !strings.Contains(err.Error(), "case-insensitive StableID collision") {
		t.Fatalf("case-insensitive StableID collision was not rejected: %v", err)
	}
}

func TestConfigRoundTripDoesNotContainAPIToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remnawave_announce_config.json")
	config := defaultConfig()
	config.Policy.Enabled = true
	config.SquadPairs = []SquadPair{{InternalSquadUUID: "internal-1", ExternalSquadUUID: "external-1"}}
	config.NodeMappings["stable-1"] = NodeMapping{HostUUID: "host-1", GroupKey: "de", PublicLabel: "Германия"}
	if err := writeConfigFile(path, config, time.Now()); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) == "" {
		t.Fatal("empty config")
	}
	loaded, err := DecodeConfig(data)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if loaded.NodeMappings["stable-1"].HostUUID != "host-1" {
		t.Fatalf("mapping was not preserved: %+v", loaded.NodeMappings)
	}
}

func TestConfigRejectsRemnawaveTemplateInjection(t *testing.T) {
	config := defaultConfig()
	config.Policy.NormalMessage = "{{USERNAME}}"
	if err := validateConfig(config); err == nil {
		t.Fatal("template delimiters were accepted")
	}
}

func TestConfigRejectsUserFacingURL(t *testing.T) {
	config := defaultConfig()
	config.Policy.NormalMessage = "Подробности: https://status.example"
	if err := validateConfig(config); err == nil {
		t.Fatal("URL in user-facing announce was accepted")
	}
}
