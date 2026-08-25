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
	"xray-checker/remnawave"
	"xray-checker/speedtest"
	"xray-checker/telegram"
)

type AdminProxyInfo struct {
	StableID           string `json:"stableId"`
	Name               string `json:"name"`
	SubName            string `json:"subName"`
	Server             string `json:"server"`
	Port               int    `json:"port"`
	Protocol           string `json:"protocol"`
	ProxyPort          int    `json:"proxyPort"`
	Online             bool   `json:"online"`
	LatencyMs          int64  `json:"latencyMs"`
	DownSince          string `json:"downSince,omitempty"`
	DowntimeSec        int64  `json:"downtimeSec"`
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

type AdminNodeTestURLRequest struct {
	StableID string `json:"stableId"`
	URL      string `json:"url"`
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

type AdminNodesOverviewGeoRequest struct {
	StableIDs []string `json:"stableIds"`
}

type AdminNodesOverviewDeleteRequest struct {
	StableID string `json:"stableId"`
}

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
			details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
			if err != nil {
				online, latency, _ := proxyChecker.GetProxyStatusByStableID(proxy.StableID)
				details.Online = online
				details.Latency = latency
			}
			result = append(result, adminProxyInfo(proxy, details, startPort))
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
			details, _ := proxyChecker.GetProxyStatusDetailsByStableID(stableID)
			proxyCopy := *proxy
			proxyCopy.StableID = stableID
			result = append(result, adminProxyInfo(&proxyCopy, details, startPort))
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

func AdminSpeedTestRunHandler(manager *speedtest.Manager) http.HandlerFunc {
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
		if err := manager.UpdateNodeTestURL(req.StableID, req.URL); err != nil {
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
		if err := store.DeleteArchived(stableID); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := manager.DeleteHistory(stableID); err != nil {
			if rollbackErr := store.RestoreArchived(record, mergedFrom...); rollbackErr != nil {
				err = errors.Join(err, errors.New("restore archived node: "+rollbackErr.Error()))
			}
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	}
}

func AdminScheduleHandler(manager *speedtest.Manager) http.HandlerFunc {
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

func adminProxyInfo(proxy *models.ProxyConfig, details checker.ProxyStatusDetails, startPort int) AdminProxyInfo {
	downSince := ""
	downtimeSec := int64(0)
	if !details.DownSince.IsZero() {
		downSince = details.DownSince.Format(time.RFC3339)
		downtimeSec = int64(time.Since(details.DownSince).Seconds())
	}

	return AdminProxyInfo{
		StableID:           proxy.StableID,
		Name:               proxy.Name,
		SubName:            proxy.SubName,
		Server:             proxy.Server,
		Port:               proxy.Port,
		Protocol:           proxy.Protocol,
		ProxyPort:          startPort + proxy.Index,
		Online:             details.Online,
		LatencyMs:          details.Latency.Milliseconds(),
		DownSince:          downSince,
		DowntimeSec:        downtimeSec,
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
