package nodearchive

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

const stateVersion = 1

const (
	incidentKindNode       = "node"
	incidentKindMass       = "mass"
	incidentStatusActive   = "active"
	incidentStatusResolved = "resolved"
	maxIncidentRecords     = 1000
)

type Store struct {
	path         string
	proxyChecker *checker.ProxyChecker
	httpClient   *http.Client

	mu          sync.RWMutex
	nodes       map[string]NodeRecord
	mergedNodes map[string][]string
	incidents   []IncidentRecord
}

type StateFile struct {
	Version     int                   `json:"version"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	Nodes       map[string]NodeRecord `json:"nodes"`
	MergedNodes map[string][]string   `json:"mergedNodes,omitempty"`
	Incidents   []IncidentRecord      `json:"incidents,omitempty"`
}

// IncidentRecord is an append-oriented operational journal entry. Node
// incidents retain the exact StableID, while mass incidents capture a
// correlated global or subscription-scoped failure.
type IncidentRecord struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Status        string    `json:"status"`
	Scope         string    `json:"scope"`
	Subscription  string    `json:"subscription,omitempty"`
	StableIDs     []string  `json:"stableIds"`
	NodeNames     []string  `json:"nodeNames,omitempty"`
	AffectedCount int       `json:"affectedCount"`
	TotalCount    int       `json:"totalCount"`
	CauseCode     string    `json:"causeCode"`
	CauseSummary  string    `json:"causeSummary"`
	CauseDetail   string    `json:"causeDetail,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	ResolvedAt    time.Time `json:"resolvedAt,omitempty"`
	DurationSec   int64     `json:"durationSec"`
}

type NodeRecord struct {
	StableID            string    `json:"stableId"`
	Name                string    `json:"name"`
	SubName             string    `json:"subName"`
	Server              string    `json:"server"`
	Port                int       `json:"port"`
	Protocol            string    `json:"protocol"`
	Active              bool      `json:"active"`
	FirstSeenAt         time.Time `json:"firstSeenAt"`
	LastSeenAt          time.Time `json:"lastSeenAt"`
	RetiredAt           time.Time `json:"retiredAt,omitempty"`
	ClaimedCountry      string    `json:"claimedCountry,omitempty"`
	ClaimedCountryCode  string    `json:"claimedCountryCode,omitempty"`
	GeoIP               string    `json:"geoIp,omitempty"`
	GeoCountry          string    `json:"geoCountry,omitempty"`
	GeoCountryCode      string    `json:"geoCountryCode,omitempty"`
	GeoOrg              string    `json:"geoOrg,omitempty"`
	GeoUpdatedAt        time.Time `json:"geoUpdatedAt,omitempty"`
	GeoError            string    `json:"geoError,omitempty"`
	IfconfigIP          string    `json:"ifconfigIp,omitempty"`
	IfconfigCountry     string    `json:"ifconfigCountry,omitempty"`
	IfconfigCountryCode string    `json:"ifconfigCountryCode,omitempty"`
	IfconfigASN         string    `json:"ifconfigAsn,omitempty"`
	IfconfigOrg         string    `json:"ifconfigOrg,omitempty"`
	IfconfigUpdatedAt   time.Time `json:"ifconfigUpdatedAt,omitempty"`
	IfconfigError       string    `json:"ifconfigError,omitempty"`
	TotalDowntimeSec    int64     `json:"totalDowntimeSec"`
	IncidentCount       int       `json:"incidentCount"`
	LongestDowntimeSec  int64     `json:"longestDowntimeSec"`
	CurrentDownSince    time.Time `json:"currentDownSince,omitempty"`
	LastOfflineAt       time.Time `json:"lastOfflineAt,omitempty"`
	LastOnlineAt        time.Time `json:"lastOnlineAt,omitempty"`
	LastStatusAt        time.Time `json:"lastStatusAt,omitempty"`
}

type Summary struct {
	NodeRecord
	MergedFromStableIDs []string          `json:"mergedFromStableIds,omitempty"`
	IPInfoURL           string            `json:"ipInfoUrl"`
	CountryMatch        string            `json:"countryMatch"`
	GeoSources          []GeoSource       `json:"geoSources"`
	GeoBlacklistHits    []GeoBlacklistHit `json:"geoBlacklistHits,omitempty"`
	GeoBlacklisted      bool              `json:"geoBlacklisted"`
	ResultCount         int               `json:"resultCount"`
	SuccessfulResults   int               `json:"successfulResults"`
	FailedResults       int               `json:"failedResults"`
	AvgMbps             float64           `json:"avgMbps"`
	MinMbps             float64           `json:"minMbps"`
	MaxMbps             float64           `json:"maxMbps"`
	LastMbps            float64           `json:"lastMbps"`
	LastSpeedAt         time.Time         `json:"lastSpeedAt,omitempty"`
	LastSpeedError      string            `json:"lastSpeedError,omitempty"`
	LastSpeedOffline    bool              `json:"lastSpeedOffline"`
}

type GeoSource struct {
	Source      string    `json:"source"`
	IP          string    `json:"ip,omitempty"`
	Country     string    `json:"country,omitempty"`
	CountryCode string    `json:"countryCode,omitempty"`
	Org         string    `json:"org,omitempty"`
	ASN         string    `json:"asn,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type GeoBlacklistHit struct {
	Source      string `json:"source"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
}

type GeoRefreshResult struct {
	Updated int      `json:"updated"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

type ipInfoResponse struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	Org     string `json:"org"`
	Error   any    `json:"error"`
}

type ifconfigResponse struct {
	IP         string `json:"ip"`
	Country    string `json:"country"`
	CountryISO string `json:"country_iso"`
	ASN        string `json:"asn"`
	ASNOrg     string `json:"asn_org"`
}

func NewStore(path string, proxyChecker *checker.ProxyChecker) *Store {
	return &Store{
		path:         path,
		proxyChecker: proxyChecker,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		nodes:       make(map[string]NodeRecord),
		mergedNodes: make(map[string][]string),
		incidents:   make([]IncidentRecord, 0),
	}
}

// ClaimedCountryCode returns the user-declared country inferred from the node
// name or subscription name. GeoIP sources are deliberately not consulted.
func (s *Store) ClaimedCountryCode(stableID string) string {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.ToUpper(strings.TrimSpace(s.nodes[stableID].ClaimedCountryCode))
}

func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	nodes := make(map[string]NodeRecord, len(state.Nodes))
	for stableID, record := range state.Nodes {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			stableID = strings.TrimSpace(record.StableID)
		}
		if stableID == "" {
			continue
		}
		record.StableID = stableID
		normalizeRecordCountries(&record)
		nodes[stableID] = record
	}
	mergedNodes := normalizeMergedNodes(state.MergedNodes, nodes)
	incidents := normalizeIncidents(state.Incidents)

	s.mu.Lock()
	s.nodes = nodes
	s.mergedNodes = mergedNodes
	s.incidents = incidents
	s.mu.Unlock()
	return nil
}

func (s *Store) SyncProxies(proxies []*models.ProxyConfig) error {
	now := time.Now()
	changed := false
	active := make(map[string]bool, len(proxies))

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if proxy.StableID == "" {
			continue
		}
		active[proxy.StableID] = true
		record := s.nodes[proxy.StableID]
		previous := record
		record = applyProxy(record, proxy, now)
		record.Active = true
		record.RetiredAt = time.Time{}
		if previous != record {
			s.nodes[proxy.StableID] = record
			changed = true
		}
	}

	for stableID, record := range s.nodes {
		if !record.Active || active[stableID] {
			continue
		}
		previous := record
		record.Active = false
		record.RetiredAt = now
		record.LastSeenAt = now
		record = closeDowntime(record, now)
		if incidentIndex := s.findActiveIncidentLocked(incidentKindNode, "node:"+stableID); incidentIndex >= 0 {
			s.resolveIncidentLocked(incidentIndex, now)
			changed = true
		}
		if previous != record {
			s.nodes[stableID] = record
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) SyncSpeedHistory(history map[string][]speedtest.Result) error {
	if len(history) == 0 {
		return nil
	}
	now := time.Now()
	changed := false

	s.mu.Lock()
	defer s.mu.Unlock()

	for stableID, entries := range history {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" || len(entries) == 0 {
			continue
		}
		record := s.nodes[stableID]
		previous := record
		if record.StableID == "" {
			record.StableID = stableID
			record.Active = false
		}
		for _, entry := range entries {
			if record.Name == "" {
				record.Name = entry.Name
			}
			if record.SubName == "" {
				record.SubName = entry.SubName
			}
			if record.Protocol == "" {
				record.Protocol = entry.Protocol
			}
			if !entry.CheckedAt.IsZero() {
				if record.FirstSeenAt.IsZero() || entry.CheckedAt.Before(record.FirstSeenAt) {
					record.FirstSeenAt = entry.CheckedAt
				}
				if record.LastSeenAt.IsZero() || entry.CheckedAt.After(record.LastSeenAt) {
					record.LastSeenAt = entry.CheckedAt
				}
			}
		}
		if record.FirstSeenAt.IsZero() {
			record.FirstSeenAt = now
		}
		if record.LastSeenAt.IsZero() {
			record.LastSeenAt = now
		}
		normalizeRecordCountries(&record)
		if previous != record {
			s.nodes[stableID] = record
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) RecordAvailability() error {
	if s.proxyChecker == nil {
		return nil
	}
	proxies := s.proxyChecker.GetProxies()
	now := time.Now()
	changed := false
	active := make(map[string]bool, len(proxies))
	detailsByStableID := make(map[string]checker.ProxyStatusDetails, len(proxies))

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if proxy.StableID == "" {
			continue
		}
		active[proxy.StableID] = true
		record := applyProxy(s.nodes[proxy.StableID], proxy, now)
		previous := record
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err == nil {
			detailsByStableID[proxy.StableID] = details
			record = applyAvailability(record, details, now)
			if s.updateNodeIncidentLocked(proxy, details, now) {
				changed = true
			}
		}
		if previous != record || s.nodes[proxy.StableID] != record {
			s.nodes[proxy.StableID] = record
			changed = true
		}
	}
	if s.updateMassIncidentsLocked(proxies, detailsByStableID, now) {
		changed = true
	}
	if s.pruneIncidentsLocked() {
		changed = true
	}

	for stableID, record := range s.nodes {
		if !record.Active || active[stableID] {
			continue
		}
		previous := record
		record.Active = false
		record.RetiredAt = now
		record.LastSeenAt = now
		record = closeDowntime(record, now)
		if previous != record {
			s.nodes[stableID] = record
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) Summaries(history map[string][]speedtest.Result) []Summary {
	s.mu.RLock()
	records := make(map[string]NodeRecord, len(s.nodes))
	for stableID, record := range s.nodes {
		records[stableID] = record
	}
	mergedNodes := copyMergedNodes(s.mergedNodes)
	s.mu.RUnlock()

	for stableID, entries := range history {
		if _, ok := records[stableID]; ok || len(entries) == 0 {
			continue
		}
		record := NodeRecord{StableID: stableID}
		for _, entry := range entries {
			if record.Name == "" {
				record.Name = entry.Name
			}
			if record.SubName == "" {
				record.SubName = entry.SubName
			}
			if record.Protocol == "" {
				record.Protocol = entry.Protocol
			}
			if !entry.CheckedAt.IsZero() {
				if record.FirstSeenAt.IsZero() || entry.CheckedAt.Before(record.FirstSeenAt) {
					record.FirstSeenAt = entry.CheckedAt
				}
				if record.LastSeenAt.IsZero() || entry.CheckedAt.After(record.LastSeenAt) {
					record.LastSeenAt = entry.CheckedAt
				}
			}
		}
		normalizeRecordCountries(&record)
		records[stableID] = record
	}

	result := make([]Summary, 0, len(records))
	for stableID, record := range records {
		summary := Summary{
			NodeRecord:          record,
			MergedFromStableIDs: append([]string(nil), mergedNodes[stableID]...),
		}
		summary.IPInfoURL = ipInfoURL(record.Server)
		summary.GeoSources = geoSources(record)
		summary.GeoBlacklistHits = geoBlacklistHits(summary.GeoSources)
		summary.GeoBlacklisted = len(summary.GeoBlacklistHits) > 0
		summary.CountryMatch = countryMatch(record)
		applySpeedStats(&summary, history[stableID])
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Active != result[j].Active {
			return result[i].Active
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

// Incidents returns newest-first copies of the persisted incident journal.
func (s *Store) Incidents(limit int) []IncidentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	incidents := append([]IncidentRecord(nil), s.incidents...)
	for index := range incidents {
		incidents[index].StableIDs = append([]string(nil), incidents[index].StableIDs...)
		incidents[index].NodeNames = append([]string(nil), incidents[index].NodeNames...)
	}
	sort.Slice(incidents, func(i, j int) bool {
		return incidents[i].StartedAt.After(incidents[j].StartedAt)
	})
	if limit > 0 && len(incidents) > limit {
		incidents = incidents[:limit]
	}
	return incidents
}

func (s *Store) RefreshGeo(ctx context.Context, stableIDs []string) (GeoRefreshResult, error) {
	s.mu.RLock()
	records := make(map[string]NodeRecord, len(s.nodes))
	for id, record := range s.nodes {
		records[id] = record
	}
	s.mu.RUnlock()

	selected := make(map[string]bool)
	for _, stableID := range stableIDs {
		stableID = strings.TrimSpace(stableID)
		if stableID != "" {
			selected[stableID] = true
		}
	}

	var result GeoRefreshResult
	updates := make(map[string]NodeRecord)
	for stableID, record := range records {
		if len(selected) > 0 && !selected[stableID] {
			continue
		}
		if strings.TrimSpace(record.Server) == "" {
			continue
		}
		updated, successes, errs := s.lookupGeo(ctx, record)
		if successes == 0 {
			result.Failed++
		} else {
			result.Updated++
		}
		for _, err := range errs {
			if len(result.Errors) < 5 {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", stableID, err))
			}
		}
		updates[stableID] = updated
	}

	if len(updates) == 0 {
		return result, nil
	}

	s.mu.Lock()
	for stableID, record := range updates {
		current, ok := s.nodes[stableID]
		if !ok {
			continue
		}
		// The server may have changed while the external lookups were in
		// flight. In that case their result no longer describes this node.
		if serverHost(current.Server) != serverHost(record.Server) {
			continue
		}
		s.nodes[stableID] = mergeGeoFields(current, record)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	return result, err
}

func mergeGeoFields(current NodeRecord, geo NodeRecord) NodeRecord {
	current.GeoIP = geo.GeoIP
	current.GeoCountry = geo.GeoCountry
	current.GeoCountryCode = geo.GeoCountryCode
	current.GeoOrg = geo.GeoOrg
	current.GeoUpdatedAt = geo.GeoUpdatedAt
	current.GeoError = geo.GeoError
	current.IfconfigIP = geo.IfconfigIP
	current.IfconfigCountry = geo.IfconfigCountry
	current.IfconfigCountryCode = geo.IfconfigCountryCode
	current.IfconfigASN = geo.IfconfigASN
	current.IfconfigOrg = geo.IfconfigOrg
	current.IfconfigUpdatedAt = geo.IfconfigUpdatedAt
	current.IfconfigError = geo.IfconfigError
	return current
}

func (s *Store) DeleteArchived(stableID string) error {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.nodes[stableID]
	if !ok {
		return fmt.Errorf("node not found")
	}
	if record.Active {
		return fmt.Errorf("active nodes are managed by subscription and cannot be deleted")
	}
	mergedFrom := append([]string(nil), s.mergedNodes[stableID]...)
	delete(s.nodes, stableID)
	delete(s.mergedNodes, stableID)
	if err := s.saveLocked(); err != nil {
		s.nodes[stableID] = record
		if len(mergedFrom) > 0 {
			s.mergedNodes[stableID] = mergedFrom
		}
		return err
	}
	return nil
}

func (s *Store) ArchivedRecord(stableID string) (NodeRecord, error) {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return NodeRecord{}, fmt.Errorf("stableId is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.nodes[stableID]
	if !ok {
		return NodeRecord{}, fmt.Errorf("node not found")
	}
	if record.Active {
		return NodeRecord{}, fmt.Errorf("active nodes are managed by subscription and cannot be deleted")
	}
	return record, nil
}

func (s *Store) RestoreArchived(record NodeRecord, mergedFrom ...string) error {
	stableID := strings.TrimSpace(record.StableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}
	if record.Active {
		return fmt.Errorf("cannot restore an active record as archived")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.nodes[stableID]
	previousMerged, hadMerged := s.mergedNodes[stableID]
	s.nodes[stableID] = record
	restoredMerged := uniqueSortedStrings(mergedFrom)
	if len(restoredMerged) > 0 {
		s.mergedNodes[stableID] = restoredMerged
	} else {
		delete(s.mergedNodes, stableID)
	}
	if err := s.saveLocked(); err != nil {
		if existed {
			s.nodes[stableID] = previous
		} else {
			delete(s.nodes, stableID)
		}
		if hadMerged {
			s.mergedNodes[stableID] = previousMerged
		} else {
			delete(s.mergedNodes, stableID)
		}
		return err
	}
	return nil
}

// MergedFromStableIDs returns the persisted identity lineage for a node.
func (s *Store) MergedFromStableIDs(stableID string) []string {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.mergedNodes[stableID]...)
}

// Record returns a copy of a persisted node record regardless of active state.
func (s *Store) Record(stableID string) (NodeRecord, error) {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return NodeRecord{}, fmt.Errorf("stableId is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.nodes[stableID]
	if !ok {
		return NodeRecord{}, fmt.Errorf("node not found")
	}
	return record, nil
}

func (s *Store) lookupGeo(ctx context.Context, record NodeRecord) (NodeRecord, int, []error) {
	target := serverHost(record.Server)
	if target == "" {
		return record, 0, []error{fmt.Errorf("server is empty")}
	}

	now := time.Now()
	successes := 0
	var errors []error
	updated, err := s.lookupIPInfo(ctx, record, target, now)
	if err != nil {
		record.GeoError = err.Error()
		record.GeoUpdatedAt = now
		errors = append(errors, err)
	} else {
		record = updated
		successes++
	}

	updated, err = s.lookupIfconfig(ctx, record, target, now)
	if err != nil {
		record.IfconfigError = err.Error()
		record.IfconfigUpdatedAt = now
		errors = append(errors, err)
	} else {
		record = updated
		successes++
	}

	return record, successes, errors
}

func (s *Store) lookupIPInfo(ctx context.Context, record NodeRecord, target string, now time.Time) (NodeRecord, error) {
	reqURL := "https://ipinfo.io/" + url.PathEscape(target) + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return record, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return record, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return record, fmt.Errorf("ipinfo status %d", resp.StatusCode)
	}

	var payload ipInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return record, err
	}
	if payload.Error != nil {
		return record, fmt.Errorf("ipinfo error")
	}

	countryCode := normalizeGeoCountryCode(payload.Country, "")
	if countryCode == "" {
		return record, fmt.Errorf("ipinfo country missing")
	}
	record.GeoIP = strings.TrimSpace(payload.IP)
	record.GeoCountryCode = countryCode
	record.GeoCountry = countryName(countryCode)
	record.GeoOrg = strings.TrimSpace(payload.Org)
	record.GeoUpdatedAt = now
	record.GeoError = ""
	return record, nil
}

func (s *Store) lookupIfconfig(ctx context.Context, record NodeRecord, target string, now time.Time) (NodeRecord, error) {
	values := url.Values{}
	values.Set("ip", target)
	reqURL := "https://ifconfig.net/?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return record, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return record, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return record, fmt.Errorf("ifconfig status %d", resp.StatusCode)
	}

	var payload ifconfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return record, err
	}

	countryCode := normalizeGeoCountryCode(payload.CountryISO, payload.Country)
	if countryCode == "" {
		return record, fmt.Errorf("ifconfig country missing")
	}
	record.IfconfigIP = strings.TrimSpace(payload.IP)
	record.IfconfigCountryCode = countryCode
	if strings.TrimSpace(payload.Country) != "" {
		record.IfconfigCountry = strings.TrimSpace(payload.Country)
	} else {
		record.IfconfigCountry = countryName(countryCode)
	}
	record.IfconfigASN = strings.TrimSpace(payload.ASN)
	record.IfconfigOrg = strings.TrimSpace(payload.ASNOrg)
	record.IfconfigUpdatedAt = now
	record.IfconfigError = ""
	return record, nil
}

func applyProxy(record NodeRecord, proxy *models.ProxyConfig, now time.Time) NodeRecord {
	if proxy.StableID == "" {
		proxy.StableID = proxy.GenerateStableID()
	}
	if record.StableID == "" {
		record.StableID = proxy.StableID
	}
	if record.FirstSeenAt.IsZero() {
		record.FirstSeenAt = now
	}
	record.LastSeenAt = now
	record.Name = proxy.Name
	record.SubName = proxy.SubName
	record.Server = proxy.Server
	record.Port = proxy.Port
	record.Protocol = proxy.Protocol
	record.Active = true
	normalizeRecordCountries(&record)
	return record
}

func applyAvailability(record NodeRecord, details checker.ProxyStatusDetails, now time.Time) NodeRecord {
	record.LastStatusAt = now
	if details.Online {
		record.LastOnlineAt = now
		record = closeDowntime(record, now)
		return record
	}

	downSince := details.DownSince
	if downSince.IsZero() {
		downSince = now
	}
	record.LastOfflineAt = now
	if record.CurrentDownSince.IsZero() {
		record.CurrentDownSince = downSince
		record.IncidentCount++
	}
	return record
}

func closeDowntime(record NodeRecord, closedAt time.Time) NodeRecord {
	if record.CurrentDownSince.IsZero() {
		return record
	}
	if closedAt.Before(record.CurrentDownSince) {
		record.CurrentDownSince = time.Time{}
		return record
	}
	durationSec := int64(closedAt.Sub(record.CurrentDownSince).Seconds())
	record.TotalDowntimeSec += durationSec
	if durationSec > record.LongestDowntimeSec {
		record.LongestDowntimeSec = durationSec
	}
	record.CurrentDownSince = time.Time{}
	return record
}

func (s *Store) updateNodeIncidentLocked(proxy *models.ProxyConfig, details checker.ProxyStatusDetails, now time.Time) bool {
	if proxy == nil || proxy.StableID == "" {
		return false
	}
	index := s.findActiveIncidentLocked(incidentKindNode, "node:"+proxy.StableID)
	if details.Online {
		if index < 0 {
			return false
		}
		s.resolveIncidentLocked(index, now)
		return true
	}

	failure := details.Failure
	if failure.Code == "" {
		failure.Code = checker.FailureCodeUnknown
		failure.Summary = checker.FailureSummary(failure.Code)
	}
	startedAt := details.DownSince
	if startedAt.IsZero() {
		startedAt = now
	}
	if index < 0 {
		s.incidents = append(s.incidents, IncidentRecord{
			ID:            incidentID("node", proxy.StableID, startedAt),
			Kind:          incidentKindNode,
			Status:        incidentStatusActive,
			Scope:         "node:" + proxy.StableID,
			Subscription:  proxy.SubName,
			StableIDs:     []string{proxy.StableID},
			NodeNames:     []string{proxy.Name},
			AffectedCount: 1,
			TotalCount:    1,
			CauseCode:     failure.Code,
			CauseSummary:  failure.Summary,
			CauseDetail:   failure.Detail,
			StartedAt:     startedAt,
			UpdatedAt:     now,
		})
		return true
	}
	incident := &s.incidents[index]
	incident.CauseCode = failure.Code
	incident.CauseSummary = failure.Summary
	incident.CauseDetail = failure.Detail
	incident.UpdatedAt = now
	incident.DurationSec = durationSeconds(incident.StartedAt, now)
	return true
}

type massIncidentCandidate struct {
	Scope        string
	Subscription string
	StableIDs    []string
	NodeNames    []string
	TotalCount   int
	Cause        checker.FailureDetails
}

type incidentNodeFailure struct {
	proxy   *models.ProxyConfig
	details checker.ProxyStatusDetails
}

func (s *Store) updateMassIncidentsLocked(proxies []*models.ProxyConfig, detailsByStableID map[string]checker.ProxyStatusDetails, now time.Time) bool {
	candidates := correlateMassIncidents(proxies, detailsByStableID)
	activeScopes := make(map[string]bool, len(candidates))
	changed := false
	for _, candidate := range candidates {
		activeScopes[candidate.Scope] = true
		index := s.findActiveIncidentLocked(incidentKindMass, candidate.Scope)
		if index < 0 {
			s.incidents = append(s.incidents, IncidentRecord{
				ID:            incidentID("mass", candidate.Scope, now),
				Kind:          incidentKindMass,
				Status:        incidentStatusActive,
				Scope:         candidate.Scope,
				Subscription:  candidate.Subscription,
				StableIDs:     append([]string(nil), candidate.StableIDs...),
				NodeNames:     append([]string(nil), candidate.NodeNames...),
				AffectedCount: len(candidate.StableIDs),
				TotalCount:    candidate.TotalCount,
				CauseCode:     candidate.Cause.Code,
				CauseSummary:  candidate.Cause.Summary,
				CauseDetail:   candidate.Cause.Detail,
				StartedAt:     now,
				UpdatedAt:     now,
			})
			changed = true
			continue
		}
		incident := &s.incidents[index]
		incident.StableIDs = append([]string(nil), candidate.StableIDs...)
		incident.NodeNames = append([]string(nil), candidate.NodeNames...)
		incident.AffectedCount = len(candidate.StableIDs)
		incident.TotalCount = candidate.TotalCount
		incident.CauseCode = candidate.Cause.Code
		incident.CauseSummary = candidate.Cause.Summary
		incident.CauseDetail = candidate.Cause.Detail
		incident.UpdatedAt = now
		incident.DurationSec = durationSeconds(incident.StartedAt, now)
		changed = true
	}
	for index := range s.incidents {
		incident := &s.incidents[index]
		if incident.Kind == incidentKindMass && incident.Status == incidentStatusActive && !activeScopes[incident.Scope] {
			s.resolveIncidentLocked(index, now)
			changed = true
		}
	}
	return changed
}

func correlateMassIncidents(proxies []*models.ProxyConfig, detailsByStableID map[string]checker.ProxyStatusDetails) []massIncidentCandidate {
	var failed []incidentNodeFailure
	active := make([]*models.ProxyConfig, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.StableID == "" {
			continue
		}
		active = append(active, proxy)
		if details, ok := detailsByStableID[proxy.StableID]; ok && !details.Online {
			failed = append(failed, incidentNodeFailure{proxy: proxy, details: details})
		}
	}
	if candidate, ok := massCandidate("global", "", active, failed); ok {
		return []massIncidentCandidate{candidate}
	}

	bySubscription := make(map[string][]*models.ProxyConfig)
	failedBySubscription := make(map[string][]incidentNodeFailure)
	for _, proxy := range active {
		bySubscription[proxy.SubName] = append(bySubscription[proxy.SubName], proxy)
	}
	for _, item := range failed {
		failedBySubscription[item.proxy.SubName] = append(failedBySubscription[item.proxy.SubName], item)
	}
	var result []massIncidentCandidate
	for subscription, subscriptionProxies := range bySubscription {
		scope := "subscription:" + subscription
		if subscription == "" {
			scope = "subscription:(unnamed)"
		}
		if candidate, ok := massCandidate(scope, subscription, subscriptionProxies, failedBySubscription[subscription]); ok {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Scope < result[j].Scope })
	return result
}

func massCandidate(scope, subscription string, active []*models.ProxyConfig, failed []incidentNodeFailure) (massIncidentCandidate, bool) {
	if len(active) == 0 || len(failed) == 0 {
		return massIncidentCandidate{}, false
	}
	required := (len(active) + 1) / 2
	if required < 3 {
		required = 3
	}
	byCause := make(map[string][]incidentNodeFailure)
	for _, item := range failed {
		code := item.details.Failure.Code
		if code == "" {
			code = checker.FailureCodeUnknown
		}
		byCause[code] = append(byCause[code], item)
	}
	var causeCodes []string
	for code := range byCause {
		causeCodes = append(causeCodes, code)
	}
	sort.Strings(causeCodes)
	selectedCode := ""
	for _, code := range causeCodes {
		if len(byCause[code]) < required {
			continue
		}
		if selectedCode == "" || len(byCause[code]) > len(byCause[selectedCode]) {
			selectedCode = code
		}
	}
	if selectedCode == "" {
		return massIncidentCandidate{}, false
	}
	selected := byCause[selectedCode]
	sort.Slice(selected, func(i, j int) bool { return selected[i].proxy.StableID < selected[j].proxy.StableID })
	cause := selected[0].details.Failure
	if cause.Code == "" {
		cause.Code = selectedCode
	}
	if cause.Summary == "" {
		cause.Summary = checker.FailureSummary(cause.Code)
	}
	if scope == "global" && likelySharedCheckEndpoint(selectedCode, selected) {
		cause = checker.FailureDetails{
			Code:    checker.FailureCodeCheckEndpoint,
			Summary: "Вероятен общий сбой проверочного endpoint",
			Detail:  "одинаковая ошибка проверки при доступных TCP-портах разных нод",
		}
	}
	candidate := massIncidentCandidate{
		Scope:        scope,
		Subscription: subscription,
		TotalCount:   len(active),
		Cause:        cause,
	}
	for _, item := range selected {
		candidate.StableIDs = append(candidate.StableIDs, item.proxy.StableID)
		candidate.NodeNames = append(candidate.NodeNames, item.proxy.Name)
	}
	return candidate, true
}

func likelySharedCheckEndpoint(code string, failed []incidentNodeFailure) bool {
	switch code {
	case checker.FailureCodeDNS, checker.FailureCodeHTTPStatus, checker.FailureCodeProxyTimeout, checker.FailureCodeTLS:
	default:
		return false
	}
	servers := make(map[string]bool)
	for _, item := range failed {
		if !item.details.HostCheck.Checked || !item.details.HostCheck.Online {
			return false
		}
		servers[item.proxy.Server] = true
	}
	return len(servers) >= 2
}

func (s *Store) findActiveIncidentLocked(kind, scope string) int {
	for index := len(s.incidents) - 1; index >= 0; index-- {
		incident := s.incidents[index]
		if incident.Kind == kind && incident.Scope == scope && incident.Status == incidentStatusActive {
			return index
		}
	}
	return -1
}

func (s *Store) resolveIncidentLocked(index int, resolvedAt time.Time) {
	if index < 0 || index >= len(s.incidents) {
		return
	}
	incident := &s.incidents[index]
	incident.Status = incidentStatusResolved
	incident.ResolvedAt = resolvedAt
	incident.UpdatedAt = resolvedAt
	incident.DurationSec = durationSeconds(incident.StartedAt, resolvedAt)
}

func (s *Store) pruneIncidentsLocked() bool {
	if len(s.incidents) <= maxIncidentRecords {
		return false
	}
	active := make([]IncidentRecord, 0)
	resolved := make([]IncidentRecord, 0)
	for _, incident := range s.incidents {
		if incident.Status == incidentStatusActive {
			active = append(active, incident)
		} else {
			resolved = append(resolved, incident)
		}
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].StartedAt.After(resolved[j].StartedAt) })
	keepResolved := maxIncidentRecords - len(active)
	if keepResolved < 0 {
		keepResolved = 0
	}
	if len(resolved) > keepResolved {
		resolved = resolved[:keepResolved]
	}
	s.incidents = append(active, resolved...)
	return true
}

func normalizeIncidents(input []IncidentRecord) []IncidentRecord {
	result := make([]IncidentRecord, 0, len(input))
	seen := make(map[string]bool)
	for _, incident := range input {
		incident.ID = strings.TrimSpace(incident.ID)
		incident.Kind = strings.TrimSpace(incident.Kind)
		incident.Scope = strings.TrimSpace(incident.Scope)
		if incident.ID == "" || incident.Scope == "" || incident.StartedAt.IsZero() || seen[incident.ID] {
			continue
		}
		if incident.Kind != incidentKindNode && incident.Kind != incidentKindMass {
			continue
		}
		if incident.Status != incidentStatusActive && incident.Status != incidentStatusResolved {
			incident.Status = incidentStatusResolved
		}
		incident.StableIDs = uniqueSortedStrings(incident.StableIDs)
		incident.NodeNames = append([]string(nil), incident.NodeNames...)
		if incident.CauseCode == "" {
			incident.CauseCode = checker.FailureCodeUnknown
		}
		if incident.CauseSummary == "" {
			incident.CauseSummary = checker.FailureSummary(incident.CauseCode)
		}
		seen[incident.ID] = true
		result = append(result, incident)
	}
	if len(result) > maxIncidentRecords {
		result = result[len(result)-maxIncidentRecords:]
	}
	return result
}

func normalizeMergedNodes(input map[string][]string, nodes map[string]NodeRecord) map[string][]string {
	result := make(map[string][]string)
	for rawTarget, rawSources := range input {
		target := strings.TrimSpace(rawTarget)
		if target == "" {
			continue
		}
		if _, ok := nodes[target]; !ok {
			continue
		}
		sources := make([]string, 0, len(rawSources))
		for _, source := range rawSources {
			source = strings.TrimSpace(source)
			if source == "" || source == target {
				continue
			}
			sources = append(sources, source)
		}
		sources = uniqueSortedStrings(sources)
		if len(sources) > 0 {
			result[target] = sources
		}
	}
	return result
}

func copyMergedNodes(input map[string][]string) map[string][]string {
	result := make(map[string][]string, len(input))
	for target, sources := range input {
		result[target] = append([]string(nil), sources...)
	}
	return result
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
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

func incidentID(kind, scope string, startedAt time.Time) string {
	replacer := strings.NewReplacer(" ", "-", ":", "-", "/", "-")
	return fmt.Sprintf("%s-%d-%s", kind, startedAt.UnixNano(), replacer.Replace(scope))
}

func durationSeconds(startedAt, endedAt time.Time) int64 {
	if startedAt.IsZero() || endedAt.Before(startedAt) {
		return 0
	}
	return int64(endedAt.Sub(startedAt).Seconds())
}

func applySpeedStats(summary *Summary, entries []speedtest.Result) {
	for _, entry := range entries {
		summary.ResultCount++
		if !entry.CheckedAt.IsZero() && (summary.LastSpeedAt.IsZero() || entry.CheckedAt.After(summary.LastSpeedAt)) {
			summary.LastSpeedAt = entry.CheckedAt
			summary.LastMbps = entry.Mbps
			summary.LastSpeedError = entry.Error
			summary.LastSpeedOffline = entry.Offline
		}
		if entry.Error != "" || entry.Offline {
			summary.FailedResults++
			continue
		}
		if entry.Mbps <= 0 {
			continue
		}
		summary.SuccessfulResults++
		summary.AvgMbps += entry.Mbps
		if summary.MinMbps == 0 || entry.Mbps < summary.MinMbps {
			summary.MinMbps = entry.Mbps
		}
		if entry.Mbps > summary.MaxMbps {
			summary.MaxMbps = entry.Mbps
		}
	}
	if summary.SuccessfulResults > 0 {
		summary.AvgMbps /= float64(summary.SuccessfulResults)
	}
}

func normalizeRecordCountries(record *NodeRecord) {
	countryCode, country := detectClaimedCountry(record.Name)
	if countryCode == "" {
		countryCode, country = detectClaimedCountry(record.SubName)
	}
	record.ClaimedCountryCode = countryCode
	record.ClaimedCountry = country
	record.GeoCountryCode = strings.ToUpper(strings.TrimSpace(record.GeoCountryCode))
	if record.GeoCountryCode != "" && record.GeoCountry == "" {
		record.GeoCountry = countryName(record.GeoCountryCode)
	}
	record.IfconfigCountryCode = strings.ToUpper(strings.TrimSpace(record.IfconfigCountryCode))
	if record.IfconfigCountryCode != "" && record.IfconfigCountry == "" {
		record.IfconfigCountry = countryName(record.IfconfigCountryCode)
	}
}

func detectClaimedCountry(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if code := countryCodeFromFlag(value); code != "" {
		return code, countryName(code)
	}
	lower := strings.ToLower(value)
	for _, candidate := range countryCandidates {
		for _, alias := range candidate.Aliases {
			if strings.Contains(lower, alias) {
				return candidate.Code, countryName(candidate.Code)
			}
		}
		if containsCountryCodeToken(lower, candidate.Code) {
			return candidate.Code, countryName(candidate.Code)
		}
	}
	return "", ""
}

func containsCountryCodeToken(value string, code string) bool {
	code = strings.ToLower(code)
	tokens := strings.FieldsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for _, token := range tokens {
		if token == code {
			return true
		}
	}
	return false
}

func countryCodeFromFlag(value string) string {
	runes := []rune(value)
	for i := 0; i+1 < len(runes); i++ {
		first := runes[i]
		second := runes[i+1]
		if first >= 0x1F1E6 && first <= 0x1F1FF && second >= 0x1F1E6 && second <= 0x1F1FF {
			return string([]rune{'A' + first - 0x1F1E6, 'A' + second - 0x1F1E6})
		}
	}
	return ""
}

func countryMatch(record NodeRecord) string {
	claimed := strings.ToUpper(strings.TrimSpace(record.ClaimedCountryCode))
	if claimed == "" {
		return "unknown"
	}

	codes := geoCountryCodes(record)
	if len(codes) == 0 {
		return "unknown"
	}
	unique := make(map[string]bool, len(codes))
	for _, code := range codes {
		unique[code] = true
	}
	if len(unique) > 1 {
		return "conflict"
	}

	if codes[0] == claimed {
		if len(codes) < 2 {
			return "partial"
		}
		return "match"
	}
	return "mismatch"
}

func ipInfoURL(server string) string {
	server = serverHost(server)
	if server == "" {
		return ""
	}
	return "https://ipinfo.io/" + url.PathEscape(server)
}

func geoCountryCodes(record NodeRecord) []string {
	result := make([]string, 0, 2)
	if code := strings.ToUpper(strings.TrimSpace(record.GeoCountryCode)); code != "" && strings.TrimSpace(record.GeoError) == "" {
		result = append(result, code)
	}
	if code := strings.ToUpper(strings.TrimSpace(record.IfconfigCountryCode)); code != "" && strings.TrimSpace(record.IfconfigError) == "" {
		result = append(result, code)
	}
	return result
}

func geoSources(record NodeRecord) []GeoSource {
	sources := make([]GeoSource, 0, 2)
	if record.GeoCountryCode != "" || record.GeoCountry != "" || record.GeoIP != "" || record.GeoOrg != "" || record.GeoError != "" {
		sources = append(sources, GeoSource{
			Source:      "ipinfo.io",
			IP:          record.GeoIP,
			Country:     record.GeoCountry,
			CountryCode: record.GeoCountryCode,
			Org:         record.GeoOrg,
			UpdatedAt:   record.GeoUpdatedAt,
			Error:       record.GeoError,
		})
	}
	if record.IfconfigCountryCode != "" || record.IfconfigCountry != "" || record.IfconfigIP != "" || record.IfconfigOrg != "" || record.IfconfigASN != "" || record.IfconfigError != "" {
		sources = append(sources, GeoSource{
			Source:      "ifconfig.net",
			IP:          record.IfconfigIP,
			Country:     record.IfconfigCountry,
			CountryCode: record.IfconfigCountryCode,
			Org:         record.IfconfigOrg,
			ASN:         record.IfconfigASN,
			UpdatedAt:   record.IfconfigUpdatedAt,
			Error:       record.IfconfigError,
		})
	}
	return sources
}

func geoBlacklistHits(sources []GeoSource) []GeoBlacklistHit {
	var hits []GeoBlacklistHit
	for _, source := range sources {
		if strings.TrimSpace(source.Error) != "" {
			continue
		}
		code := strings.ToUpper(strings.TrimSpace(source.CountryCode))
		entry, ok := geoBlacklistCountries[code]
		if !ok {
			continue
		}
		country := strings.TrimSpace(source.Country)
		if country == "" {
			country = entry
		}
		hits = append(hits, GeoBlacklistHit{
			Source:      source.Source,
			Country:     country,
			CountryCode: code,
		})
	}
	return hits
}

func normalizeGeoCountryCode(code string, country string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code != "" {
		return code
	}
	return countryCodeFromName(country)
}

func countryCodeFromName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for code, country := range countryNames {
		if strings.EqualFold(country, name) {
			return code
		}
	}
	return ""
}

func serverHost(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(server); err == nil {
		return host
	}
	return server
}

func countryName(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if name, ok := countryNames[code]; ok {
		return name
	}
	return code
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	state := StateFile{
		Version:     stateVersion,
		UpdatedAt:   time.Now(),
		Nodes:       s.nodes,
		MergedNodes: s.mergedNodes,
		Incidents:   s.incidents,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type countryCandidate struct {
	Code    string
	Aliases []string
}

var countryCandidates = []countryCandidate{
	{Code: "NL", Aliases: []string{"нидерланд", "netherlands", "holland", "amsterdam"}},
	{Code: "FI", Aliases: []string{"финлянд", "finland", "helsinki"}},
	{Code: "EE", Aliases: []string{"эстони", "estonia", "tallinn"}},
	{Code: "DE", Aliases: []string{"герман", "germany", "deutschland", "frankfurt"}},
	{Code: "US", Aliases: []string{"соединенные штаты", "сша", "united states", "usa", "los angeles", "new york"}},
	{Code: "FR", Aliases: []string{"франци", "france", "paris"}},
	{Code: "GB", Aliases: []string{"великобрит", "united kingdom", "london"}},
	{Code: "PL", Aliases: []string{"польш", "poland", "warsaw"}},
	{Code: "SE", Aliases: []string{"швеци", "sweden", "stockholm"}},
	{Code: "NO", Aliases: []string{"норвеги", "norway", "oslo"}},
	{Code: "LV", Aliases: []string{"латви", "latvia", "riga"}},
	{Code: "LT", Aliases: []string{"литв", "lithuania", "vilnius"}},
	{Code: "RU", Aliases: []string{"росси", "russia", "moscow"}},
}

var countryNames = map[string]string{
	"BY": "Belarus",
	"CN": "China",
	"CU": "Cuba",
	"DE": "Germany",
	"EE": "Estonia",
	"FI": "Finland",
	"FR": "France",
	"GB": "United Kingdom",
	"IR": "Iran",
	"KP": "North Korea",
	"LT": "Lithuania",
	"LV": "Latvia",
	"MM": "Myanmar",
	"NL": "Netherlands",
	"NO": "Norway",
	"PL": "Poland",
	"RU": "Russia",
	"SE": "Sweden",
	"SY": "Syria",
	"TM": "Turkmenistan",
	"US": "United States",
	"VE": "Venezuela",
}

var geoBlacklistCountries = map[string]string{
	"BY": "Belarus",
	"CN": "China",
	"CU": "Cuba",
	"IR": "Iran",
	"KP": "North Korea",
	"MM": "Myanmar",
	"RU": "Russia",
	"SY": "Syria",
	"TM": "Turkmenistan",
	"VE": "Venezuela",
}
