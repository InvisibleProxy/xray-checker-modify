package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
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

func AdminProxiesHandler(proxyChecker *checker.ProxyChecker, startPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, manager.Snapshot())
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
