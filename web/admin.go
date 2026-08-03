package web

import (
	"context"
	"encoding/json"
	"errors"
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
}

type AdminNodeTestURLRequest struct {
	StableID string `json:"stableId"`
	URL      string `json:"url"`
}

type AdminSubscriptionRefreshResult struct {
	Updated bool   `json:"updated"`
	Count   int    `json:"count"`
	Message string `json:"message,omitempty"`
}

type AdminSubscriptionRefreshFunc func() (AdminSubscriptionRefreshResult, error)

type AdminNodesOverviewGeoRequest struct {
	StableIDs []string `json:"stableIds"`
}

type AdminNodesOverviewDeleteRequest struct {
	StableID string `json:"stableId"`
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

func AdminSubscriptionRefreshHandler(refresh AdminSubscriptionRefreshFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := refresh()
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

func AdminBackupRestoreHandler(restorer *backup.Restorer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-Xray-Checker-Action") != "restore-backup" {
			writeError(w, "Restore confirmation header is required", http.StatusForbidden)
			return
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
		writeJSON(w, manager.ResultHistory(stableID))
	}
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
		if err := store.DeleteArchived(stableID); err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := manager.DeleteHistory(stableID); err != nil {
			if rollbackErr := store.RestoreArchived(record); rollbackErr != nil {
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
	}
}
