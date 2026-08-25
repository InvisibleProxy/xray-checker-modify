package remnawave

import (
	"fmt"
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
	if !config.Policy.Messages.SingleLocation.Enabled || config.Policy.Messages.SingleLocation.Template != defaultSingleLocationTemplate || config.Policy.Messages.Healthy.Enabled {
		t.Fatalf("message scenarios were not normalized: %+v", config.Policy.Messages)
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
	config.Policy.Messages.SingleLocation.Template = "Недоступна: {location}"
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
	if loaded.Policy.Messages.SingleLocation.Template != "Недоступна: {location}" {
		t.Fatalf("message template was not preserved: %+v", loaded.Policy.Messages)
	}
}

func TestDecodeConfigMigratesV1NormalMessageToHealthyScenario(t *testing.T) {
	config, err := DecodeConfig([]byte(`{
  "version": 1,
  "policy": {
    "enabled": true,
    "outageMinutes": 15,
    "minimumFailures": 3,
    "recoveryMinutes": 5,
    "normalMessage": "Работа восстановлена"
  },
  "squadPairs": [],
  "nodeMappings": {}
}`))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if config.Version != ConfigVersion || !config.Policy.Messages.Healthy.Enabled || config.Policy.Messages.Healthy.Template != "Работа восстановлена" {
		t.Fatalf("v1 normalMessage migration = %+v", config.Policy)
	}
	if config.Policy.NormalMessage != "" || config.Policy.Messages.MultipleLocations.Template != defaultMultipleLocationsTemplate {
		t.Fatalf("legacy/default message fields = %+v", config.Policy)
	}
}

func TestConfigRejectsRemnawaveTemplateInjection(t *testing.T) {
	config := defaultConfig()
	config.Policy.Messages.Healthy.Template = "{{USERNAME}}"
	if err := validateConfig(config); err == nil {
		t.Fatal("template delimiters were accepted")
	}
}

func TestConfigRejectsUserFacingURL(t *testing.T) {
	config := defaultConfig()
	config.Policy.Messages.SingleLocation.Template = "Подробности: https://status.example"
	if err := validateConfig(config); err == nil {
		t.Fatal("URL in user-facing announce was accepted")
	}
}

func TestConfigRejectsUnknownMessagePlaceholder(t *testing.T) {
	config := defaultConfig()
	config.Policy.Messages.SingleLocation.Template = "Недоступна: {host}"
	if err := validateConfig(config); err == nil || !strings.Contains(err.Error(), "unknown or unsupported placeholder") {
		t.Fatalf("unknown placeholder was not rejected: %v", err)
	}
}

func TestDecodeRuntimeMigratesV1WholeValueOwnership(t *testing.T) {
	runtime, err := DecodeRuntime([]byte(`{
  "version": 1,
  "managed": {
    "external-users": {
      "value": "rwEncodeBase64:old managed value",
      "message": "old managed value",
      "groups": null,
      "updatedAt": "2026-08-25T12:00:00Z"
    }
  }
}`))
	if err != nil {
		t.Fatalf("DecodeRuntime: %v", err)
	}
	if runtime.Version != RuntimeVersion {
		t.Fatalf("version = %d", runtime.Version)
	}
	managed := runtime.Managed["external-users"]
	if managed.BasePresent || managed.BaseValue != "" {
		t.Fatalf("v1 ownership unexpectedly gained a base announce: %+v", managed)
	}
	if managed.Value != composeManagedAnnounce(false, "", managed.Message) {
		t.Fatalf("v1 whole-value ownership changed during migration: %+v", managed)
	}
	if managed.Groups == nil {
		t.Fatal("v1 nil groups were not normalized")
	}
}

func TestDecodeRuntimeAcceptsV2BaseAndManagedSuffix(t *testing.T) {
	base := "rwEncodeBase64:{{USERNAME}} | Нажми, чтобы продлить подписку →"
	message := "⚠️ Временно недоступна локация «Германия»."
	data := []byte(fmt.Sprintf(`{
  "version": 2,
  "managed": {
    "external-users": {
      "value": %q,
      "message": %q,
      "basePresent": true,
      "baseValue": %q,
      "updatedAt": "2026-08-25T12:00:00Z"
    }
  }
}`, base+"\n"+message, message, base))
	runtime, err := DecodeRuntime(data)
	if err != nil {
		t.Fatalf("DecodeRuntime: %v", err)
	}
	managed := runtime.Managed["external-users"]
	if !managed.BasePresent || managed.BaseValue != base || managed.Value != base+"\n"+message {
		t.Fatalf("v2 base ownership = %+v", managed)
	}
}
