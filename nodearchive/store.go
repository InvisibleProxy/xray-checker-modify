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

type Store struct {
	path         string
	proxyChecker *checker.ProxyChecker
	httpClient   *http.Client

	mu    sync.RWMutex
	nodes map[string]NodeRecord
}

type StateFile struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updatedAt"`
	Nodes     map[string]NodeRecord `json:"nodes"`
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
	IPInfoURL         string      `json:"ipInfoUrl"`
	CountryMatch      string      `json:"countryMatch"`
	GeoSources        []GeoSource `json:"geoSources"`
	ResultCount       int         `json:"resultCount"`
	SuccessfulResults int         `json:"successfulResults"`
	FailedResults     int         `json:"failedResults"`
	AvgMbps           float64     `json:"avgMbps"`
	MinMbps           float64     `json:"minMbps"`
	MaxMbps           float64     `json:"maxMbps"`
	LastMbps          float64     `json:"lastMbps"`
	LastSpeedAt       time.Time   `json:"lastSpeedAt,omitempty"`
	LastSpeedError    string      `json:"lastSpeedError,omitempty"`
	LastSpeedOffline  bool        `json:"lastSpeedOffline"`
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
		nodes: make(map[string]NodeRecord),
	}
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

	s.mu.Lock()
	s.nodes = nodes
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
			record = applyAvailability(record, details, now)
		}
		if previous != record || s.nodes[proxy.StableID] != record {
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
		summary := Summary{NodeRecord: record}
		summary.IPInfoURL = ipInfoURL(record.Server)
		summary.GeoSources = geoSources(record)
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
		s.nodes[stableID] = record
	}
	err := s.saveLocked()
	s.mu.Unlock()
	return result, err
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
	delete(s.nodes, stableID)
	return s.saveLocked()
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
		Version:   stateVersion,
		UpdatedAt: time.Now(),
		Nodes:     s.nodes,
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
	"DE": "Germany",
	"EE": "Estonia",
	"FI": "Finland",
	"FR": "France",
	"GB": "United Kingdom",
	"IR": "Iran",
	"LT": "Lithuania",
	"LV": "Latvia",
	"NL": "Netherlands",
	"NO": "Norway",
	"PL": "Poland",
	"RU": "Russia",
	"SE": "Sweden",
	"US": "United States",
}
