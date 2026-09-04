package checker

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"xray-checker/metrics"
	"xray-checker/models"
	"xray-checker/projectmaintenance"
)

// checkProxyInternal records Prometheus gauges, which panic on an uninitialised
// registry. Only the tests that drive the real probe need them.
var initTestMetrics = sync.OnceFunc(func() { metrics.InitMetrics("") })

func TestGetProxyStatusByStableIDFallsBackToStatusDetails(t *testing.T) {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Name:     "NL",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "00000000-0000-0000-0000-000000000001",
	}
	proxy.StableID = proxy.GenerateStableID()

	proxyChecker := NewProxyChecker(
		[]*models.ProxyConfig{proxy},
		10000,
		"https://example.com/ip",
		30,
		"https://example.com/status",
		"https://example.com/file",
		60,
		51200,
		"ip",
	)
	proxyChecker.storeStatusDetails(proxy.StableID, true, 123*time.Millisecond, nil, nil)

	online, latency, err := proxyChecker.GetProxyStatusByStableID(proxy.StableID)
	if err != nil {
		t.Fatalf("expected status fallback, got error: %v", err)
	}
	if !online {
		t.Fatal("expected proxy to be online")
	}
	if latency != 123*time.Millisecond {
		t.Fatalf("expected latency %s, got %s", 123*time.Millisecond, latency)
	}
}

func TestProxyFailureIsStoredBeforeDiagnosticsComplete(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.storeStatusDetails(proxy.StableID, true, 10*time.Millisecond, nil, nil)

	diagnosticsStarted := make(chan struct{})
	releaseDiagnostics := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseDiagnostics)
		}
	}()
	diagnosticsAt := time.Now()
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		close(diagnosticsStarted)
		<-releaseDiagnostics
		return HostCheckDetails{Checked: true, Online: false, CheckedAt: diagnosticsAt},
			PingCheckDetails{Checked: true, Online: true, CheckedAt: diagnosticsAt}
	}

	done := make(chan struct{})
	go func() {
		proxyChecker.markUnavailableAndCollectDiagnostics(proxy)
		close(done)
	}()

	select {
	case <-diagnosticsStarted:
	case <-time.After(time.Second):
		t.Fatal("host diagnostics did not start")
	}
	details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Online || !details.IsProxyFailure() || details.ProxyFailureSince.IsZero() || !details.DownSince.IsZero() {
		t.Fatalf("proxy-failure transition was not stored before diagnostics: %+v", details)
	}
	proxyFailureSince := details.ProxyFailureSince
	if details.HostCheck.Checked || details.PingCheck.Checked {
		t.Fatalf("diagnostics unexpectedly completed before release: %+v", details)
	}

	close(releaseDiagnostics)
	released = true
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("host diagnostics did not complete")
	}
	details, err = proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil {
		t.Fatal(err)
	}
	if !details.IsProxyFailure() || !details.ProxyFailureSince.Equal(proxyFailureSince) || !details.DownSince.IsZero() {
		t.Fatalf("proxy failure was incorrectly converted to downtime: %+v", details)
	}
	if !details.HostCheck.Checked || !details.PingCheck.Checked || !details.PingCheck.Online {
		t.Fatalf("completed diagnostics were not stored: %+v", details)
	}
}

func TestProxyFailureRequiresTCPAndPingFailureBeforeOffline(t *testing.T) {
	tests := []struct {
		name       string
		hostOnline bool
		pingOnline bool
		wantStatus AvailabilityState
	}{
		{name: "both diagnostics pass", hostOnline: true, pingOnline: true, wantStatus: AvailabilityStateProxyFailure},
		{name: "only TCP passes", hostOnline: true, pingOnline: false, wantStatus: AvailabilityStateProxyFailure},
		{name: "only ping passes", hostOnline: false, pingOnline: true, wantStatus: AvailabilityStateProxyFailure},
		{name: "all checks fail", hostOnline: false, pingOnline: false, wantStatus: AvailabilityStateOffline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := testProxy("node-1", "Node one")
			proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
			proxyChecker.storeStatusDetails(proxy.StableID, true, 10*time.Millisecond, nil, nil)
			proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
				return HostCheckDetails{Checked: true, Online: tt.hostOnline}, PingCheckDetails{Checked: true, Online: tt.pingOnline}
			}

			proxyChecker.markUnavailableAndCollectDiagnostics(proxy, failureDetails(FailureCodeProxyTimeout, "timeout"))
			details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
			if err != nil {
				t.Fatal(err)
			}
			if details.EffectiveStatus() != tt.wantStatus {
				t.Fatalf("status = %s, want %s: %+v", details.EffectiveStatus(), tt.wantStatus, details)
			}
			if tt.wantStatus == AvailabilityStateOffline {
				if details.DownSince.IsZero() || !details.ProxyFailureSince.IsZero() {
					t.Fatalf("offline timestamps = %+v", details)
				}
			} else if details.ProxyFailureSince.IsZero() || !details.DownSince.IsZero() {
				t.Fatalf("proxy-failure timestamps = %+v", details)
			}
		})
	}
}

func TestOfflineDowntimeStartsAfterProxyFailureDiagnostics(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyFailureSince := time.Now().Add(-time.Hour)
	proxyChecker.statusDetails.Store(proxy.StableID, ProxyStatusDetails{
		Status:            AvailabilityStateProxyFailure,
		ProxyFailureSince: proxyFailureSince,
	})

	proxyChecker.storeOfflineDiagnostics(
		proxy.StableID,
		HostCheckDetails{Checked: true, Online: false},
		PingCheckDetails{Checked: true, Online: false},
	)
	details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil {
		t.Fatal(err)
	}
	if !details.IsOffline() || details.DownSince.IsZero() || !details.ProxyFailureSince.IsZero() {
		t.Fatalf("offline transition = %+v", details)
	}
	if details.DownSince.Sub(proxyFailureSince) < 59*time.Minute {
		t.Fatalf("downtime was backdated to proxy failure: proxy failure=%s downtime=%s", proxyFailureSince, details.DownSince)
	}
}

func TestMatchesPingReplyWhenKernelRewritesEchoID(t *testing.T) {
	probeData := []byte("xray-checker:probe")
	reply := &icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{
			ID:   54321,
			Seq:  123,
			Data: append([]byte(nil), probeData...),
		},
	}

	if !matchesPingReply(reply, ipv4.ICMPTypeEchoReply, 123, probeData) {
		t.Fatal("valid echo reply was rejected after the kernel rewrote its ID")
	}
	if matchesPingReply(reply, ipv4.ICMPTypeEchoReply, 124, probeData) {
		t.Fatal("echo reply with an unexpected sequence was accepted")
	}
	if matchesPingReply(reply, ipv4.ICMPTypeEchoReply, 123, []byte("other-probe")) {
		t.Fatal("echo reply with unexpected payload was accepted")
	}
	reply.Type = ipv4.ICMPTypeEcho
	if matchesPingReply(reply, ipv4.ICMPTypeEchoReply, 123, probeData) {
		t.Fatal("echo request was accepted as a reply")
	}
}

func TestPingIPLoopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("unprivileged ICMP datagram behavior is verified on Linux")
	}

	latency, err := pingIP(net.ParseIP("127.0.0.1"), time.Second)
	if err != nil {
		t.Fatalf("pingIP(loopback) error = %v", err)
	}
	if latency < 0 || latency >= time.Second {
		t.Fatalf("pingIP(loopback) latency = %s, want less than 1s", latency)
	}
}

func TestFastRecoverySkipsProxyCheckWhileTCPIsUnavailable(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	downSince := time.Now().Add(-time.Minute)
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, downSince, HostCheckDetails{}, PingCheckDetails{}) {
		t.Fatal("failed to seed offline status")
	}

	diagnosticsAt := time.Now()
	proxyChecks := 0
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		return HostCheckDetails{Checked: true, Online: false, CheckedAt: diagnosticsAt, Target: "node.example.com:443", Error: "timeout"},
			PingCheckDetails{Checked: true, Online: false, CheckedAt: diagnosticsAt, Target: "node.example.com", Error: "timeout"}
	}
	proxyChecker.checkProxyFunc = func(*models.ProxyConfig, uint64, bool, bool) {
		proxyChecks++
	}

	report, err := proxyChecker.CheckUnavailableProxies()
	if err != nil {
		t.Fatalf("CheckUnavailableProxies() error = %v", err)
	}
	if proxyChecks != 0 {
		t.Fatalf("proxy checks = %d, want 0 while TCP is unavailable", proxyChecks)
	}
	if len(report.Results) != 1 || report.Results[0].ProxyChecked || report.Results[0].Recovered {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil {
		t.Fatal(err)
	}
	if !details.HostCheck.Checked || details.HostCheck.Online || !details.HostCheck.CheckedAt.Equal(diagnosticsAt) {
		t.Fatalf("TCP diagnostics were not refreshed: %+v", details.HostCheck)
	}
	if !details.PingCheck.Checked || details.PingCheck.Online || !details.PingCheck.CheckedAt.Equal(diagnosticsAt) {
		t.Fatalf("ping diagnostics were not refreshed: %+v", details.PingCheck)
	}
}

func TestFastRecoveryChecksProxyImmediatelyWhenTCPReturns(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, time.Now().Add(-time.Minute), HostCheckDetails{}, PingCheckDetails{}) {
		t.Fatal("failed to seed offline status")
	}

	proxyChecks := 0
	quietProbe := false
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		return HostCheckDetails{Checked: true, Online: true, CheckedAt: time.Now()}, PingCheckDetails{Checked: true, Online: false, CheckedAt: time.Now()}
	}
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, quiet bool) {
		proxyChecks++
		quietProbe = quiet
		proxyChecker.storeStatusDetails(candidate.StableID, true, 25*time.Millisecond, nil, nil)
	}

	report, err := proxyChecker.CheckUnavailableProxies()
	if err != nil {
		t.Fatalf("CheckUnavailableProxies() error = %v", err)
	}
	if proxyChecks != 1 || !quietProbe {
		t.Fatalf("proxy checks = %d, quiet = %v; want one quiet recovery probe", proxyChecks, quietProbe)
	}
	if got := report.RecoveredStableIDs(); len(got) != 1 || got[0] != proxy.StableID {
		t.Fatalf("recovered IDs = %v, want [%s]", got, proxy.StableID)
	}
	if !report.Results[0].ProxyChecked || !report.Results[0].Online || !report.Results[0].Recovered {
		t.Fatalf("unexpected recovery result: %+v", report.Results[0])
	}
}

func TestManualAvailabilityCheckUsesFullProbeForOnlineNode(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.storeStatusDetails(proxy.StableID, true, 10*time.Millisecond, nil, nil)

	diagnosticsCalls := 0
	proxyChecks := 0
	quietProbe := true
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		diagnosticsCalls++
		return HostCheckDetails{}, PingCheckDetails{}
	}
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, quiet bool) {
		proxyChecks++
		quietProbe = quiet
		proxyChecker.storeStatusDetails(candidate.StableID, true, 12*time.Millisecond, nil, nil)
	}

	report, err := proxyChecker.CheckProxiesByStableIDs([]string{proxy.StableID})
	if err != nil {
		t.Fatalf("CheckProxiesByStableIDs() error = %v", err)
	}
	if diagnosticsCalls != 0 {
		t.Fatalf("diagnostic calls = %d, want 0 before a full check of an online node", diagnosticsCalls)
	}
	if proxyChecks != 1 || quietProbe {
		t.Fatalf("proxy checks = %d, quiet = %v; want one visible manual probe", proxyChecks, quietProbe)
	}
	if len(report.Results) != 1 || !report.Results[0].ProxyChecked || !report.Results[0].Online || report.Results[0].Recovered {
		t.Fatalf("unexpected manual check result: %+v", report)
	}
}

func TestFastRecoveryOnlyChecksUnavailableNodes(t *testing.T) {
	offline := testProxy("node-offline", "Offline")
	online := testProxy("node-online", "Online")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{offline, online})
	if !proxyChecker.RestoreOfflineStatus(offline.StableID, time.Now().Add(-time.Minute), HostCheckDetails{}, PingCheckDetails{}) {
		t.Fatal("failed to seed offline status")
	}
	proxyChecker.storeStatusDetails(online.StableID, true, 10*time.Millisecond, nil, nil)

	var diagnosticIDs []string
	proxyChecker.hostDiagnosticsFunc = func(proxy *models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		diagnosticIDs = append(diagnosticIDs, proxy.StableID)
		return HostCheckDetails{Checked: true, Online: false, CheckedAt: time.Now()}, PingCheckDetails{Checked: true, CheckedAt: time.Now()}
	}

	report, err := proxyChecker.CheckUnavailableProxies()
	if err != nil {
		t.Fatalf("CheckUnavailableProxies() error = %v", err)
	}
	if len(diagnosticIDs) != 1 || diagnosticIDs[0] != offline.StableID {
		t.Fatalf("diagnostic IDs = %v, want only %s", diagnosticIDs, offline.StableID)
	}
	if len(report.Results) != 1 || report.Results[0].StableID != offline.StableID {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestFullCheckNeverUsesTCPAsAGate(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, time.Now().Add(-time.Minute), HostCheckDetails{}, PingCheckDetails{}) {
		t.Fatal("failed to seed offline status")
	}

	diagnosticsCalls := 0
	proxyChecks := 0
	quietProbe := false
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		diagnosticsCalls++
		return HostCheckDetails{Checked: true, Online: false}, PingCheckDetails{Checked: true, Online: false}
	}
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, quiet bool) {
		proxyChecks++
		quietProbe = quiet
		proxyChecker.storeStatusDetails(candidate.StableID, true, 20*time.Millisecond, nil, nil)
	}

	proxyChecker.CheckAllProxies()

	if diagnosticsCalls != 0 {
		t.Fatalf("full check made %d preflight diagnostic calls, want 0", diagnosticsCalls)
	}
	if proxyChecks != 1 {
		t.Fatalf("full proxy checks = %d, want 1", proxyChecks)
	}
	if quietProbe {
		t.Fatal("full proxy check unexpectedly ran as a quiet recovery probe")
	}
	details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil || !details.Online {
		t.Fatalf("full check did not update the node online: details=%+v err=%v", details, err)
	}
}

func TestFullCheckReportsCurrentIPPreflightFailure(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := NewProxyChecker(
		[]*models.ProxyConfig{proxy},
		10000,
		"://invalid-current-ip-url",
		1,
		"",
		"",
		1,
		0,
		"ip",
	)
	proxyChecks := 0
	proxyChecker.checkProxyFunc = func(*models.ProxyConfig, uint64, bool, bool) {
		proxyChecks++
	}

	err := proxyChecker.CheckAllProxies()
	if err == nil {
		t.Fatal("full check did not report the current-IP preflight failure")
	}
	if proxyChecks != 0 {
		t.Fatalf("proxy checks = %d, want 0 after failed preflight", proxyChecks)
	}
}

func TestMaintenanceModeKeepsProbeChecksAndClearsMonitoringStatus(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.storeStatusDetails(proxy.StableID, false, 0, nil, nil)
	checks := 0
	proxyChecker.checkProxyFunc = func(*models.ProxyConfig, uint64, bool, bool) {
		checks++
	}

	if err := proxyChecker.SetMaintenanceMode(proxy.StableID, true); err != nil {
		t.Fatal(err)
	}
	if proxyChecker.MonitoringEnabled(proxy.StableID) {
		t.Fatal("monitoring remains enabled in maintenance mode")
	}
	proxyChecker.CheckAllProxies()
	if checks != 1 {
		t.Fatalf("maintenance full probes = %d, want 1", checks)
	}
	if _, err := proxyChecker.CheckProxiesByStableIDs([]string{proxy.StableID}); !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("non-admin maintenance check error = %v, want ErrMaintenanceMode", err)
	}
	if _, err := proxyChecker.CheckProxiesByStableIDsIncludingMaintenance([]string{proxy.StableID}); err != nil {
		t.Fatalf("admin maintenance check error = %v", err)
	}
	if checks != 2 {
		t.Fatalf("checks after admin maintenance probe = %d, want 2", checks)
	}
	if _, _, err := proxyChecker.GetProxyStatusByStableID(proxy.StableID); !errors.Is(err, ErrMaintenanceMode) {
		t.Fatalf("maintenance status error = %v, want ErrMaintenanceMode", err)
	}

	if err := proxyChecker.SetMaintenanceMode(proxy.StableID, false); err != nil {
		t.Fatal(err)
	}
	proxyChecker.CheckAllProxies()
	if checks != 3 {
		t.Fatalf("checks after resume = %d, want 3 including maintenance probes", checks)
	}
}

func TestProjectMaintenanceBlocksAutomaticChecksButAllowsAdminProbe(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	checks := 0
	proxyChecker.checkProxyFunc = func(*models.ProxyConfig, uint64, bool, bool) {
		checks++
	}
	proxyChecker.SetProjectMaintenance(true)

	if err := proxyChecker.CheckAllProxies(); !errors.Is(err, projectmaintenance.ErrEnabled) {
		t.Fatalf("CheckAllProxies() error = %v, want project maintenance", err)
	}
	if _, err := proxyChecker.CheckUnavailableProxies(); !errors.Is(err, projectmaintenance.ErrEnabled) {
		t.Fatalf("CheckUnavailableProxies() error = %v, want project maintenance", err)
	}
	if _, err := proxyChecker.CheckProxiesByStableIDs([]string{proxy.StableID}); !errors.Is(err, projectmaintenance.ErrEnabled) {
		t.Fatalf("non-admin check error = %v, want project maintenance", err)
	}
	if _, err := proxyChecker.CheckProxiesByStableIDsIncludingMaintenance([]string{proxy.StableID}); err != nil {
		t.Fatalf("admin project maintenance probe error = %v", err)
	}
	if checks != 1 {
		t.Fatalf("checks = %d, want only the explicit admin probe", checks)
	}
}

func TestAdminMixedMaintenanceBatchPreservesPerNodeRecoverySemantics(t *testing.T) {
	active := testProxy("active", "Active")
	paused := testProxy("paused", "Paused")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{active, paused})
	proxyChecker.storeStatusDetails(active.StableID, false, 0, nil, nil)
	if err := proxyChecker.SetMaintenanceMode(paused.StableID, true); err != nil {
		t.Fatal(err)
	}
	proxyChecker.storeStatusDetailsMode(paused.StableID, false, 0, nil, nil, nil, true)
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		return HostCheckDetails{Online: true}, PingCheckDetails{Online: true}
	}
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, _ bool) {
		proxyChecker.storeStatusDetailsMode(candidate.StableID, true, 10*time.Millisecond, nil, nil, nil, candidate.StableID == paused.StableID)
	}

	report, err := proxyChecker.CheckProxiesByStableIDsIncludingMaintenance([]string{active.StableID, paused.StableID})
	if err != nil {
		t.Fatalf("admin mixed check: %v", err)
	}
	if len(report.Results) != 2 || !report.Results[0].Recovered || report.Results[1].Recovered {
		t.Fatalf("mixed recovery report = %+v", report.Results)
	}
}

// An operator who stops a check gets a run that ends early, and the nodes it
// never reached keep the status they had.
func TestCancelStopsTheAvailabilityCheckEarly(t *testing.T) {
	proxies := make([]*models.ProxyConfig, 0, 12)
	stableIDs := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		proxy := testProxy(fmt.Sprintf("node-%d", index), fmt.Sprintf("Node %d", index))
		proxies = append(proxies, proxy)
		stableIDs = append(stableIDs, proxy.StableID)
	}
	proxyChecker := newTestProxyChecker(proxies)
	for _, proxy := range proxies {
		proxyChecker.storeStatusDetails(proxy.StableID, true, 10*time.Millisecond, nil, nil)
	}

	var mu sync.Mutex
	checked := 0
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, _ bool) {
		mu.Lock()
		checked++
		first := checked == 1
		mu.Unlock()
		if first {
			proxyChecker.CancelCheck()
		}
		proxyChecker.storeStatusDetails(candidate.StableID, true, 12*time.Millisecond, nil, nil)
	}

	_, err := proxyChecker.CheckProxiesByStableIDs(stableIDs)
	if !errors.Is(err, ErrCheckCancelled) {
		t.Fatalf("cancelled check error = %v, want ErrCheckCancelled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if checked == 0 || checked >= len(proxies) {
		// Workers already inside a probe finish it, so only "fewer than all" is
		// guaranteed here.
		t.Fatalf("checked %d of %d nodes, want the run to stop early", checked, len(proxies))
	}
	if proxyChecker.CheckRunning() {
		t.Fatal("the run context outlived the run")
	}
}

// Aborting a probe is not evidence that the node is down, so a cancelled check
// must not flip an online node to offline.
func TestCancelledProbeKeepsTheStatusItHad(t *testing.T) {
	initTestMetrics()

	// Nothing listens on the SOCKS port, so an ordinary probe fails. That is the
	// control: it proves the cancelled probe below is not passing for free.
	control := testProxy("node-1", "Node one")
	controlChecker := newTestProxyChecker([]*models.ProxyConfig{control})
	controlChecker.storeStatusDetails(control.StableID, true, 10*time.Millisecond, nil, nil)
	controlChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		return HostCheckDetails{Checked: true}, PingCheckDetails{Checked: true}
	}
	controlChecker.checkProxyInternal(control, 0, false, false, false)
	if details, ok := controlChecker.statusDetailsByStableID(control.StableID); !ok || details.Online {
		t.Fatalf("control probe should have marked the node offline, got %+v", details)
	}

	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.storeStatusDetails(proxy.StableID, true, 10*time.Millisecond, nil, nil)
	proxyChecker.hostDiagnosticsFunc = func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
		return HostCheckDetails{Checked: true}, PingCheckDetails{Checked: true}
	}

	handle := proxyChecker.registerRun()
	proxyChecker.setActiveRun(handle)
	handle.cancel()
	proxyChecker.checkProxyInternal(proxy, 0, false, false, false)
	proxyChecker.clearActiveRun(handle)
	proxyChecker.releaseRun(handle)

	details, ok := proxyChecker.statusDetailsByStableID(proxy.StableID)
	if !ok || !details.Online {
		t.Fatalf("cancelled probe rewrote the stored status: %+v", details)
	}
}

// Checks are serialised, so an operator who starts one while another is
// finishing is really waiting in a queue. Their cancel has to reach that queued
// run — otherwise it kills the run they cannot see and theirs starts anyway.
func TestCancelAlsoStopsAQueuedCheck(t *testing.T) {
	first := testProxy("node-1", "Node one")
	second := testProxy("node-2", "Node two")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{first, second})
	proxyChecker.storeStatusDetails(first.StableID, true, time.Millisecond, nil, nil)
	proxyChecker.storeStatusDetails(second.StableID, true, time.Millisecond, nil, nil)

	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	checked := make(map[string]bool)
	proxyChecker.checkProxyFunc = func(candidate *models.ProxyConfig, _ uint64, _ bool, _ bool) {
		mu.Lock()
		checked[candidate.StableID] = true
		mu.Unlock()
		if candidate.StableID == first.StableID {
			close(started)
			<-release
		}
	}

	go func() { _, _ = proxyChecker.CheckProxiesByStableIDs([]string{first.StableID}) }()
	<-started

	queued := make(chan error, 1)
	go func() {
		_, err := proxyChecker.CheckProxiesByStableIDs([]string{second.StableID})
		queued <- err
	}()
	waitForCondition(t, "the second check to queue", func() bool {
		proxyChecker.runsMu.Lock()
		defer proxyChecker.runsMu.Unlock()
		return len(proxyChecker.runs) == 2
	})

	if !proxyChecker.CancelCheck() {
		t.Fatal("cancel found nothing to stop")
	}
	close(release)

	select {
	case err := <-queued:
		if !errors.Is(err, ErrCheckCancelled) {
			t.Fatalf("queued check error = %v, want ErrCheckCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued check did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if checked[second.StableID] {
		t.Fatal("the queued check ran after it was cancelled")
	}
}

func waitForCondition(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCancelReportsWhetherAnythingWasRunning(t *testing.T) {
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{testProxy("node-1", "Node one")})
	if proxyChecker.CancelCheck() {
		t.Fatal("cancel reported a run while nothing was running")
	}
	if proxyChecker.CheckRunning() {
		t.Fatal("no run should be reported outside a check")
	}
}

func testProxy(stableID string, name string) *models.ProxyConfig {
	return &models.ProxyConfig{
		StableID: stableID,
		Protocol: "vless",
		Name:     name,
		Server:   "node.example.com",
		Port:     443,
		UUID:     stableID + "-uuid",
	}
}

func newTestProxyChecker(proxies []*models.ProxyConfig) *ProxyChecker {
	return NewProxyChecker(proxies, 10000, "", 1, "https://example.com/status", "", 1, 0, "status")
}
