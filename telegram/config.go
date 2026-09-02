package telegram

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"xray-checker/logger"
	"xray-checker/speedtest"
)

const (
	defaultTimeoutSec                  = 20
	defaultSpeedReportLimit            = 10
	defaultAlertCheckMinutes           = 5
	minAlertAfterFailures              = 2
	defaultAlertAfterFailures          = minAlertAfterFailures
	defaultAlertDiagnosticsMinutes     = 60
	defaultAlertMaxReminderMinutes     = 1440
	defaultAlertReminderScheduleString = "15,60,180,360,720"
	maxSpeedReportLimit                = 50
	menuSpeedButtonLimit               = 8
	maxDiagnosticsRefreshConcurrency   = 4
	maxRichMessageRunes                = 32768
	speedConfirmationRetryDelay        = 30 * time.Minute
	speedConfirmationRetryBusyDelay    = time.Minute
	// Shared with the manager, which branches on this source to keep the
	// confirmation run's primary and fallback phases from overlapping.
	speedConfirmationRetrySource = speedtest.ConfirmationRetrySource
	speedRetryKindConfirmation   = "speed-confirmation"
	legacySpeedRetryKindLowSpeed = "low-speed"
	legacySpeedRetryKindDeadline = "deadline"
	legacyDeadlineRetryDelay     = 5 * time.Minute
)

var defaultAlertReminderScheduleMinutes = parseMinuteSchedule(defaultAlertReminderScheduleString)

type Config struct {
	Enabled                      bool     `json:"enabled"`
	BotToken                     string   `json:"botToken"`
	ChatID                       string   `json:"chatId"`
	MessageThreadID              int      `json:"messageThreadId"`
	AdminUserIDs                 []int64  `json:"adminUserIds"`
	CommandPollingEnabled        bool     `json:"commandPollingEnabled"`
	SpeedReportsEnabled          bool     `json:"speedReportsEnabled"`
	SpeedReportMode              string   `json:"speedReportMode"`
	LowSpeedThresholdMbps        float64  `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit             int      `json:"speedReportLimit"`
	NodeAlertsEnabled            bool     `json:"nodeAlertsEnabled"`
	AlertCheckMinutes            int      `json:"alertCheckMinutes"`
	AlertAfterFailures           int      `json:"alertAfterFailures"`
	AlertRepeatMinutes           int      `json:"alertRepeatMinutes,omitempty"`
	AlertDiagnosticsMinutes      int      `json:"alertDiagnosticsMinutes"`
	AlertReminderScheduleMinutes []int    `json:"alertReminderScheduleMinutes"`
	AlertMaxReminderMinutes      int      `json:"alertMaxReminderMinutes"`
	GroupOfflineReminders        bool     `json:"groupOfflineReminders"`
	NotifyRecovery               bool     `json:"notifyRecovery"`
	MutedNodeIDs                 []string `json:"mutedNodeIds,omitempty"`
	MutedSpeedNodeIDs            []string `json:"mutedSpeedNodeIds,omitempty"`
	MutedAlertNodeIDs            []string `json:"mutedAlertNodeIds,omitempty"`
	TimeoutSec                   int      `json:"timeoutSec"`
}

type AdminConfig struct {
	Enabled                      bool     `json:"enabled"`
	CommandPollingEnabled        bool     `json:"commandPollingEnabled"`
	SpeedReportsEnabled          bool     `json:"speedReportsEnabled"`
	SpeedReportMode              string   `json:"speedReportMode"`
	LowSpeedThresholdMbps        float64  `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit             int      `json:"speedReportLimit"`
	NodeAlertsEnabled            bool     `json:"nodeAlertsEnabled"`
	AlertCheckMinutes            int      `json:"alertCheckMinutes"`
	AlertAfterFailures           int      `json:"alertAfterFailures"`
	AlertRepeatMinutes           int      `json:"alertRepeatMinutes,omitempty"`
	AlertDiagnosticsMinutes      int      `json:"alertDiagnosticsMinutes"`
	AlertReminderScheduleMinutes []int    `json:"alertReminderScheduleMinutes"`
	AlertMaxReminderMinutes      int      `json:"alertMaxReminderMinutes"`
	GroupOfflineReminders        bool     `json:"groupOfflineReminders"`
	NotifyRecovery               bool     `json:"notifyRecovery"`
	MutedNodeIDs                 []string `json:"mutedNodeIds,omitempty"`
	MutedSpeedNodeIDs            []string `json:"mutedSpeedNodeIds,omitempty"`
	MutedAlertNodeIDs            []string `json:"mutedAlertNodeIds,omitempty"`
	BotTokenConfigured           bool     `json:"botTokenConfigured"`
	ChatConfigured               bool     `json:"chatConfigured"`
	MessageThreadConfigured      bool     `json:"messageThreadConfigured"`
	AdminUserCount               int      `json:"adminUserCount"`
}

func DefaultConfig() Config {
	return Config{
		CommandPollingEnabled:        true,
		SpeedReportsEnabled:          true,
		SpeedReportMode:              "always",
		SpeedReportLimit:             defaultSpeedReportLimit,
		NodeAlertsEnabled:            true,
		AlertCheckMinutes:            defaultAlertCheckMinutes,
		AlertAfterFailures:           defaultAlertAfterFailures,
		AlertDiagnosticsMinutes:      defaultAlertDiagnosticsMinutes,
		AlertReminderScheduleMinutes: append([]int(nil), defaultAlertReminderScheduleMinutes...),
		AlertMaxReminderMinutes:      defaultAlertMaxReminderMinutes,
		GroupOfflineReminders:        true,
		NotifyRecovery:               true,
		TimeoutSec:                   defaultTimeoutSec,
	}
}

func (c *Config) Normalize() {
	c.BotToken = strings.TrimSpace(c.BotToken)
	c.ChatID = strings.TrimSpace(c.ChatID)

	if c.SpeedReportMode == "" {
		c.SpeedReportMode = "always"
	}
	if c.SpeedReportMode != "always" && c.SpeedReportMode != "issues" && c.SpeedReportMode != "disabled" {
		c.SpeedReportMode = "always"
	}
	if c.SpeedReportLimit <= 0 {
		c.SpeedReportLimit = defaultSpeedReportLimit
	}
	if c.SpeedReportLimit > maxSpeedReportLimit {
		c.SpeedReportLimit = maxSpeedReportLimit
	}
	if c.AlertCheckMinutes <= 0 {
		c.AlertCheckMinutes = defaultAlertCheckMinutes
	}
	if c.AlertAfterFailures < minAlertAfterFailures {
		c.AlertAfterFailures = defaultAlertAfterFailures
	}
	if c.AlertDiagnosticsMinutes <= 0 {
		if c.AlertRepeatMinutes > 0 {
			c.AlertDiagnosticsMinutes = c.AlertRepeatMinutes
		} else {
			c.AlertDiagnosticsMinutes = defaultAlertDiagnosticsMinutes
		}
	}
	c.AlertReminderScheduleMinutes = normalizeMinuteSchedule(c.AlertReminderScheduleMinutes)
	if len(c.AlertReminderScheduleMinutes) == 0 {
		c.AlertReminderScheduleMinutes = append([]int(nil), defaultAlertReminderScheduleMinutes...)
	}
	if c.AlertMaxReminderMinutes <= 0 {
		if c.AlertRepeatMinutes > 0 {
			c.AlertMaxReminderMinutes = c.AlertRepeatMinutes
		} else {
			c.AlertMaxReminderMinutes = defaultAlertMaxReminderMinutes
		}
	}
	if c.AlertMaxReminderMinutes < c.AlertReminderScheduleMinutes[len(c.AlertReminderScheduleMinutes)-1] {
		c.AlertMaxReminderMinutes = c.AlertReminderScheduleMinutes[len(c.AlertReminderScheduleMinutes)-1]
	}
	c.MutedNodeIDs = normalizeNodeIDs(c.MutedNodeIDs)
	c.MutedSpeedNodeIDs = normalizeNodeIDs(c.MutedSpeedNodeIDs)
	c.MutedAlertNodeIDs = normalizeNodeIDs(c.MutedAlertNodeIDs)
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = defaultTimeoutSec
	}
}

func applyLegacyAlertRepeat(data []byte, cfg *Config) {
	if cfg.AlertRepeatMinutes <= 0 {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if _, ok := raw["alertDiagnosticsMinutes"]; !ok {
		cfg.AlertDiagnosticsMinutes = cfg.AlertRepeatMinutes
	}
	if _, ok := raw["alertMaxReminderMinutes"]; !ok {
		cfg.AlertMaxReminderMinutes = cfg.AlertRepeatMinutes
	}
}

func applyEnvDefaults(cfg *Config) {
	if v := os.Getenv("TELEGRAM_ENABLED"); v != "" {
		cfg.Enabled = parseBool(v)
	}
	applyEnvSensitive(cfg)
	cfg.Normalize()
}

func applyEnvOverrides(cfg *Config) {
	if v, ok := os.LookupEnv("TELEGRAM_ENABLED"); ok && parseBool(v) {
		cfg.Enabled = true
	}
	applyEnvSensitive(cfg)
}

func applyEnvSensitive(cfg *Config) {
	if v, ok := os.LookupEnv("TELEGRAM_BOT_TOKEN"); ok {
		cfg.BotToken = v
	}
	if v, ok := os.LookupEnv("TELEGRAM_CHAT_ID"); ok {
		cfg.ChatID = v
	}
	if v, ok := os.LookupEnv("TELEGRAM_MESSAGE_THREAD_ID"); ok {
		cfg.MessageThreadID, _ = strconv.Atoi(v)
	}
	if v, ok := os.LookupEnv("TELEGRAM_ADMIN_IDS"); ok {
		cfg.AdminUserIDs = parseInt64List(v)
	}
}

func disableInvalidEnabledConfig(cfg *Config) {
	if !cfg.Enabled {
		return
	}
	if cfg.BotToken == "" {
		logger.Warn("Telegram is enabled but bot token is empty; disabling Telegram")
		cfg.Enabled = false
	}
}

func parseBool(value string) bool {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return parsed
}

func parseInt64List(value string) []int64 {
	var result []int64
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err == nil {
			result = append(result, id)
		}
	}
	return result
}

func normalizeMinuteSchedule(values []int) []int {
	seen := make(map[int]bool)
	var result []int
	for _, value := range values {
		if value <= 0 || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func parseMinuteSchedule(value string) []int {
	var result []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		minutes, err := strconv.Atoi(part)
		if err == nil {
			result = append(result, minutes)
		}
	}
	return normalizeMinuteSchedule(result)
}

func normalizeNodeIDs(values []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func formatIntList(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}
