package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"xray-checker/nodearchive"
	"xray-checker/projectmaintenance"
	"xray-checker/remnawave"
	"xray-checker/speedtest"
	"xray-checker/telegram"
)

type speedResultStateSchema struct {
	Version   int                           `json:"version"`
	UpdatedAt time.Time                     `json:"updatedAt"`
	LastRun   speedtest.RunInfo             `json:"lastRun"`
	Results   map[string]speedtest.Result   `json:"results"`
	History   map[string][]speedtest.Result `json:"history"`
}

type nodeAlertStateSchema struct {
	Version      int                                 `json:"version"`
	UpdatedAt    time.Time                           `json:"updatedAt"`
	Nodes        map[string]persistedNodeAlertSchema `json:"nodes"`
	SpeedRetries []persistedSpeedRetrySchema         `json:"speedRetries,omitempty"`
	Mutes        []persistedNodeMuteSchema           `json:"mutes,omitempty"`
}

type persistedSpeedRetrySchema struct {
	Kind      string               `json:"kind,omitempty"`
	StableIDs []string             `json:"stableIds"`
	Config    speedtest.TestConfig `json:"config"`
	DueAt     time.Time            `json:"dueAt"`
}

type persistedNodeMuteSchema struct {
	StableID string    `json:"stableId"`
	Scope    string    `json:"scope"`
	Until    time.Time `json:"until"`
}

type persistedNodeAlertSchema struct {
	FailCount       int                   `json:"failCount"`
	WasDown         bool                  `json:"wasDown"`
	DownSince       time.Time             `json:"downSince"`
	LastAlert       time.Time             `json:"lastAlert"`
	AlertCount      int                   `json:"alertCount"`
	NextAlert       time.Time             `json:"nextAlert"`
	LastDiagnostics time.Time             `json:"lastDiagnostics"`
	HostCheck       *persistedCheckSchema `json:"hostCheck,omitempty"`
	PingCheck       *persistedCheckSchema `json:"pingCheck,omitempty"`
	FailureCode     string                `json:"failureCode,omitempty"`
	FailureSummary  string                `json:"failureSummary,omitempty"`
	FailureDetail   string                `json:"failureDetail,omitempty"`
	RecoveryPending bool                  `json:"recoveryPending,omitempty"`
	RecoveredAt     time.Time             `json:"recoveredAt,omitempty"`
	RecoveryLatency int64                 `json:"recoveryLatencyMs,omitempty"`
}

type persistedCheckSchema struct {
	Checked   bool      `json:"checked"`
	Online    bool      `json:"online"`
	LatencyMs int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
	Target    string    `json:"target,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func validateDataFile(name string, data []byte) error {
	object, err := decodeJSONObjectUnique(data)
	if err != nil {
		return fmt.Errorf("backup file %s: %w", name, err)
	}

	switch name {
	case "node_alert_state.json":
		var state nodeAlertStateSchema
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("backup file %s has invalid alert state: %w", name, err)
		}
		if state.Version != 1 || state.Nodes == nil {
			return fmt.Errorf("backup file %s has an unsupported schema", name)
		}
		for _, retry := range state.SpeedRetries {
			if (retry.Kind != "" && retry.Kind != "speed-confirmation" && retry.Kind != "low-speed" && retry.Kind != "deadline") || len(retry.StableIDs) == 0 || retry.DueAt.IsZero() {
				return fmt.Errorf("backup file %s has an invalid pending speed retry", name)
			}
		}
		for _, mute := range state.Mutes {
			if mute.StableID == "" || mute.Until.IsZero() ||
				(mute.Scope != "all" && mute.Scope != "alerts" && mute.Scope != "speed") {
				return fmt.Errorf("backup file %s has an invalid node mute", name)
			}
		}
	case "node_registry.json":
		var state nodearchive.StateFile
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("backup file %s has invalid node registry: %w", name, err)
		}
		if state.Version != 1 || state.Nodes == nil {
			return fmt.Errorf("backup file %s has an unsupported schema", name)
		}
	case "project_state.json":
		var state projectmaintenance.StateFile
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("backup file %s has invalid project state: %w", name, err)
		}
		if state.Version != projectmaintenance.StateVersion || (state.Enabled && state.Since.IsZero()) || (!state.Enabled && !state.Since.IsZero()) {
			return fmt.Errorf("backup file %s has an unsupported schema", name)
		}
	case "remnawave_announce_config.json":
		if err := remnawave.ValidateConfigData(data); err != nil {
			return fmt.Errorf("backup file %s has invalid Remnawave announce settings: %w", name, err)
		}
	case "speedtest_results.json":
		var state speedResultStateSchema
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("backup file %s has invalid speed-test results: %w", name, err)
		}
		if state.Version != 1 || state.Results == nil || state.History == nil {
			return fmt.Errorf("backup file %s has an unsupported schema", name)
		}
	case "speedtest_schedule.json":
		var intervalSec int
		if err := decodeRequiredField(object, "intervalSec", &intervalSec); err != nil {
			return fmt.Errorf("backup file %s: %w", name, err)
		}
		var testConfig map[string]json.RawMessage
		if err := decodeRequiredField(object, "config", &testConfig); err != nil || testConfig == nil {
			if err == nil {
				err = fmt.Errorf("config must be a JSON object")
			}
			return fmt.Errorf("backup file %s: %w", name, err)
		}
		var schedule speedtest.ScheduleConfig
		if err := json.Unmarshal(data, &schedule); err != nil {
			return fmt.Errorf("backup file %s has invalid schedule: %w", name, err)
		}
		if rawNextRun, ok := object["nextRunAt"]; ok {
			var nextRun time.Time
			if err := json.Unmarshal(rawNextRun, &nextRun); err != nil || nextRun.IsZero() {
				if err == nil {
					err = fmt.Errorf("nextRunAt must be a non-zero RFC3339 timestamp")
				}
				return fmt.Errorf("backup file %s has invalid next scheduled run: %w", name, err)
			}
		}
	case "telegram_config.json":
		var enabled bool
		if err := decodeRequiredField(object, "enabled", &enabled); err != nil {
			return fmt.Errorf("backup file %s: %w", name, err)
		}
		var config telegram.Config
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("backup file %s has invalid Telegram settings: %w", name, err)
		}
	default:
		return fmt.Errorf("unsupported backup data file %s", name)
	}

	return nil
}

// ValidateDataFile validates one persisted state file with the same duplicate
// key and typed-schema checks used by backup restore.
func ValidateDataFile(name string, data []byte) error {
	return validateDataFile(name, data)
}

func decodeRequiredField(object map[string]json.RawMessage, name string, destination any) error {
	raw, ok := object[name]
	if !ok {
		return fmt.Errorf("is missing %s", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("field %s must not be null", name)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("field %s has an invalid type: %w", name, err)
	}
	return nil
}

func decodeJSONObjectUnique(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("contains invalid JSON: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, fmt.Errorf("must contain a JSON object")
	}

	if err := validateJSONObjectContents(decoder); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("contains trailing JSON data")
	} else if err != io.EOF {
		return nil, fmt.Errorf("contains invalid trailing JSON data: %w", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("contains invalid JSON object: %w", err)
	}
	return result, nil
}

func validateJSONObjectContents(decoder *json.Decoder) error {
	seen := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("contains invalid JSON object key: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("contains a non-string object key")
		}
		folded := strings.ToLower(key)
		if previous, exists := seen[folded]; exists {
			return fmt.Errorf("contains duplicate keys %q and %q", previous, key)
		}
		seen[folded] = key
		if err := validateJSONValue(decoder); err != nil {
			return fmt.Errorf("contains invalid value for %q: %w", key, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("contains invalid JSON object: %w", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("contains invalid JSON object closing token")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return validateJSONObjectContents(decoder)
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closingDelimiter, ok := closing.(json.Delim); !ok || closingDelimiter != ']' {
			return fmt.Errorf("contains invalid JSON array closing token")
		}
		return nil
	default:
		return fmt.Errorf("contains unexpected JSON delimiter %q", delimiter)
	}
}
