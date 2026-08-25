package remnawave

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ConfigVersion  = 1
	RuntimeVersion = 2

	announceHeader       = "announce"
	announceValuePrefix  = "rwEncodeBase64:"
	defaultOutageMinutes = 15
	defaultMinFailures   = 3
	defaultRecoveryMins  = 5
	maxMessageRunes      = 240
	maxBaseAnnounceBytes = 16 * 1024
)

// Policy controls when the integration may publish a subscription announce.
// Enabled is deliberately persisted separately from the environment master
// switch so restoring or copying settings cannot enable API access by itself.
type Policy struct {
	Enabled         bool   `json:"enabled"`
	OutageMinutes   int    `json:"outageMinutes"`
	MinimumFailures int    `json:"minimumFailures"`
	RecoveryMinutes int    `json:"recoveryMinutes"`
	NormalMessage   string `json:"normalMessage,omitempty"`
}

type SquadPair struct {
	InternalSquadUUID string `json:"internalSquadUuid"`
	ExternalSquadUUID string `json:"externalSquadUuid"`
	MonitoringOnly    bool   `json:"monitoringOnly,omitempty"`
}

// NodeMapping is keyed by the checker StableID in ConfigFile.NodeMappings.
// Several StableIDs may intentionally point at one Remnawave Host UUID (for
// example after DNS expansion), and several hosts may share one GroupKey for
// public-location redundancy.
type NodeMapping struct {
	HostUUID    string `json:"hostUuid"`
	GroupKey    string `json:"groupKey,omitempty"`
	PublicLabel string `json:"publicLabel,omitempty"`
}

type ConfigFile struct {
	Version      int                    `json:"version"`
	UpdatedAt    time.Time              `json:"updatedAt"`
	Policy       Policy                 `json:"policy"`
	SquadPairs   []SquadPair            `json:"squadPairs"`
	NodeMappings map[string]NodeMapping `json:"nodeMappings"`
}

type Settings struct {
	Policy       Policy                 `json:"policy"`
	SquadPairs   []SquadPair            `json:"squadPairs"`
	NodeMappings map[string]NodeMapping `json:"nodeMappings"`
}

type ManagedAnnouncement struct {
	Value       string            `json:"value"`
	Message     string            `json:"message"`
	BasePresent bool              `json:"basePresent,omitempty"`
	BaseValue   string            `json:"baseValue,omitempty"`
	Groups      map[string]string `json:"groups,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

type RuntimeFile struct {
	Version   int                            `json:"version"`
	UpdatedAt time.Time                      `json:"updatedAt"`
	Managed   map[string]ManagedAnnouncement `json:"managed"`
}

type HostInbound struct {
	ConfigProfileUUID        string `json:"configProfileUuid"`
	ConfigProfileInboundUUID string `json:"configProfileInboundUuid"`
}

type Host struct {
	UUID                   string      `json:"uuid"`
	Remark                 string      `json:"remark"`
	Address                string      `json:"address"`
	Port                   int         `json:"port"`
	IsDisabled             bool        `json:"isDisabled"`
	IsHidden               bool        `json:"isHidden"`
	Inbound                HostInbound `json:"inbound"`
	ExcludedInternalSquads []string    `json:"excludedInternalSquads"`
}

type InternalInbound struct {
	UUID        string `json:"uuid"`
	ProfileUUID string `json:"profileUuid,omitempty"`
	Tag         string `json:"tag,omitempty"`
	Type        string `json:"type,omitempty"`
}

type InternalSquad struct {
	UUID     string            `json:"uuid"`
	Name     string            `json:"name"`
	Inbounds []InternalInbound `json:"inbounds"`
}

type ExternalSquad struct {
	UUID               string            `json:"uuid"`
	Name               string            `json:"name"`
	ResponseHeadersAdd map[string]string `json:"responseHeadersAdd"`
}

type Topology struct {
	LoadedAt       time.Time       `json:"loadedAt,omitempty"`
	Hosts          []Host          `json:"hosts"`
	InternalSquads []InternalSquad `json:"internalSquads"`
	ExternalSquads []ExternalSquad `json:"externalSquads"`
}

type ProxyOption struct {
	StableID string `json:"stableId"`
	Name     string `json:"name"`
	SubName  string `json:"subName"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

type ConnectionInfo struct {
	Enabled            bool   `json:"enabled"`
	Configured         bool   `json:"configured"`
	APIURL             string `json:"apiUrl,omitempty"`
	APITokenConfigured bool   `json:"apiTokenConfigured"`
}

type RemoteAnnouncementStatus struct {
	ExternalSquadUUID string `json:"externalSquadUuid"`
	ExternalSquadName string `json:"externalSquadName,omitempty"`
	Present           bool   `json:"present"`
	Managed           bool   `json:"managed"`
	PreservesBase     bool   `json:"preservesBase,omitempty"`
	Message           string `json:"message,omitempty"`
}

type ReconcileStatus struct {
	LastTopologyAt  time.Time                  `json:"lastTopologyAt,omitempty"`
	LastReconcileAt time.Time                  `json:"lastReconcileAt,omitempty"`
	LastError       string                     `json:"lastError,omitempty"`
	Conflicts       []string                   `json:"conflicts,omitempty"`
	Announcements   []RemoteAnnouncementStatus `json:"announcements"`
}

type Snapshot struct {
	Connection ConnectionInfo  `json:"connection"`
	Settings   Settings        `json:"settings"`
	Topology   Topology        `json:"topology"`
	Proxies    []ProxyOption   `json:"proxies"`
	Status     ReconcileStatus `json:"status"`
}

func defaultPolicy() Policy {
	return Policy{
		OutageMinutes:   defaultOutageMinutes,
		MinimumFailures: defaultMinFailures,
		RecoveryMinutes: defaultRecoveryMins,
	}
}

func defaultConfig() ConfigFile {
	return ConfigFile{
		Version:      ConfigVersion,
		Policy:       defaultPolicy(),
		SquadPairs:   []SquadPair{},
		NodeMappings: map[string]NodeMapping{},
	}
}

func defaultRuntime() RuntimeFile {
	return RuntimeFile{
		Version: RuntimeVersion,
		Managed: map[string]ManagedAnnouncement{},
	}
}

func normalizeConfig(config *ConfigFile) {
	if config.Version == 0 {
		// Version 0 is the short-lived unversioned development format. Its
		// fields are identical to v1, so migration is lossless.
		config.Version = ConfigVersion
	}
	if config.Policy.OutageMinutes == 0 {
		config.Policy.OutageMinutes = defaultOutageMinutes
	}
	if config.Policy.MinimumFailures == 0 {
		config.Policy.MinimumFailures = defaultMinFailures
	}
	if config.Policy.RecoveryMinutes == 0 {
		config.Policy.RecoveryMinutes = defaultRecoveryMins
	}
	config.Policy.NormalMessage = strings.TrimSpace(config.Policy.NormalMessage)
	if config.SquadPairs == nil {
		config.SquadPairs = []SquadPair{}
	}
	for index := range config.SquadPairs {
		config.SquadPairs[index].InternalSquadUUID = strings.TrimSpace(config.SquadPairs[index].InternalSquadUUID)
		config.SquadPairs[index].ExternalSquadUUID = strings.TrimSpace(config.SquadPairs[index].ExternalSquadUUID)
	}
	if config.NodeMappings == nil {
		config.NodeMappings = map[string]NodeMapping{}
	}
	for stableID, mapping := range config.NodeMappings {
		mapping.HostUUID = strings.TrimSpace(mapping.HostUUID)
		mapping.GroupKey = strings.TrimSpace(mapping.GroupKey)
		mapping.PublicLabel = strings.TrimSpace(mapping.PublicLabel)
		config.NodeMappings[stableID] = mapping
	}
}

func normalizeRuntime(runtime *RuntimeFile) {
	if runtime.Version == 0 || runtime.Version == 1 {
		// v1 owned the whole announce value. Migrating it with no base keeps
		// the old exact-delete behavior while new writes can preserve a base.
		runtime.Version = RuntimeVersion
	}
	if runtime.Managed == nil {
		runtime.Managed = map[string]ManagedAnnouncement{}
	}
	for externalUUID, managed := range runtime.Managed {
		if managed.Groups == nil {
			managed.Groups = map[string]string{}
		}
		runtime.Managed[externalUUID] = managed
	}
}

func validateConfig(config ConfigFile) error {
	if config.Version != ConfigVersion {
		return fmt.Errorf("unsupported Remnawave config version %d", config.Version)
	}
	if config.Policy.OutageMinutes < 1 || config.Policy.OutageMinutes > 24*60 {
		return fmt.Errorf("outageMinutes must be between 1 and 1440")
	}
	if config.Policy.MinimumFailures < 2 || config.Policy.MinimumFailures > 100 {
		return fmt.Errorf("minimumFailures must be between 2 and 100")
	}
	if config.Policy.RecoveryMinutes < 1 || config.Policy.RecoveryMinutes > 24*60 {
		return fmt.Errorf("recoveryMinutes must be between 1 and 1440")
	}
	if err := validateDisplayText("normalMessage", config.Policy.NormalMessage, maxMessageRunes); err != nil {
		return err
	}

	internalSeen := make(map[string]bool)
	externalTargets := make(map[string]bool)
	groupLabels := make(map[string]string)
	stableIDs := make(map[string]string)
	for index, pair := range config.SquadPairs {
		if pair.InternalSquadUUID == "" || pair.ExternalSquadUUID == "" {
			return fmt.Errorf("squadPairs[%d] requires both squad UUIDs", index)
		}
		if invalidIdentifier(pair.InternalSquadUUID) || invalidIdentifier(pair.ExternalSquadUUID) {
			return fmt.Errorf("squadPairs[%d] contains an invalid squad UUID", index)
		}
		if internalSeen[pair.InternalSquadUUID] {
			return fmt.Errorf("internal squad %s is mapped more than once", pair.InternalSquadUUID)
		}
		internalSeen[pair.InternalSquadUUID] = true
		if !pair.MonitoringOnly {
			if externalTargets[pair.ExternalSquadUUID] {
				return fmt.Errorf("external squad %s is targeted more than once", pair.ExternalSquadUUID)
			}
			externalTargets[pair.ExternalSquadUUID] = true
		}
	}

	for stableID, mapping := range config.NodeMappings {
		if stableID == "" || len(stableID) > 512 || strings.ContainsAny(stableID, "\r\n") {
			return fmt.Errorf("nodeMappings contains an invalid StableID")
		}
		foldedStableID := strings.ToLower(stableID)
		if previous, exists := stableIDs[foldedStableID]; exists && previous != stableID {
			return fmt.Errorf("nodeMappings contains case-insensitive StableID collision: %s and %s", previous, stableID)
		}
		stableIDs[foldedStableID] = stableID
		if mapping.HostUUID == "" || invalidIdentifier(mapping.HostUUID) {
			return fmt.Errorf("nodeMappings[%s] requires a valid host UUID", stableID)
		}
		if mapping.GroupKey != "" && (len(mapping.GroupKey) > 128 || strings.ContainsAny(mapping.GroupKey, "\r\n")) {
			return fmt.Errorf("nodeMappings[%s].groupKey is invalid", stableID)
		}
		if err := validateDisplayText("nodeMappings["+stableID+"].publicLabel", mapping.PublicLabel, 80); err != nil {
			return err
		}
		if mapping.GroupKey != "" && mapping.PublicLabel != "" {
			if previous, exists := groupLabels[mapping.GroupKey]; exists && previous != mapping.PublicLabel {
				return fmt.Errorf("redundancy group %s has conflicting public labels", mapping.GroupKey)
			}
			groupLabels[mapping.GroupKey] = mapping.PublicLabel
		}
	}
	return nil
}

func validateRuntime(runtime RuntimeFile) error {
	if runtime.Version != RuntimeVersion {
		return fmt.Errorf("unsupported Remnawave runtime version %d", runtime.Version)
	}
	for externalUUID, managed := range runtime.Managed {
		if invalidIdentifier(externalUUID) {
			return fmt.Errorf("managed announce contains an invalid external squad UUID")
		}
		if managed.Message == "" {
			return fmt.Errorf("managed announce for %s has an empty status message", externalUUID)
		}
		if !strings.HasPrefix(managed.Value, announceValuePrefix) {
			return fmt.Errorf("managed announce for %s has an invalid value", externalUUID)
		}
		if managed.BasePresent {
			if !isAppendableBaseAnnounce(managed.BaseValue) {
				return fmt.Errorf("managed announce for %s has an invalid base value", externalUUID)
			}
		} else if managed.BaseValue != "" {
			return fmt.Errorf("managed announce for %s has a base value without basePresent", externalUUID)
		}
		if managed.Value != composeManagedAnnounce(managed.BasePresent, managed.BaseValue, managed.Message) {
			return fmt.Errorf("managed announce for %s has inconsistent value and message", externalUUID)
		}
		if err := validateDisplayText("managed announce", managed.Message, maxMessageRunes); err != nil {
			return err
		}
	}
	return nil
}

func isAppendableBaseAnnounce(value string) bool {
	return len(value) > len(announceValuePrefix) &&
		len(value) <= maxBaseAnnounceBytes &&
		strings.HasPrefix(value, announceValuePrefix) &&
		!strings.ContainsAny(value, "\r\n")
}

func composeManagedAnnounce(basePresent bool, baseValue, message string) string {
	if message == "" {
		if basePresent {
			return baseValue
		}
		return ""
	}
	if basePresent {
		return baseValue + "\n" + message
	}
	return announceValuePrefix + message
}

func validateDisplayText(name, value string, maxRunes int) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must not contain line breaks", name)
	}
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return fmt.Errorf("%s must not contain Remnawave template delimiters", name)
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return fmt.Errorf("%s must not contain URLs", name)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s must not exceed %d characters", name, maxRunes)
	}
	return nil
}

func invalidIdentifier(value string) bool {
	return value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\t ")
}

func configSettings(config ConfigFile) Settings {
	return Settings{
		Policy:       config.Policy,
		SquadPairs:   append([]SquadPair(nil), config.SquadPairs...),
		NodeMappings: cloneNodeMappings(config.NodeMappings),
	}
}

func cloneNodeMappings(input map[string]NodeMapping) map[string]NodeMapping {
	result := make(map[string]NodeMapping, len(input))
	for stableID, mapping := range input {
		result[stableID] = mapping
	}
	return result
}

func cloneManaged(input map[string]ManagedAnnouncement) map[string]ManagedAnnouncement {
	result := make(map[string]ManagedAnnouncement, len(input))
	for externalUUID, managed := range input {
		managed.Groups = cloneStringMap(managed.Groups)
		result[externalUUID] = managed
	}
	return result
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sortedMapKeys[V any](input map[string]V) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
