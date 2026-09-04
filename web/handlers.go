package web

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"xray-checker/checker"
	"xray-checker/config"
	"xray-checker/metrics"
	"xray-checker/models"
	"xray-checker/projectmaintenance"
	"xray-checker/subscription"
)

var (
	registeredEndpoints []EndpointInfo
	endpointsMu         sync.RWMutex
)

type EndpointInfo struct {
	Name               string
	GroupName          string
	ServerInfo         string
	Server             string
	ServerPort         int
	URL                string
	ProxyPort          int
	Index              int
	Status             bool
	AvailabilityStatus string
	ProxyHealthy       bool
	Maintenance        bool
	Latency            time.Duration
	StableID           string
}

type ProjectMaintenanceSource interface {
	Snapshot() projectmaintenance.Snapshot
}

func projectMaintenanceSnapshot(sources []ProjectMaintenanceSource) projectmaintenance.Snapshot {
	if len(sources) == 0 || sources[0] == nil {
		return projectmaintenance.Snapshot{}
	}
	return sources[0].Snapshot()
}

func IndexHandler(version string, proxyChecker *checker.ProxyChecker, projectSources ...ProjectMaintenanceSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		RegisterConfigEndpoints(proxyChecker.GetProxies(), proxyChecker, config.CLIConfig.Xray.StartPort)

		endpointsMu.RLock()
		allEndpoints := make([]EndpointInfo, len(registeredEndpoints))
		copy(allEndpoints, registeredEndpoints)
		endpointsMu.RUnlock()

		isPublic := config.CLIConfig.Web.Public
		showServerDetails := config.CLIConfig.Web.ShowServerDetails
		if isPublic {
			showServerDetails = false
		}

		endpoints := allEndpoints
		if isPublic {
			endpoints = make([]EndpointInfo, len(allEndpoints))
			for i, ep := range allEndpoints {
				endpoints[i] = EndpointInfo{
					Name:               ep.Name,
					Index:              ep.Index,
					Status:             ep.Status,
					AvailabilityStatus: ep.AvailabilityStatus,
					ProxyHealthy:       ep.ProxyHealthy,
					Maintenance:        ep.Maintenance,
					Latency:            ep.Latency,
					StableID:           ep.StableID,
				}
			}
		}

		projectState := projectMaintenanceSnapshot(projectSources)
		data := PageData{
			Version:                    version,
			Host:                       config.CLIConfig.Metrics.Host,
			Port:                       config.CLIConfig.Metrics.Port,
			CheckInterval:              config.CLIConfig.Proxy.CheckInterval,
			IPCheckUrl:                 config.CLIConfig.Proxy.IpCheckUrl,
			CheckMethod:                config.CLIConfig.Proxy.CheckMethod,
			StatusCheckUrl:             config.CLIConfig.Proxy.StatusCheckUrl,
			DownloadUrl:                config.CLIConfig.Proxy.DownloadUrl,
			SimulateLatency:            config.CLIConfig.Proxy.SimulateLatency,
			Timeout:                    config.CLIConfig.Proxy.Timeout,
			SubscriptionUpdate:         config.CLIConfig.Subscription.Update,
			SubscriptionUpdateInterval: config.CLIConfig.Subscription.UpdateInterval,
			StartPort:                  config.CLIConfig.Xray.StartPort,
			Instance:                   config.CLIConfig.Metrics.Instance,
			PushUrl:                    metrics.GetPushURL(config.CLIConfig.Metrics.PushURL),
			Endpoints:                  endpoints,
			ShowServerDetails:          showServerDetails,
			IsPublic:                   isPublic,
			SubscriptionName:           subscription.GetSubscriptionName(),
			ProjectMaintenance:         projectState.Enabled,
			ProjectMaintenanceSince:    projectState.Since,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		if err := RenderIndex(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}

func BasicAuthMiddleware(username, password string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || user != username || pass != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
				http.Error(w, "Unauthorized.", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ConfigStatusHandler(proxyChecker *checker.ProxyChecker, projectSources ...ProjectMaintenanceSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/config/"):]
		if path == "" {
			http.Error(w, "Config path is required", http.StatusBadRequest)
			return
		}

		found, exists := proxyChecker.GetProxyByStableID(path)
		if !exists {
			http.Error(w, "Config not found", http.StatusNotFound)
			return
		}
		// An unlisted source is watched for the operator, not published, so its
		// nodes have no endpoint an external uptime check could bind to.
		if !proxyChecker.ListedPublicly(found.StableID) {
			http.Error(w, "Config not found", http.StatusNotFound)
			return
		}
		if projectMaintenanceSnapshot(projectSources).Enabled {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Xray-Checker-Status", "project-maintenance")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Project Maintenance"))
			return
		}

		if !proxyChecker.MonitoringEnabled(found.StableID) {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Xray-Checker-Status", "maintenance")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Maintenance"))
			return
		}
		details, err := proxyChecker.GetProxyStatusDetailsByStableID(found.StableID)
		if err != nil {
			http.Error(w, "Status not available", http.StatusNotFound)
			return
		}

		if config.CLIConfig.Proxy.SimulateLatency {
			time.Sleep(details.Latency)
		}

		if details.IsProxyFailure() {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Xray-Checker-Status", string(checker.AvailabilityStateProxyFailure))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Proxy failure"))
		} else if !details.IsOffline() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Failed"))
		}
	}
}

func RegisterConfigEndpoints(proxies []*models.ProxyConfig, proxyChecker *checker.ProxyChecker, startPort int) {
	endpoints := make([]EndpointInfo, 0, len(proxies))

	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		// The status page and the /config endpoints are what this deployment
		// publishes as its own service. A source the operator marked unlisted
		// is not that, so its nodes reach neither.
		if !proxyChecker.ListedPublicly(proxy.StableID) {
			continue
		}

		endpoint := fmt.Sprintf("./config/%s", proxy.StableID)

		details, _ := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)

		endpoints = append(endpoints, EndpointInfo{
			Name:               proxyChecker.Label(proxy),
			GroupName:          proxy.GroupName,
			ServerInfo:         fmt.Sprintf("%s:%d", proxy.Server, proxy.Port),
			Server:             proxy.Server,
			ServerPort:         proxy.Port,
			URL:                endpoint,
			ProxyPort:          startPort + proxy.Index,
			Index:              proxy.Index,
			Status:             !details.IsOffline(),
			AvailabilityStatus: string(details.EffectiveStatus()),
			ProxyHealthy:       details.Online,
			Maintenance:        !proxyChecker.MonitoringEnabled(proxy.StableID),
			Latency:            details.Latency,
			StableID:           proxy.StableID,
		})
	}

	endpointsMu.Lock()
	registeredEndpoints = endpoints
	endpointsMu.Unlock()
}

type PrefixServeMux struct {
	prefix string
	mux    *http.ServeMux
}

func NewPrefixServeMux(prefix string) (*PrefixServeMux, error) {
	if strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("served url path prefix '%s' should not ends with a '/'", prefix)
	}
	return &PrefixServeMux{
		prefix: prefix,
		mux:    http.NewServeMux(),
	}, nil
}

func (pm *PrefixServeMux) Handle(pattern string, handler http.Handler) {
	pm.mux.Handle(pm.prefix+pattern, http.StripPrefix(pm.prefix, handler))
}

func (pm *PrefixServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == pm.prefix || strings.HasPrefix(r.URL.Path, pm.prefix+"/") {
		pm.mux.ServeHTTP(w, r)
	} else {
		http.NotFound(w, r)
	}
}
