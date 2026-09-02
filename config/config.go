package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/alecthomas/kong"
)

var CLIConfig CLI
var Version string

func Parse(version string) {
	Version = version
	ctx := kong.Parse(&CLIConfig,
		kong.Name("xray-checker"),
		kong.Description("Xray Checker: A Prometheus exporter for monitoring Xray proxies"),
		kong.Vars{
			"version": version,
		},
	)
	_ = ctx
}

type CLI struct {
	Subscription struct {
		URLs           []string `name:"subscription-url" help:"URL(s) of the subscription (can be specified multiple times)" required:"true" env:"SUBSCRIPTION_URL"`
		Update         bool     `name:"subscription-update" help:"Whether to recheck the subscription" default:"true" env:"SUBSCRIPTION_UPDATE"`
		UpdateInterval int      `name:"subscription-update-interval" help:"Interval for subscription updates in seconds" default:"300" env:"SUBSCRIPTION_UPDATE_INTERVAL"`
	} `embed:"" prefix:""`

	Proxy struct {
		CheckInterval    int    `name:"proxy-check-interval" help:"Interval for full proxy checks in seconds" default:"300" env:"PROXY_CHECK_INTERVAL"`
		RecoveryInterval int    `name:"proxy-recovery-interval" help:"Interval for TCP-gated recovery checks of unavailable proxies in seconds; 0 disables fast recovery checks" default:"15" env:"PROXY_RECOVERY_INTERVAL"`
		CheckMethod      string `name:"proxy-check-method" help:"Method for checking proxy, ip, status or download" default:"ip" env:"PROXY_CHECK_METHOD"`
		IpCheckUrl       string `name:"proxy-ip-check-url" help:"Service URL for IP checking" default:"https://api.ipify.org?format=text" env:"PROXY_IP_CHECK_URL"`
		StatusCheckUrl   string `name:"proxy-status-check-url" help:"Response status generator, used by check-method=status" default:"http://cp.cloudflare.com/generate_204" env:"PROXY_STATUS_CHECK_URL"`
		DownloadUrl      string `name:"proxy-download-url" help:"URL for file download checking, used by check-method=download" default:"https://proof.ovh.net/files/1Mb.dat" env:"PROXY_DOWNLOAD_URL"`
		DownloadTimeout  int    `name:"proxy-download-timeout" help:"Timeout for download checking in seconds" default:"60" env:"PROXY_DOWNLOAD_TIMEOUT"`
		DownloadMinSize  int64  `name:"proxy-download-min-size" help:"Minimum bytes to download for successful check" default:"51200" env:"PROXY_DOWNLOAD_MIN_SIZE"`
		Timeout          int    `name:"proxy-timeout" help:"Timeout for IP checking in seconds" default:"30" env:"PROXY_TIMEOUT"`
		SimulateLatency  bool   `name:"simulate-latency" help:"Whether to add latency to the response" default:"true" env:"SIMULATE_LATENCY"`
		ResolveDomains   bool   `name:"proxy-resolve-domains" help:"Resolve proxy server domains into IPs and expand configs" env:"PROXY_RESOLVE_DOMAINS"`
	} `embed:"" prefix:""`

	Xray struct {
		StartPort int    `name:"xray-start-port" help:"Start port for proxy configuration" default:"10000" env:"XRAY_START_PORT"`
		LogLevel  string `name:"xray-log-level" help:"Xray log level (debug|info|warning|error|none)" default:"none" env:"XRAY_LOG_LEVEL"`
	} `embed:"" prefix:""`

	Metrics struct {
		Host      string `name:"metrics-host" help:"Host to listen on" default:"0.0.0.0" env:"METRICS_HOST"`
		Port      string `name:"metrics-port" help:"Port to listen on" default:"2112" env:"METRICS_PORT"`
		Protected bool   `name:"metrics-protected" help:"Whether metrics are protected by basic auth" default:"false" env:"METRICS_PROTECTED"`
		Username  string `name:"metrics-username" help:"Username for admin endpoints and protected metrics" default:"" env:"METRICS_USERNAME"`
		Password  string `name:"metrics-password" help:"Password for admin endpoints and protected metrics" default:"" env:"METRICS_PASSWORD"`
		Instance  string `name:"metrics-instance" help:"Instance label for metrics" default:"" env:"METRICS_INSTANCE"`
		PushURL   string `name:"metrics-push-url" help:"Prometheus pushgateway URL (e.g. https://user:pass@host:port)" default:"" env:"METRICS_PUSH_URL"`
		BasePath  string `name:"metrics-base-path" help:"URL path to metrics (e.g. /xray/metrics)" default:"" env:"METRICS_BASE_PATH"`
	} `embed:"" prefix:""`

	Web struct {
		ShowServerDetails bool   `name:"web-show-details" help:"Show server IP addresses and ports in web UI" default:"false" env:"WEB_SHOW_DETAILS"`
		Public            bool   `name:"web-public" help:"Make dashboard public (requires --metrics-protected)" default:"false" env:"WEB_PUBLIC"`
		CustomAssetsPath  string `name:"web-custom-assets-path" help:"Path to custom assets directory (logo.svg, favicon.ico, custom.css, index.html)" default:"" env:"WEB_CUSTOM_ASSETS_PATH"`
	} `embed:"" prefix:""`

	SpeedTest struct {
		URL string `name:"speed-test-url" help:"Default URL for private speed tests" default:"https://proof.ovh.net/files/10Mb.dat" env:"SPEED_TEST_URL"`
	} `embed:"" prefix:""`

	Remnawave struct {
		Enabled                  bool   `name:"remnawave-announce-enabled" help:"Enable managed Remnawave subscription announce integration" default:"false" env:"REMNAWAVE_ANNOUNCE_ENABLED"`
		APIURL                   string `name:"remnawave-api-url" help:"Remnawave panel base URL (with or without /api)" default:"" env:"REMNAWAVE_API_URL"`
		APIToken                 string `name:"remnawave-api-token" help:"Remnawave API token; never persisted or exposed by the admin API" default:"" env:"REMNAWAVE_API_TOKEN"`
		TimeoutSeconds           int    `name:"remnawave-api-timeout" help:"Remnawave API request timeout in seconds" default:"10" env:"REMNAWAVE_API_TIMEOUT_SECONDS"`
		ReconcileIntervalSeconds int    `name:"remnawave-reconcile-interval" help:"Managed announce reconciliation interval in seconds" default:"60" env:"REMNAWAVE_RECONCILE_INTERVAL_SECONDS"`
		TopologyIntervalSeconds  int    `name:"remnawave-topology-interval" help:"Remnawave hosts and squads refresh interval in seconds" default:"300" env:"REMNAWAVE_TOPOLOGY_INTERVAL_SECONDS"`
	} `embed:"" prefix:""`

	RemoteDiagnostics struct {
		Enabled                    bool   `name:"remote-diagnostics-enabled" help:"Enable remote diagnostic probe-agent enrollment and control endpoints" default:"false" env:"REMOTE_DIAGNOSTICS_ENABLED"`
		AutomationEnabled          bool   `name:"remote-diagnostics-automation-enabled" help:"Automatically run isolated agent diagnostics after an unresolved speed-test fallback" default:"false" env:"REMOTE_DIAGNOSTICS_AUTOMATION_ENABLED"`
		AutomationCooldownMinutes  int    `name:"probe-automation-cooldown" help:"Cooldown per StableID after an automatic diagnostic session, in minutes" default:"30" env:"PROBE_AUTOMATION_COOLDOWN_MINUTES"`
		AutomationAlertWaitSeconds int    `name:"probe-automation-alert-wait" help:"Maximum time a background Telegram speed alert waits for agent evidence" default:"90" env:"PROBE_AUTOMATION_ALERT_WAIT_SECONDS"`
		AutomationMaxConcurrent    int    `name:"probe-automation-max-concurrent" help:"Maximum concurrent automatic diagnostic sessions" default:"2" env:"PROBE_AUTOMATION_MAX_CONCURRENT"`
		ReachabilityEnabled        bool   `name:"reachability-sweep-enabled" help:"Periodically ask every connected agent whether it can reach every node, and record the disagreements" default:"false" env:"REACHABILITY_SWEEP_ENABLED"`
		ReachabilityIntervalMin    int    `name:"reachability-sweep-interval" help:"Gap between the end of one reachability sweep and the start of the next, in minutes" default:"60" env:"REACHABILITY_SWEEP_INTERVAL_MINUTES"`
		ReachabilityTimeoutSeconds int    `name:"reachability-sweep-timeout" help:"How long one reachability probe may take before the sweep moves on, in seconds" default:"120" env:"REACHABILITY_SWEEP_TIMEOUT_SECONDS"`
		ReachabilityProfile        string `name:"reachability-sweep-profile" help:"Diagnostic profile the sweep runs; empty selects the status probe every agent supports" default:"" env:"REACHABILITY_SWEEP_PROFILE"`
		RegistryPath               string `name:"probe-agent-registry" help:"Path to the controller-bound probe-agent registry" default:"data/diagnostic_agents.json" env:"PROBE_AGENT_REGISTRY"`
		AgentImage                 string `name:"probe-agent-image" help:"Probe-agent image written into generated Compose files; pin an immutable digest in production" default:"ghcr.io/invisibleproxy/xray-checker-probe-agent:main" env:"PROBE_AGENT_IMAGE"`
		EnrollmentTTLMinutes       int    `name:"probe-enrollment-ttl" help:"One-time enrollment token lifetime in minutes" default:"15" env:"PROBE_ENROLLMENT_TTL_MINUTES"`
		HeartbeatIntervalSeconds   int    `name:"probe-heartbeat-interval" help:"Requested probe-agent heartbeat interval in seconds" default:"30" env:"PROBE_HEARTBEAT_INTERVAL_SECONDS"`
		HeartbeatMaxSkewSeconds    int    `name:"probe-heartbeat-max-skew" help:"Maximum accepted probe-agent heartbeat clock skew in seconds" default:"120" env:"PROBE_HEARTBEAT_MAX_SKEW_SECONDS"`
		TrustedProxySecret         string `name:"probe-trusted-proxy-secret" help:"Secret required before trusting the Caddy-provided probe-agent source IP header" default:"" env:"PROBE_TRUSTED_PROXY_SECRET"`
	} `embed:"" prefix:""`

	Version  VersionFlag `name:"version" help:"Print version information and quit"`
	RunOnce  bool        `name:"run-once" help:"Run one check cycle and exit" default:"false" env:"RUN_ONCE"`
	LogLevel string      `name:"log-level" help:"Log level (debug|info|warn|error|none)" default:"info" env:"LOG_LEVEL"`
}

func (c *CLI) Validate() error {
	if c.Web.Public && !c.Metrics.Protected {
		return fmt.Errorf("--web-public requires --metrics-protected to be enabled")
	}
	if !c.RunOnce && (strings.TrimSpace(c.Metrics.Username) == "" || strings.TrimSpace(c.Metrics.Password) == "") {
		return fmt.Errorf("--metrics-username and --metrics-password are required because /admin and /api/v1/admin/* are enabled")
	}
	if c.Remnawave.Enabled {
		apiURL := strings.TrimSpace(c.Remnawave.APIURL)
		parsed, err := url.Parse(apiURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("--remnawave-api-url must be a valid http or https URL when Remnawave announce is enabled")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("--remnawave-api-url must not contain credentials, query or fragment")
		}
		if strings.TrimSpace(c.Remnawave.APIToken) == "" {
			return fmt.Errorf("--remnawave-api-token is required when Remnawave announce is enabled")
		}
		if c.Remnawave.TimeoutSeconds < 1 || c.Remnawave.ReconcileIntervalSeconds < 10 || c.Remnawave.TopologyIntervalSeconds < 30 {
			return fmt.Errorf("Remnawave timeout must be positive, reconcile interval at least 10 seconds, and topology interval at least 30 seconds")
		}
	}
	if c.RemoteDiagnostics.AutomationEnabled && !c.RemoteDiagnostics.Enabled {
		return fmt.Errorf("remote diagnostic automation requires Remote Diagnostics to be enabled")
	}
	if c.RemoteDiagnostics.ReachabilityEnabled {
		if !c.RemoteDiagnostics.Enabled {
			return fmt.Errorf("the reachability sweep requires Remote Diagnostics to be enabled")
		}
		// The floors are not taste: a shorter interval turns the sweep into
		// continuous load on every agent and every node, and a shorter probe
		// timeout would abandon jobs that were about to answer.
		if c.RemoteDiagnostics.ReachabilityIntervalMin < 5 {
			return fmt.Errorf("--reachability-sweep-interval must be at least 5 minutes")
		}
		if c.RemoteDiagnostics.ReachabilityTimeoutSeconds < 15 {
			return fmt.Errorf("--reachability-sweep-timeout must be at least 15 seconds")
		}
	}
	if c.RemoteDiagnostics.Enabled {
		if strings.TrimSpace(c.RemoteDiagnostics.RegistryPath) == "" || strings.TrimSpace(c.RemoteDiagnostics.AgentImage) == "" {
			return fmt.Errorf("probe-agent registry path and image are required when remote diagnostics is enabled")
		}
		if c.RemoteDiagnostics.EnrollmentTTLMinutes < 1 || c.RemoteDiagnostics.HeartbeatIntervalSeconds < 5 || c.RemoteDiagnostics.HeartbeatMaxSkewSeconds < 1 {
			return fmt.Errorf("probe enrollment TTL must be positive, heartbeat interval at least 5 seconds, and heartbeat clock skew positive")
		}
		if secret := c.RemoteDiagnostics.TrustedProxySecret; secret != "" && len(secret) < 32 {
			return fmt.Errorf("probe trusted-proxy secret must contain at least 32 bytes when configured")
		}
		if c.RemoteDiagnostics.AutomationCooldownMinutes < 1 || c.RemoteDiagnostics.AutomationAlertWaitSeconds < 0 ||
			c.RemoteDiagnostics.AutomationAlertWaitSeconds > 300 || c.RemoteDiagnostics.AutomationMaxConcurrent < 1 ||
			c.RemoteDiagnostics.AutomationMaxConcurrent > 16 {
			return fmt.Errorf("probe automation cooldown must be positive, alert wait 0-300 seconds, and max concurrency 1-16")
		}
	}
	return nil
}

type VersionFlag string

func (v VersionFlag) Decode(ctx *kong.DecodeContext) error { return nil }
func (v VersionFlag) IsBool() bool                         { return true }
func (v VersionFlag) BeforeApply(app *kong.Kong, vars kong.Vars) error {
	fmt.Println("Xray Checker: A Prometheus exporter for monitoring Xray proxies")
	fmt.Printf("Version:\t %s\n", vars["version"])
	fmt.Printf("GitHub: https://github.com/kutovoys/xray-checker\n")
	app.Exit(0)
	return nil
}
