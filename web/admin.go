package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"xray-checker/backup"
	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/nodearchive"
	"xray-checker/nodemerge"
	"xray-checker/projectmaintenance"
	"xray-checker/remnawave"
	"xray-checker/speedtest"
	"xray-checker/telegram"
)

type AdminProxyInfo struct {
	StableID string `json:"stableId"`
	Name     string `json:"name"`
	SubName  string `json:"subName"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	// Transport is what tells two nodes on one server apart: a host published
	// over both TCP and xHTTP differs only by this and by the port.
	Transport          string `json:"transport,omitempty"`
	ProxyPort          int    `json:"proxyPort"`
	Online             bool   `json:"online"`
	Status             string `json:"status"`
	ProxyHealthy       bool   `json:"proxyHealthy"`
	Maintenance        bool   `json:"maintenance"`
	LatencyMs          int64  `json:"latencyMs"`
	DownSince          string `json:"downSince,omitempty"`
	DowntimeSec        int64  `json:"downtimeSec"`
	ProxyFailureSince  string `json:"proxyFailureSince,omitempty"`
	ProxyFailureSec    int64  `json:"proxyFailureSec"`
	HostCheckChecked   bool   `json:"hostCheckChecked"`
	HostCheckOnline    bool   `json:"hostCheckOnline"`
	HostCheckLatencyMs int64  `json:"hostCheckLatencyMs"`
	HostCheckTarget    string `json:"hostCheckTarget,omitempty"`
	HostCheckError     string `json:"hostCheckError,omitempty"`
	PingCheckChecked   bool   `json:"pingCheckChecked"`
	PingCheckOnline    bool   `json:"pingCheckOnline"`
	PingCheckLatencyMs int64  `json:"pingCheckLatencyMs"`
	PingCheckTarget    string `json:"pingCheckTarget,omitempty"`
	PingCheckError     string `json:"pingCheckError,omitempty"`
	FailureCode        string `json:"failureCode,omitempty"`
	FailureSummary     string `json:"failureSummary,omitempty"`
	FailureDetail      string `json:"failureDetail,omitempty"`
}

// AdminNodeTestURLRequest carries the per-node speed-test overrides. Each
// override is a pointer so an omitted field keeps its stored value: the admin
// UI edits one field at a time, and a plain zero would otherwise read as
// "clear it". An explicit empty string or zero is how an override is removed.
// `url` remains for callers that only ever set the Test URL.
type AdminNodeTestURLRequest struct {
	StableID              string   `json:"stableId"`
	URL                   string   `json:"url"`
	TestURL               *string  `json:"testUrl,omitempty"`
	MaxBytes              *int64   `json:"maxBytes,omitempty"`
	LowSpeedThresholdMbps *float64 `json:"lowSpeedThresholdMbps,omitempty"`
}

type AdminProxyCheckRequest struct {
	StableIDs []string `json:"stableIds"`
}

type AdminProxyCheckFunc func([]string) error

type AdminSubscriptionRefreshResult struct {
	Updated              bool     `json:"updated"`
	Count                int      `json:"count"`
	Added                int      `json:"added"`
	Removed              int      `json:"removed"`
	Changed              int      `json:"changed"`
	RemovedNames         []string `json:"removedNames,omitempty"`
	RequiresConfirmation bool     `json:"requiresConfirmation,omitempty"`
	ConfirmationToken    string   `json:"confirmationToken,omitempty"`
	Message              string   `json:"message,omitempty"`
}

type AdminSubscriptionRefreshRequest struct {
	Force             bool   `json:"force"`
	ConfirmationToken string `json:"confirmationToken,omitempty"`
}

type AdminSubscriptionRefreshFunc func(AdminSubscriptionRefreshRequest) (AdminSubscriptionRefreshResult, error)

type AdminProjectMaintenanceRequest struct {
	Enabled bool `json:"enabled"`
}

type AdminProjectMaintenanceFunc func(bool) (projectmaintenance.Snapshot, error)

type AdminNodesOverviewGeoRequest struct {
	StableIDs []string `json:"stableIds"`
}

type AdminNodesOverviewDeleteRequest struct {
	StableID string `json:"stableId"`
}

type AdminNodeMaintenanceRequest struct {
	StableID string `json:"stableId"`
	Enabled  bool   `json:"enabled"`
}

type AdminNodeMaintenanceFunc func(string, bool) (nodearchive.NodeRecord, error)

type AdminNodesOverviewMergeRequest struct {
	SourceStableID    string `json:"sourceStableId"`
	TargetStableID    string `json:"targetStableId"`
	ConfirmationToken string `json:"confirmationToken,omitempty"`
}

type AdminNodeMergeService interface {
	Preview(sourceStableID, targetStableID string) (nodemerge.Preview, error)
	Stage(sourceStableID, targetStableID, confirmationToken string) (nodemerge.StageResult, error)
}

type AdminBackupRestoreGuard func() (release func(), err error)

type AdminRemnawaveService interface {
	Snapshot() remnawave.Snapshot
	UpdateSettings(remnawave.Settings) (remnawave.Snapshot, error)
	SyncNow(context.Context) (remnawave.Snapshot, error)
	SuggestLocations() remnawave.LocationSuggestion
	AdoptAnnounceBase(string, bool) (remnawave.Snapshot, error)
}

type AdminRemnawaveAnnounceBaseRequest struct {
	ExternalSquadUUID string `json:"externalSquadUuid"`
	Release           bool   `json:"release,omitempty"`
}

func AdminHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin" && r.URL.Path != "/admin/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		if err := RenderAdmin(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

func AdminRemnawaveHandler(service AdminRemnawaveService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, service.Snapshot())
		case http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
			var settings remnawave.Settings
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&settings); err != nil {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				writeError(w, "JSON body must contain one object", http.StatusBadRequest)
				return
			}
			snapshot, err := service.UpdateSettings(settings)
			if err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, snapshot)
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// AdminRemnawaveSuggestLocationsHandler reports the groupings the panel's own host
// tags already describe. It only proposes: nothing is saved until the operator
// picks which tags are locations and submits them like any hand-made card.
func AdminRemnawaveSuggestLocationsHandler(service AdminRemnawaveService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, service.SuggestLocations())
	}
}

// AdminRemnawaveAnnounceBaseHandler records the announce text a squad currently
// holds as the operator-owned base, or forgets it. Capturing it takes an explicit
// action because the checker will not guess where operator text ends.
func AdminRemnawaveAnnounceBaseHandler(service AdminRemnawaveService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var request AdminRemnawaveAnnounceBaseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		snapshot, err := service.AdoptAnnounceBase(request.ExternalSquadUUID, request.Release)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, snapshot)
	}
}

func AdminRemnawaveSyncHandler(service AdminRemnawaveService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot, err := service.SyncNow(r.Context())
		if err != nil {
			writeError(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, snapshot)
	}
}

func AdminSubscriptionRefreshHandler(refresh AdminSubscriptionRefreshFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var request AdminSubscriptionRefreshRequest
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
		}
		result, err := refresh(request)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "already running") {
				status = http.StatusConflict
			}
			writeError(w, err.Error(), status)
			return
		}
		writeJSON(w, result)
	}
}

func AdminProjectMaintenanceHandler(snapshot func() projectmaintenance.Snapshot, update AdminProjectMaintenanceFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, snapshot())
		case http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			var request AdminProjectMaintenanceRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				writeError(w, "JSON body must contain one object", http.StatusBadRequest)
				return
			}
			result, err := update(request.Enabled)
			if err != nil {
				writeError(w, err.Error(), http.StatusConflict)
				return
			}
			writeJSON(w, result)
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func AdminBackupHandler(creator *backup.Creator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		tmp, err := os.CreateTemp("", "xray-checker-backup-*.zip")
		if err != nil {
			writeError(w, "Failed to prepare backup", http.StatusInternalServerError)
			return
		}
		tmpPath := tmp.Name()
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}()

		result, err := creator.Create(tmp)
		if err != nil {
			writeError(w, "Failed to create backup: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			writeError(w, "Failed to prepare backup download", http.StatusInternalServerError)
			return
		}
		info, err := tmp.Stat()
		if err != nil {
			writeError(w, "Failed to prepare backup download", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": result.Filename}))
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.Header().Set("Cache-Control", "no-store, private")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, err := io.Copy(w, tmp); err != nil {
			return
		}
	}
}

func AdminBackupRestoreHandler(restorer *backup.Restorer, guards ...AdminBackupRestoreGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-Xray-Checker-Action") != "restore-backup" {
			writeError(w, "Restore confirmation header is required", http.StatusForbidden)
			return
		}
		for _, guard := range guards {
			if guard == nil {
				continue
			}
			release, err := guard()
			if err != nil {
				writeError(w, err.Error(), http.StatusConflict)
				return
			}
			if release != nil {
				defer release()
			}
		}

		r.Body = http.MaxBytesReader(w, r.Body, backup.MaxArchiveSize+1024*1024)
		if err := r.ParseMultipartForm(32 * 1024 * 1024); err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				writeError(w, "Backup archive is too large", http.StatusRequestEntityTooLarge)
				return
			}
			writeError(w, "Invalid backup upload", http.StatusBadRequest)
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}

		file, _, err := r.FormFile("backup")
		if err != nil {
			writeError(w, "Backup archive is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		size, err := file.Seek(0, io.SeekEnd)
		if err != nil {
			writeError(w, "Failed to read backup archive", http.StatusBadRequest)
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			writeError(w, "Failed to read backup archive", http.StatusBadRequest)
			return
		}

		result, err := restorer.Stage(file, size)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, result)
	}
}

func AdminProxiesHandler(proxyChecker *checker.ProxyChecker, startPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		proxies := proxyChecker.GetProxies()
		result := make([]AdminProxyInfo, 0, len(proxies))
		for _, proxy := range proxies {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			details, err := proxyChecker.GetProxyStatusDetailsIncludingMaintenance(proxy.StableID)
			if err != nil {
				online, latency, _ := proxyChecker.GetProxyStatusByStableID(proxy.StableID)
				details.Online = online
				details.Latency = latency
			}
			result = append(result, adminProxyInfo(proxy, details, startPort, !proxyChecker.MonitoringEnabled(proxy.StableID)))
		}
		writeJSON(w, result)
	}
}

func AdminProxyCheckHandler(check AdminProxyCheckFunc, proxyChecker *checker.ProxyChecker, startPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req AdminProxyCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.StableIDs) == 0 {
			writeError(w, "Select at least one node", http.StatusBadRequest)
			return
		}
		if err := check(req.StableIDs); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}

		selected := make(map[string]bool, len(req.StableIDs))
		for _, stableID := range req.StableIDs {
			selected[strings.TrimSpace(stableID)] = true
		}
		result := make([]AdminProxyInfo, 0, len(selected))
		for _, proxy := range proxyChecker.GetProxies() {
			if proxy == nil {
				continue
			}
			stableID := proxy.StableID
			if stableID == "" {
				stableID = proxy.GenerateStableID()
			}
			if !selected[stableID] {
				continue
			}
			details, _ := proxyChecker.GetProxyStatusDetailsIncludingMaintenance(stableID)
			proxyCopy := *proxy
			proxyCopy.StableID = stableID
			result = append(result, adminProxyInfo(&proxyCopy, details, startPort, !proxyChecker.MonitoringEnabled(stableID)))
		}
		writeJSON(w, result)
	}
}

func AdminSpeedTestSnapshotHandler(manager *speedtest.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, manager.Snapshot())
	}
}

func AdminSpeedTestHistoryHandler(manager *speedtest.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stableID := strings.TrimSpace(r.URL.Query().Get("stableId"))
		if stableID == "" {
			writeError(w, "stableId is required", http.StatusBadRequest)
			return
		}
		from, err := parseHistoryTime(r.URL.Query().Get("from"), "from")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := parseHistoryTime(r.URL.Query().Get("to"), "to")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !from.IsZero() && !to.IsZero() && !from.Before(to) {
			writeError(w, "from must be before to", http.StatusBadRequest)
			return
		}

		history := manager.ResultHistory(stableID)
		if !from.IsZero() || !to.IsZero() {
			filtered := make([]speedtest.Result, 0, len(history))
			for _, result := range history {
				if result.CheckedAt.IsZero() {
					continue
				}
				if !from.IsZero() && result.CheckedAt.Before(from) {
					continue
				}
				if !to.IsZero() && !result.CheckedAt.Before(to) {
					continue
				}
				filtered = append(filtered, result)
			}
			history = filtered
		}
		writeJSON(w, history)
	}
}

func AdminAvailabilityHistoryHandler(store *nodearchive.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stableID := strings.TrimSpace(r.URL.Query().Get("stableId"))
		if stableID == "" {
			writeError(w, "stableId is required", http.StatusBadRequest)
			return
		}
		from, err := parseHistoryTime(r.URL.Query().Get("from"), "from")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := parseHistoryTime(r.URL.Query().Get("to"), "to")
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !from.IsZero() && !to.IsZero() && !from.Before(to) {
			writeError(w, "from must be before to", http.StatusBadRequest)
			return
		}
		writeJSON(w, store.AvailabilityHistory(stableID, from, to))
	}
}

func parseHistoryTime(raw, name string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339", name)
	}
	return parsed, nil
}

func AdminSpeedTestRunHandler(manager *speedtest.Manager, availabilityChecks ...AdminProxyCheckFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req speedtest.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.ProxyIDs) == 0 {
			writeError(w, "Select at least one node", http.StatusBadRequest)
			return
		}
		if len(availabilityChecks) > 0 && availabilityChecks[0] != nil {
			if err := availabilityChecks[0](req.ProxyIDs); err != nil {
				writeError(w, fmt.Sprintf("availability check failed: %v", err), http.StatusBadRequest)
				return
			}
		}
		req.OnlyOnline = true
		req.SkipOffline = true
		if err := manager.Run(req, "manual"); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "already running") {
				status = http.StatusConflict
			}
			writeError(w, err.Error(), status)
			return
		}
		writeJSON(w, manager.Snapshot())
	}
}

func AdminSpeedTestNodeURLHandler(manager *speedtest.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req AdminNodeTestURLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		settings := speedtest.NodeSettings{
			MaxBytes:              req.MaxBytes,
			LowSpeedThresholdMbps: req.LowSpeedThresholdMbps,
		}
		switch {
		case req.TestURL != nil:
			settings.TestURL = req.TestURL
		case req.MaxBytes == nil && req.LowSpeedThresholdMbps == nil:
			// A body with neither override is the original request shape, where
			// an absent `url` means "clear the Test URL".
			settings.TestURL = &req.URL
		case req.URL != "":
			settings.TestURL = &req.URL
		}
		if err := manager.UpdateNodeSettings(req.StableID, settings); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, manager.Snapshot())
	}
}

func AdminNodesOverviewHandler(store *nodearchive.Store, manager *speedtest.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, store.Summaries(manager.AllResultHistory()))
	}
}

func AdminNodeMaintenanceHandler(update AdminNodeMaintenanceFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req AdminNodeMaintenanceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		req.StableID = strings.TrimSpace(req.StableID)
		if req.StableID == "" {
			writeError(w, "stableId is required", http.StatusBadRequest)
			return
		}
		record, err := update(req.StableID, req.Enabled)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, record)
	}
}

func AdminIncidentsHandler(store *nodearchive.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limit := 200
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(w, "limit must be between 1 and 1000", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		writeJSON(w, store.Incidents(limit))
	}
}

func AdminNodesOverviewGeoHandler(store *nodearchive.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req AdminNodesOverviewGeoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		result, err := store.RefreshGeo(ctx, req.StableIDs)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, result)
	}
}

func AdminNodesOverviewMergePreviewHandler(service AdminNodeMergeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req AdminNodesOverviewMergeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		preview, err := service.Preview(req.SourceStableID, req.TargetStableID)
		if err != nil {
			writeError(w, err.Error(), nodeMergeErrorStatus(err))
			return
		}
		writeJSON(w, preview)
	}
}

func AdminNodesOverviewMergeHandler(service AdminNodeMergeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req AdminNodesOverviewMergeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		result, err := service.Stage(req.SourceStableID, req.TargetStableID, req.ConfirmationToken)
		if err != nil {
			writeError(w, err.Error(), nodeMergeErrorStatus(err))
			return
		}
		writeJSON(w, result)
	}
}

func nodeMergeErrorStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"already pending", "being applied", "candidate changed", "identity changed", "backup restore", "became active", "no longer active"} {
		if strings.Contains(message, fragment) {
			return http.StatusConflict
		}
	}
	return http.StatusBadRequest
}

func AdminNodesOverviewDeleteHandler(store *nodearchive.Store, manager *speedtest.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req AdminNodesOverviewDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		stableID := strings.TrimSpace(req.StableID)
		if stableID == "" {
			writeError(w, "stableId is required", http.StatusBadRequest)
			return
		}
		record, err := store.ArchivedRecord(stableID)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		mergedFrom := store.MergedFromStableIDs(stableID)
		availabilityHistory := store.ArchivedAvailabilityHistory(stableID)
		if err := store.DeleteArchived(stableID); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := manager.DeleteHistory(stableID); err != nil {
			if rollbackErr := store.RestoreArchivedState(record, availabilityHistory, mergedFrom...); rollbackErr != nil {
				err = errors.Join(err, errors.New("restore archived node: "+rollbackErr.Error()))
			}
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	}
}

func AdminScheduleHandler(manager *speedtest.Manager, archives ...*nodearchive.Store) http.HandlerFunc {
	var archive *nodearchive.Store
	if len(archives) > 0 {
		archive = archives[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, manager.Schedule())
		case http.MethodPut:
			var schedule speedtest.ScheduleConfig
			if err := json.NewDecoder(r.Body).Decode(&schedule); err != nil {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			if err := manager.UpdateSchedule(schedule); err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if archive != nil {
				retentionDays := manager.Schedule().HistoryRetentionDays
				if err := archive.SetAvailabilityHistoryRetentionDays(retentionDays); err != nil {
					writeError(w, "schedule was saved but availability history pruning failed: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			writeJSON(w, manager.Schedule())
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func AdminTelegramHandler(service *telegram.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, service.AdminConfig())
		case http.MethodPut:
			var cfg telegram.AdminConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				writeError(w, "Invalid JSON body", http.StatusBadRequest)
				return
			}
			if err := service.UpdateAdminConfig(cfg); err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, service.AdminConfig())
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func AdminTelegramTestHandler(service *telegram.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := service.SendTestMessage(); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "sent"})
	}
}

func adminProxyInfo(proxy *models.ProxyConfig, details checker.ProxyStatusDetails, startPort int, maintenance bool) AdminProxyInfo {
	downSince := ""
	downtimeSec := int64(0)
	if !details.DownSince.IsZero() {
		downSince = details.DownSince.Format(time.RFC3339)
		downtimeSec = int64(time.Since(details.DownSince).Seconds())
	}
	proxyFailureSince := ""
	proxyFailureSec := int64(0)
	if !details.ProxyFailureSince.IsZero() {
		proxyFailureSince = details.ProxyFailureSince.Format(time.RFC3339)
		proxyFailureSec = int64(time.Since(details.ProxyFailureSince).Seconds())
	}

	return AdminProxyInfo{
		StableID:           proxy.StableID,
		Name:               proxy.Name,
		SubName:            proxy.SubName,
		Server:             proxy.Server,
		Port:               proxy.Port,
		Protocol:           proxy.Protocol,
		Transport:          proxy.Type,
		ProxyPort:          startPort + proxy.Index,
		Online:             !details.IsOffline(),
		Status:             string(details.EffectiveStatus()),
		ProxyHealthy:       details.Online,
		Maintenance:        maintenance,
		LatencyMs:          details.Latency.Milliseconds(),
		DownSince:          downSince,
		DowntimeSec:        downtimeSec,
		ProxyFailureSince:  proxyFailureSince,
		ProxyFailureSec:    proxyFailureSec,
		HostCheckChecked:   details.HostCheck.Checked,
		HostCheckOnline:    details.HostCheck.Online,
		HostCheckLatencyMs: details.HostCheck.Latency.Milliseconds(),
		HostCheckTarget:    details.HostCheck.Target,
		HostCheckError:     details.HostCheck.Error,
		PingCheckChecked:   details.PingCheck.Checked,
		PingCheckOnline:    details.PingCheck.Online,
		PingCheckLatencyMs: details.PingCheck.Latency.Milliseconds(),
		PingCheckTarget:    details.PingCheck.Target,
		PingCheckError:     details.PingCheck.Error,
		FailureCode:        details.Failure.Code,
		FailureSummary:     details.Failure.Summary,
		FailureDetail:      details.Failure.Detail,
	}
}
