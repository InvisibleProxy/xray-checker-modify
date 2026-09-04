package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/backup"
	"xray-checker/checker"
	"xray-checker/config"
	"xray-checker/logger"
	"xray-checker/metrics"
	"xray-checker/models"
	"xray-checker/nodearchive"
	"xray-checker/nodemerge"
	"xray-checker/observation"
	"xray-checker/probeagent"
	"xray-checker/projectmaintenance"
	"xray-checker/reachability"
	remnawaveannounce "xray-checker/remnawave"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
	"xray-checker/subscription"
	"xray-checker/subsource"
	"xray-checker/telegram"
	"xray-checker/web"
	"xray-checker/xray"

	"github.com/go-co-op/gocron"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version   = "unknown"
	startTime = time.Now()
)

func main() {
	config.Parse(version)

	logLevel := logger.ParseLevel(config.CLIConfig.LogLevel)
	logger.SetLevel(logLevel)

	logger.Startup("Xray Checker %s", version)
	if logLevel == logger.LevelNone {
		logger.Startup("Log level: none (silent mode)")
	}
	if recovered, err := nodemerge.RecoverUnconfirmed("data"); err != nil {
		logger.Fatal("Failed to roll back unconfirmed node merge: %v", err)
	} else if recovered {
		logger.Warn("Recovered an incomplete node merge transaction from the previous startup")
	}
	if recovered, err := backup.RecoverUnconfirmedRestore("data"); err != nil {
		logger.Fatal("Failed to roll back unconfirmed backup restore: %v", err)
	} else if recovered {
		logger.Warn("Recovered an incomplete backup restore transaction from the previous startup")
	}
	restoreApplied := false
	mergeApplied := false
	if applied, err := backup.ApplyPendingRestore("data"); err != nil {
		logger.Warn("Failed to apply pending backup restore: %v", err)
	} else if applied {
		restoreApplied = true
		logger.Startup("Applied pending backup restore")
	}
	handleStateLoadError := func(owner string, loadErr error) {
		if loadErr == nil {
			return
		}
		if mergeApplied {
			if rollbackErr := nodemerge.RollbackApplied("data"); rollbackErr != nil {
				logger.Fatal("Failed to load node-merged %s state (%v) and rollback failed: %v", owner, loadErr, rollbackErr)
			}
			logger.Fatal("Node-merged %s state was rejected and rolled back; restart the application: %v", owner, loadErr)
		}
		if !restoreApplied {
			logger.Warn("Failed to load %s state: %v", owner, loadErr)
			return
		}
		if rollbackErr := backup.RollbackAppliedRestore("data"); rollbackErr != nil {
			logger.Fatal("Failed to load restored %s state (%v) and rollback failed: %v", owner, loadErr, rollbackErr)
		}
		logger.Fatal("Restored %s state was rejected and rolled back; restart the application: %v", owner, loadErr)
	}
	projectMaintenance := projectmaintenance.NewManager("data/project_state.json")
	handleStateLoadError("project maintenance", projectMaintenance.Load())

	if err := web.InitAssetLoader(config.CLIConfig.Web.CustomAssetsPath); err != nil {
		logger.Fatal("Failed to initialize custom assets: %v", err)
	}

	geoManager := xray.NewGeoFileManager("")
	if err := geoManager.EnsureGeoFiles(); err != nil {
		logger.Fatal("Failed to ensure geo files: %v", err)
	}

	// Sources added from the panel load before the first fetch, so they take
	// part in startup exactly like the ones the environment provides. A failure
	// here is not fatal: the environment sources alone must still be enough to
	// start, which is what every existing deployment relies on.
	subscriptionSources := subsource.NewStore("data/subscription_sources.json")
	if err := subscriptionSources.Load(); err != nil {
		logger.Warn("Failed to load subscription sources added from the panel: %v", err)
	}

	configFile := "xray_config.json"
	proxyConfigs, err := subscription.InitializeConfiguration(configFile, version, allSubscriptionFeeds(subscriptionSources))
	if err != nil {
		logger.Fatal("Error initializing configuration: %v", err)
	}

	logger.Info("Loaded %d proxy configurations", len(*proxyConfigs))
	if !restoreApplied {
		if applied, applyErr := nodemerge.ApplyPending("data", activeStableIDSet(*proxyConfigs)); applyErr != nil {
			logger.Warn("Failed to apply pending node merge: %v", applyErr)
		} else if applied {
			mergeApplied = true
			logger.Startup("Applied pending node merge")
		}
	}

	if config.CLIConfig.Web.Public {
		if name := subscription.GetSubscriptionName(); name != "" {
			logger.Info("Subscription name for public status page: %s", name)
		}
	} else {
		subNames := web.CollectSubscriptionNames(*proxyConfigs)
		if len(subNames) > 0 {
			logger.Info("Subscriptions: %s", strings.Join(subNames, ", "))
		}
	}

	if logLevel == logger.LevelDebug {
		logger.Debug("=== Parsed Proxy Configurations ===")
		for _, pc := range *proxyConfigs {
			logger.Debug("%s", pc.DebugString())
		}
	}

	xrayRunner := xray.NewRunner(configFile)
	if err := xrayRunner.Start(); err != nil {
		logger.Fatal("Error starting Xray: %v", err)
	}

	defer func() {
		if err := xrayRunner.Stop(); err != nil {
			logger.Error("Error stopping Xray: %v", err)
		}
	}()

	metrics.InitMetrics(config.CLIConfig.Metrics.Instance)

	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.GetProxyStatusMetric())
	registry.MustRegister(metrics.GetProxyLatencyMetric())
	registry.MustRegister(metrics.GetProjectMaintenanceMetric())
	metrics.RecordProjectMaintenance(projectMaintenance.Enabled())
	backupCreator := backup.NewCreator("data", version)
	backupRestorer := backup.NewRestorer("data")
	probeAgentRegistry, err := probeagent.NewRegistry(probeagent.RegistryConfig{
		Path:                 config.CLIConfig.RemoteDiagnostics.RegistryPath,
		Enabled:              config.CLIConfig.RemoteDiagnostics.Enabled,
		DefaultControllerURL: config.CLIConfig.RemoteDiagnostics.ControllerURL,
		DefaultControllerIP:  config.CLIConfig.RemoteDiagnostics.ControllerIP,
		AgentImage:           config.CLIConfig.RemoteDiagnostics.AgentImage,
		EnrollmentTTL:        time.Duration(config.CLIConfig.RemoteDiagnostics.EnrollmentTTLMinutes) * time.Minute,
		HeartbeatMaxSkew:     time.Duration(config.CLIConfig.RemoteDiagnostics.HeartbeatMaxSkewSeconds) * time.Second,
		HeartbeatIntervalSec: config.CLIConfig.RemoteDiagnostics.HeartbeatIntervalSeconds,
	})
	if err != nil {
		logger.Fatal("Failed to configure diagnostic agent registry: %v", err)
	}
	if err := probeAgentRegistry.Load(); err != nil {
		if config.CLIConfig.RemoteDiagnostics.Enabled {
			logger.Fatal("Failed to load diagnostic agent registry: %v", err)
		}
		logger.Warn("Failed to load disabled diagnostic agent registry: %v", err)
	}

	proxyChecker := checker.NewProxyChecker(
		*proxyConfigs,
		config.CLIConfig.Xray.StartPort,
		config.CLIConfig.Proxy.IpCheckUrl,
		config.CLIConfig.Proxy.Timeout,
		config.CLIConfig.Proxy.StatusCheckUrl,
		config.CLIConfig.Proxy.DownloadUrl,
		config.CLIConfig.Proxy.DownloadTimeout,
		config.CLIConfig.Proxy.DownloadMinSize,
		config.CLIConfig.Proxy.CheckMethod,
	)
	proxyChecker.SetProjectMaintenance(projectMaintenance.Enabled())
	proxyChecker.SetSourcePolicies(sourceObservationPolicies(subscriptionSources))
	remoteDiagnosticController, err := remoteprobe.NewController(remoteprobe.Config{
		Enabled:     config.CLIConfig.RemoteDiagnostics.Enabled,
		CheckMethod: config.CLIConfig.Proxy.CheckMethod,
	}, probeAgentRegistry, proxyChecker)
	if err != nil {
		logger.Fatal("Failed to configure remote diagnostic jobs: %v", err)
	}
	diagnosticAutomation, err := agentautomation.New(agentautomation.Config{
		Enabled:       config.CLIConfig.RemoteDiagnostics.AutomationEnabled,
		Cooldown:      time.Duration(config.CLIConfig.RemoteDiagnostics.AutomationCooldownMinutes) * time.Minute,
		AlertWait:     time.Duration(config.CLIConfig.RemoteDiagnostics.AutomationAlertWaitSeconds) * time.Second,
		MaxConcurrent: config.CLIConfig.RemoteDiagnostics.AutomationMaxConcurrent,
	}, remoteDiagnosticController, probeAgentRegistry)
	if err != nil {
		logger.Fatal("Failed to configure diagnostic automation: %v", err)
	}
	reachabilityMatrix := reachability.NewMatrix("data/reachability.json")
	handleStateLoadError("reachability matrix", reachabilityMatrix.Load())
	reachabilitySweeper, err := reachability.NewSweeper(reachability.Config{
		Enabled:      config.CLIConfig.RemoteDiagnostics.ReachabilityEnabled,
		Interval:     time.Duration(config.CLIConfig.RemoteDiagnostics.ReachabilityIntervalMin) * time.Minute,
		ProbeTimeout: time.Duration(config.CLIConfig.RemoteDiagnostics.ReachabilityTimeoutSeconds) * time.Second,
		ProfileID:    config.CLIConfig.RemoteDiagnostics.ReachabilityProfile,
		OnSweep: func(summary reachability.Summary) {
			if summary.SaveError != nil {
				logger.Warn("Failed to persist the reachability matrix: %v", summary.SaveError)
			}
			if summary.Skipped {
				return
			}
			logger.Info("Reachability sweep: %d agents, %d nodes, %d cells, %d confirmed divergences, %d timeouts, %d errors",
				summary.Agents, summary.Nodes, summary.Recorded, summary.Confirmed, summary.Timeouts, summary.Errors)
		},
	}, remoteDiagnosticController, probeAgentRegistry, func() []reachability.Target {
		return reachabilityTargets(proxyChecker)
	}, reachabilityMatrix)
	if err != nil {
		logger.Fatal("Failed to configure the reachability sweep: %v", err)
	}

	xrayLifecycle := &sync.RWMutex{}
	proxyChecker.SetRunGate(xrayLifecycle.RLocker())
	speedTestManager := speedtest.NewManager(
		proxyChecker,
		config.CLIConfig.Xray.StartPort,
		"data/speedtest_schedule.json",
		speedtest.TestConfig{URL: config.CLIConfig.SpeedTest.URL},
	)
	speedTestManager.SetProjectMaintenance(projectMaintenance.Enabled())
	speedTestManager.ConfigureCountryFallbacks(
		"data/country-test-urls.yaml",
		"data/speedtest_url_health.json",
	)
	if err := speedTestManager.LoadCountryFallbacks(); err != nil {
		logger.Warn("Failed to load country Test URL fallbacks; continuing without invalid state: %v", err)
	}
	speedTestManager.SetRunGate(xrayLifecycle.RLocker())
	handleStateLoadError("speed-test", speedTestManager.Load())

	nodeArchive := nodearchive.NewStore("data/node_registry.json", proxyChecker)
	if err := nodeArchive.SetAvailabilityHistoryRetentionDays(speedTestManager.Schedule().HistoryRetentionDays); err != nil {
		logger.Fatal("Failed to configure availability history retention: %v", err)
	}
	handleStateLoadError("node registry", nodeArchive.Load())
	if err := nodeArchive.SyncProxies(*proxyConfigs); err != nil {
		logger.Warn("Failed to sync node registry: %v", err)
	}
	proxyChecker.ReplaceMaintenanceModes(nodeArchive.ActiveMaintenanceStableIDs())
	proxyChecker.ApplyDisplayNames(nodeArchive.DisplayNames())
	if err := nodeArchive.SyncSpeedHistory(speedTestManager.AllResultHistory()); err != nil {
		logger.Warn("Failed to sync speed history into node registry: %v", err)
	}
	speedTestManager.SetCountryResolver(nodeArchive.ClaimedCountryCode)
	nodeMergeCoordinator := nodemerge.NewCoordinator("data", nodeArchive, speedTestManager)

	var remnawaveAPI remnawaveannounce.API
	if config.CLIConfig.Remnawave.Enabled {
		client, clientErr := remnawaveannounce.NewHTTPClient(
			config.CLIConfig.Remnawave.APIURL,
			config.CLIConfig.Remnawave.APIToken,
			time.Duration(config.CLIConfig.Remnawave.TimeoutSeconds)*time.Second,
		)
		if clientErr != nil {
			logger.Fatal("Failed to configure Remnawave announce integration: %v", clientErr)
		}
		remnawaveAPI = client
	}
	remnawaveService := remnawaveannounce.NewService(remnawaveannounce.Options{
		MasterEnabled:      config.CLIConfig.Remnawave.Enabled,
		APIURL:             config.CLIConfig.Remnawave.APIURL,
		APITokenConfigured: strings.TrimSpace(config.CLIConfig.Remnawave.APIToken) != "",
		ConfigPath:         "data/remnawave_announce_config.json",
		RuntimePath:        "data/remnawave_announce_state.json",
		API:                remnawaveAPI,
		ProxySource:        proxyChecker,
		IncidentSource:     nodeArchive,
		RequestTimeout:     time.Duration(config.CLIConfig.Remnawave.TimeoutSeconds) * time.Second,
		ReconcileInterval:  time.Duration(config.CLIConfig.Remnawave.ReconcileIntervalSeconds) * time.Second,
		TopologyInterval:   time.Duration(config.CLIConfig.Remnawave.TopologyIntervalSeconds) * time.Second,
	})
	remnawaveService.SetProjectMaintenance(projectMaintenance.Enabled())
	if err := remnawaveService.LoadConfig(); err != nil {
		if restoreApplied {
			handleStateLoadError("Remnawave announce config", err)
		} else {
			logger.Warn("Failed to load Remnawave announce config; using disabled defaults: %v", err)
		}
	}
	if err := remnawaveService.LoadRuntime(); err != nil {
		logger.Warn("Failed to load Remnawave announce ownership state; remote writes are disabled: %v", err)
	}

	telegramService := telegram.NewService(
		"data/telegram_config.json",
		proxyChecker,
		speedTestManager,
		config.CLIConfig.Xray.StartPort,
	)
	telegramService.SetProjectMaintenance(projectMaintenance.Enabled())
	telegramService.SetSpeedDiagnosticAutomation(diagnosticAutomation)
	handleStateLoadError("Telegram", telegramService.Load())
	if projectMaintenance.Enabled() {
		if err := telegramService.ClearAllMonitoringState(); err != nil {
			logger.Warn("Failed to clear Telegram monitoring state for project maintenance: %v", err)
		}
	}
	if mergeApplied {
		if err := nodemerge.ConfirmApplied("data"); err != nil {
			logger.Fatal("Failed to confirm applied node merge: %v", err)
		}
		logger.Startup("Confirmed applied node merge")
	} else if restoreApplied {
		if err := backup.ConfirmAppliedRestore("data"); err != nil {
			logger.Fatal("Failed to confirm applied backup restore: %v", err)
		}
		logger.Startup("Confirmed restored persisted state")
	}
	if projectMaintenance.Enabled() {
		proxyChecker.ClearProjectMonitoringState()
		speedTestManager.ClearProjectMaintenanceProbes()
		if err := nodeArchive.PauseProjectMonitoring(); err != nil {
			logger.Warn("Failed to close restored monitoring accounting for project maintenance: %v", err)
		}
	} else if err := nodeArchive.ReconcileAvailabilityState(); err != nil {
		logger.Warn("Failed to record restored node availability: %v", err)
	}
	remnawaveService.Start()
	defer remnawaveService.Stop()
	automaticBackups := backup.NewAutomaticScheduler(backupCreator, "data/backups")
	automaticBackups.Start()
	defer automaticBackups.Stop()

	notifyRecoveredNodes := func(stableIDs []string) {
		if len(stableIDs) == 0 {
			return
		}
		telegramService.NotifyNodeRecoveries(stableIDs)
	}
	// A failed check produced no fresh status, so archive, announce and recovery
	// notifications must be skipped rather than fed the previous iteration's data.
	runAvailabilityCheck := func(stableIDs []string, allowMaintenance bool) error {
		projectProbe := projectMaintenance.Enabled()
		var recovered []string
		var checkErr error
		var completed bool
		if len(stableIDs) == 0 {
			unavailableBefore := unavailableStableIDSet(proxyChecker)
			checkErr = proxyChecker.CheckAllProxies()
			completed = checkErr == nil
			if completed {
				recovered = recoveredStableIDs(proxyChecker, unavailableBefore)
			}
		} else {
			var report checker.AvailabilityCheckReport
			if allowMaintenance {
				report, checkErr = proxyChecker.CheckProxiesByStableIDsIncludingMaintenance(stableIDs)
			} else {
				report, checkErr = proxyChecker.CheckProxiesByStableIDs(stableIDs)
			}
			completed = checkErr == nil
			if completed {
				recovered = report.RecoveredStableIDs()
			}
		}
		projectProbe = projectProbe || projectMaintenance.Enabled()
		if completed && !projectProbe {
			if err := nodeArchive.RecordAvailability(); err != nil {
				logger.Warn("Failed to record node availability after manual check: %v", err)
			}
			remnawaveService.Trigger()
			notifyRecoveredNodes(recovered)
		}
		return checkErr
	}
	runManualAvailabilityCheck := func(stableIDs []string) error {
		return runAvailabilityCheck(stableIDs, false)
	}
	runAdminAvailabilityCheck := func(stableIDs []string) error {
		return runAvailabilityCheck(stableIDs, true)
	}
	telegramService.SetAvailabilityCheckFunc(runManualAvailabilityCheck)
	// Renaming touches nothing but what is read, so it applies at once and needs
	// none of the Xray lifecycle locking a maintenance change does.
	setNodeDisplayName := func(stableID string, name string) (nodearchive.NodeRecord, error) {
		record, err := nodeArchive.SetDisplayName(stableID, name)
		if err != nil {
			return nodearchive.NodeRecord{}, err
		}
		proxyChecker.ApplyDisplayNames(nodeArchive.DisplayNames())
		return record, nil
	}
	setNodeMaintenance := func(stableID string, enabled bool) (nodearchive.NodeRecord, error) {
		xrayLifecycle.Lock()
		defer xrayLifecycle.Unlock()
		if _, ok := proxyChecker.GetProxyByStableID(stableID); !ok {
			return nodearchive.NodeRecord{}, fmt.Errorf("proxy not found")
		}
		record, err := nodeArchive.SetMaintenance(stableID, enabled)
		if err != nil {
			return nodearchive.NodeRecord{}, err
		}
		if err := proxyChecker.SetMaintenanceMode(stableID, enabled); err != nil {
			return nodearchive.NodeRecord{}, err
		}
		speedTestManager.ClearMaintenanceProbe(stableID)
		if enabled {
			if err := telegramService.ClearMonitoringState(stableID); err != nil {
				logger.Warn("Failed to clear Telegram monitoring state for %s: %v", stableID, err)
			}
		}
		if err := nodeArchive.RecordAvailability(); err != nil {
			logger.Warn("Failed to reconcile node registry after maintenance change: %v", err)
		}
		remnawaveService.Trigger()
		return record, nil
	}
	setProjectMaintenance := func(enabled bool) (projectmaintenance.Snapshot, error) {
		xrayLifecycle.Lock()
		defer xrayLifecycle.Unlock()
		current := projectMaintenance.Snapshot()
		if current.Enabled == enabled {
			return current, nil
		}
		snapshot, err := projectMaintenance.Set(enabled)
		if err != nil {
			return current, err
		}
		proxyChecker.SetProjectMaintenance(enabled)
		speedTestManager.SetProjectMaintenance(enabled)
		telegramService.SetProjectMaintenance(enabled)
		remnawaveService.SetProjectMaintenance(enabled)
		metrics.RecordProjectMaintenance(enabled)
		proxyChecker.ClearProjectMonitoringState()
		speedTestManager.ClearProjectMaintenanceProbes()
		if enabled {
			if err := nodeArchive.PauseProjectMonitoring(); err != nil {
				logger.Warn("Failed to close monitoring accounting for project maintenance: %v", err)
			}
			if err := telegramService.ClearAllMonitoringState(); err != nil {
				logger.Warn("Failed to clear Telegram monitoring state for project maintenance: %v", err)
			}
			logger.Info("Project maintenance enabled")
		} else {
			logger.Info("Project maintenance disabled; monitoring will resume from fresh checks")
		}
		return snapshot, nil
	}

	speedTestManager.SetReporter(telegramService)
	telegramService.Start()

	speedTestManager.StartScheduler()
	defer speedTestManager.Stop()
	defer telegramService.Stop()

	runCheckIteration := func() {
		if projectMaintenance.Enabled() {
			return
		}
		logger.Info("Starting proxy check iteration")
		projectProbe := projectMaintenance.Enabled()
		unavailableBefore := unavailableStableIDSet(proxyChecker)
		if err := proxyChecker.CheckAllProxies(); err != nil {
			// A cancel from the panel stops every check in flight, this one
			// included. That is what was asked for, not a failure to report.
			if errors.Is(err, checker.ErrCheckCancelled) {
				logger.Info("Proxy check iteration cancelled")
			} else {
				logger.Warn("Proxy check iteration skipped: %v", err)
			}
			return
		}
		projectProbe = projectProbe || projectMaintenance.Enabled()
		if projectProbe {
			return
		}
		recovered := recoveredStableIDs(proxyChecker, unavailableBefore)
		if err := nodeArchive.RecordAvailability(); err != nil {
			logger.Warn("Failed to record node availability: %v", err)
		}
		remnawaveService.ObserveFullCheck()
		go func() {
			if !telegramService.NotifyNodeStatuses() {
				notifyRecoveredNodes(recovered)
			}
		}()

		if config.CLIConfig.Metrics.PushURL != "" {
			pushConfig, err := metrics.ParseURL(config.CLIConfig.Metrics.PushURL)
			if err != nil {
				logger.Error("Error parsing push URL: %v", err)
				return
			}

			if pushConfig != nil {
				if err := metrics.PushMetrics(pushConfig, registry); err != nil {
					logger.Error("Error pushing metrics: %v", err)
				}
			}
		}
	}

	if config.CLIConfig.RunOnce {
		runCheckIteration()
		logger.Info("Check completed")
		return
	}

	checkScheduler := gocron.NewScheduler(time.UTC)
	checkScheduler.Every(config.CLIConfig.Proxy.CheckInterval).Seconds().Do(func() {
		runCheckIteration()
	})
	checkScheduler.StartAsync()

	// The sweep returns immediately when it is disabled, so it needs no guard
	// here. It deliberately starts after the check scheduler: the local half of
	// every comparison is the checker's own last result, and sweeping before
	// the first pass has produced one only yields unknown cells.
	go reachabilitySweeper.Run(context.Background())

	if recoveryInterval := config.CLIConfig.Proxy.RecoveryInterval; recoveryInterval > 0 {
		recoveryStop := make(chan struct{})
		var recoveryWG sync.WaitGroup
		recoveryWG.Add(1)
		go func() {
			defer recoveryWG.Done()
			ticker := time.NewTicker(time.Duration(recoveryInterval) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if projectMaintenance.Enabled() {
						continue
					}
					report, err := proxyChecker.CheckUnavailableProxies()
					if len(report.Results) > 0 {
						if archiveErr := nodeArchive.RecordAvailability(); archiveErr != nil {
							logger.Warn("Failed to record node availability after recovery check: %v", archiveErr)
						}
					}
					recovered := report.RecoveredStableIDs()
					if len(recovered) > 0 {
						notifyRecoveredNodes(recovered)
						remnawaveService.Trigger()
					}
					if errors.Is(err, checker.ErrCheckCancelled) {
						logger.Info("Fast recovery check cancelled")
					} else if err != nil {
						logger.Warn("Fast recovery check failed: %v", err)
					}
				case <-recoveryStop:
					return
				}
			}
		}()
		defer func() {
			close(recoveryStop)
			recoveryWG.Wait()
		}()
		logger.Startup("Fast recovery checks enabled every %d seconds", recoveryInterval)
	}

	refreshProgress := web.NewSubscriptionRefreshTracker()
	subscriptionRefreshLock := make(chan struct{}, 1)
	refreshSubscription := func(source string, force bool, confirmationToken string) (web.AdminSubscriptionRefreshResult, error) {
		if source != "manual" && projectMaintenance.Enabled() {
			return web.AdminSubscriptionRefreshResult{}, projectmaintenance.ErrEnabled
		}
		select {
		case subscriptionRefreshLock <- struct{}{}:
			defer func() { <-subscriptionRefreshLock }()
		default:
			return web.AdminSubscriptionRefreshResult{}, fmt.Errorf("subscription refresh already running")
		}

		logger.Info("Checking subscriptions for updates (%s)...", source)
		refreshProgress.Begin(source)
		refreshFailed := true
		defer func() { refreshProgress.Done(refreshFailed) }()

		refreshProgress.Phase(web.RefreshPhaseFetching)
		newConfigs, err := subscription.ReadFromFeeds(allSubscriptionFeeds(subscriptionSources))
		if err != nil {
			return web.AdminSubscriptionRefreshResult{}, fmt.Errorf("fetch subscriptions: %w", err)
		}

		if config.CLIConfig.Proxy.ResolveDomains {
			refreshProgress.Phase(web.RefreshPhaseResolving)
			resolved, err := subscription.ResolveDomainsForConfigs(newConfigs)
			if err != nil {
				logger.Error("Error resolving domains: %v", err)
			} else {
				newConfigs = resolved
			}
		}

		refreshProgress.Phase(web.RefreshPhaseComparing)
		if deduplicated, dropped := xray.DeduplicateByStableID(newConfigs); dropped > 0 {
			logger.Info("Dropped %d duplicate node(s) listed more than once by the subscription", dropped)
			newConfigs = deduplicated
		}

		if preserved := xray.PreserveStableIDs(*proxyConfigs, newConfigs); preserved > 0 {
			logger.Info("Preserved node statistics for %d refreshed proxies", preserved)
		}
		if err := xray.ValidateStableIDs(newConfigs); err != nil {
			return web.AdminSubscriptionRefreshResult{}, fmt.Errorf("reject subscription update: %w", err)
		}
		diff := xray.AnalyzeConfigDiff(*proxyConfigs, newConfigs)
		refreshResult := web.AdminSubscriptionRefreshResult{
			Count:        len(newConfigs),
			Added:        diff.Added,
			Removed:      diff.Removed,
			Changed:      diff.Changed,
			RemovedNames: diff.RemovedNames,
		}
		if diff.Suspicious() && !force {
			refreshResult.RequiresConfirmation = source == "manual"
			refreshResult.ConfirmationToken = xray.ConfigFingerprint(newConfigs)
			refreshResult.Message = fmt.Sprintf("Suspicious subscription update blocked: %d of %d nodes would be removed", diff.Removed, diff.Before)
			if source == "manual" {
				// Awaiting the operator's confirmation is an outcome, not a failure.
				refreshFailed = false
				return refreshResult, nil
			}
			return refreshResult, fmt.Errorf("%s", refreshResult.Message)
		}
		if diff.Suspicious() && source == "manual" && confirmationToken != xray.ConfigFingerprint(newConfigs) {
			return refreshResult, fmt.Errorf("subscription candidate changed since confirmation; review the update again")
		}

		if xray.IsConfigsEqual(*proxyConfigs, newConfigs) {
			logger.Info("Subscriptions checked, no changes")
			refreshResult.Message = "Subscriptions checked, no changes"
			refreshFailed = false
			return refreshResult, nil
		}

		refreshProgress.Phase(web.RefreshPhaseApplying)

		xrayLifecycle.Lock()
		if source != "manual" && projectMaintenance.Enabled() {
			xrayLifecycle.Unlock()
			return web.AdminSubscriptionRefreshResult{}, projectmaintenance.ErrEnabled
		}
		err = updateConfiguration(newConfigs, proxyConfigs, xrayRunner, proxyChecker)
		if err != nil {
			xrayLifecycle.Unlock()
			return web.AdminSubscriptionRefreshResult{}, err
		}
		if err := nodeArchive.SyncProxies(*proxyConfigs); err != nil {
			logger.Warn("Failed to sync node registry after subscription update: %v", err)
		}
		proxyChecker.ReplaceMaintenanceModes(nodeArchive.ActiveMaintenanceStableIDs())
		proxyChecker.ApplyDisplayNames(nodeArchive.DisplayNames())
		xrayLifecycle.Unlock()
		refreshProgress.Phase(web.RefreshPhaseFinishing)
		if err := telegramService.PruneInactiveMutedNodes(); err != nil {
			logger.Warn("Failed to prune inactive muted Telegram nodes: %v", err)
		}
		remnawaveService.Trigger()

		refreshResult.Updated = true
		refreshResult.Message = "Configuration updated"
		refreshFailed = false
		return refreshResult, nil
	}

	if config.CLIConfig.Subscription.Update {
		updateScheduler := gocron.NewScheduler(time.UTC)
		updateScheduler.Every(config.CLIConfig.Subscription.UpdateInterval).Seconds().WaitForSchedule().Do(func() {
			if projectMaintenance.Enabled() {
				return
			}
			if _, err := refreshSubscription("scheduled", false, ""); err != nil {
				logger.Error("Error updating subscriptions: %v", err)
			}
		})
		updateScheduler.StartAsync()
	}

	mux, err := web.NewPrefixServeMux(config.CLIConfig.Metrics.BasePath)
	if err != nil {
		logger.Fatal("Error creating web server: %v", err)
	}
	mux.Handle("/health", web.HealthHandler())
	mux.Handle("/static/", web.StaticHandler())
	mux.Handle("/api/v1/public/proxies", web.APIPublicProxiesHandler(proxyChecker))
	mux.Handle(probeagent.EnrollPath, web.ProbeAgentEnrollHandler(probeAgentRegistry, config.CLIConfig.RemoteDiagnostics.TrustedProxySecret))
	mux.Handle(probeagent.HeartbeatPath, web.ProbeAgentHeartbeatHandler(probeAgentRegistry, config.CLIConfig.RemoteDiagnostics.TrustedProxySecret))
	mux.Handle(probeagent.JobPollPath, web.ProbeAgentJobHandler(probeAgentRegistry, remoteDiagnosticController, config.CLIConfig.RemoteDiagnostics.TrustedProxySecret))
	mux.Handle(probeagent.ObservationPath, web.ProbeAgentObservationHandler(probeAgentRegistry, remoteDiagnosticController, config.CLIConfig.RemoteDiagnostics.TrustedProxySecret))

	web.RegisterConfigEndpoints(*proxyConfigs, proxyChecker, config.CLIConfig.Xray.StartPort)

	protectedHandler := http.NewServeMux()
	protectedHandler.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	protectedHandler.Handle("/config/", web.ConfigStatusHandler(proxyChecker, projectMaintenance))
	protectedHandler.Handle("/api/v1/proxies/", web.APIProxyHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/proxies", web.APIProxiesHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/config", web.APIConfigHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/status", web.APIStatusHandler(proxyChecker, projectMaintenance))
	protectedHandler.Handle("/api/v1/system/info", web.APISystemInfoHandler(version, startTime))
	protectedHandler.Handle("/api/v1/system/ip", web.APISystemIPHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/docs", web.APIDocsHandler())
	protectedHandler.Handle("/api/v1/openapi.yaml", web.APIOpenAPIHandler())
	protectedHandler.Handle("/admin", web.AdminHandler())
	protectedHandler.Handle("/admin/", web.AdminHandler())
	protectedHandler.Handle("/api/v1/admin/proxies", web.AdminProxiesHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/admin/proxies/check", web.AdminProxyCheckHandler(runAdminAvailabilityCheck, proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/admin/proxies/check/cancel", web.AdminProxyCheckCancelHandler(proxyChecker.CancelCheck))
	protectedHandler.Handle("/api/v1/admin/subscription/refresh/progress", web.AdminSubscriptionRefreshProgressHandler(refreshProgress))
	protectedHandler.Handle("/api/v1/admin/subscription/refresh", web.AdminSubscriptionRefreshHandler(func(request web.AdminSubscriptionRefreshRequest) (web.AdminSubscriptionRefreshResult, error) {
		return refreshSubscription("manual", request.Force, request.ConfirmationToken)
	}))
	protectedHandler.Handle("/api/v1/admin/subscription/sources", web.AdminSubscriptionSourcesHandler(
		subscriptionSources,
		config.CLIConfig.Subscription.URLs,
		func() { proxyChecker.SetSourcePolicies(sourceObservationPolicies(subscriptionSources)) },
	))
	protectedHandler.Handle("/api/v1/admin/project-maintenance", web.AdminProjectMaintenanceHandler(projectMaintenance.Snapshot, setProjectMaintenance))
	protectedHandler.Handle("/api/v1/admin/backup", web.AdminBackupHandler(backupCreator))
	protectedHandler.Handle("/api/v1/admin/backup/restore", web.AdminBackupRestoreHandler(backupRestorer, nodeMergeCoordinator.AcquireRestoreGuard))
	protectedHandler.Handle("/api/v1/admin/speed-tests/run", web.AdminSpeedTestRunHandler(speedTestManager, runAdminAvailabilityCheck))
	protectedHandler.Handle("/api/v1/admin/speed-tests/cancel", web.AdminSpeedTestCancelHandler(speedTestManager, proxyChecker.CancelCheck))
	protectedHandler.Handle("/api/v1/admin/speed-tests/node-url", web.AdminSpeedTestNodeURLHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests/history", web.AdminSpeedTestHistoryHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests", web.AdminSpeedTestSnapshotHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/availability/history", web.AdminAvailabilityHistoryHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/geo", web.AdminNodesOverviewGeoHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/merge/preview", web.AdminNodesOverviewMergePreviewHandler(nodeMergeCoordinator))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/merge", web.AdminNodesOverviewMergeHandler(nodeMergeCoordinator))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/delete", web.AdminNodesOverviewDeleteHandler(nodeArchive, speedTestManager))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/maintenance", web.AdminNodeMaintenanceHandler(setNodeMaintenance))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/name", web.AdminNodeDisplayNameHandler(setNodeDisplayName))
	protectedHandler.Handle("/api/v1/admin/nodes-overview", web.AdminNodesOverviewHandler(nodeArchive, speedTestManager))
	protectedHandler.Handle("/api/v1/admin/incidents", web.AdminIncidentsHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/schedules", web.AdminScheduleHandler(speedTestManager, nodeArchive))
	protectedHandler.Handle("/api/v1/admin/telegram/test", web.AdminTelegramTestHandler(telegramService))
	protectedHandler.Handle("/api/v1/admin/telegram", web.AdminTelegramHandler(telegramService))
	protectedHandler.Handle("/api/v1/admin/remnawave/announce-base", web.AdminRemnawaveAnnounceBaseHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/remnawave/locations/suggest", web.AdminRemnawaveSuggestLocationsHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/remnawave/sync", web.AdminRemnawaveSyncHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/remnawave", web.AdminRemnawaveHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents/reissue", web.AdminDiagnosticAgentReissueHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents/revoke", web.AdminDiagnosticAgentRevokeHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents/delete", web.AdminDiagnosticAgentDeleteHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents", web.AdminDiagnosticAgentsHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-sessions/cancel", web.AdminDiagnosticSessionCancelHandler(remoteDiagnosticController))
	protectedHandler.Handle("/api/v1/admin/diagnostic-sessions/delete", web.AdminDiagnosticSessionDeleteHandler(remoteDiagnosticController))
	protectedHandler.Handle("/api/v1/admin/diagnostic-sessions/clear", web.AdminDiagnosticSessionsClearHandler(remoteDiagnosticController))
	protectedHandler.Handle("/api/v1/admin/diagnostic-sessions/export", web.AdminDiagnosticSessionExportHandler(remoteDiagnosticController))
	protectedHandler.Handle("/api/v1/admin/diagnostic-sessions", web.AdminDiagnosticSessionsHandler(remoteDiagnosticController, diagnosticAutomation.Snapshot))
	protectedHandler.Handle("/api/v1/admin/reachability", web.AdminReachabilityHandler(reachabilitySweeper))

	if config.CLIConfig.Web.Public {
		mux.Handle("/", web.IndexHandler(version, proxyChecker, projectMaintenance))
		mux.Handle("/config/", web.ConfigStatusHandler(proxyChecker, projectMaintenance))
		middlewareHandler := web.BasicAuthMiddleware(
			config.CLIConfig.Metrics.Username,
			config.CLIConfig.Metrics.Password,
		)(protectedHandler)
		mux.Handle("/admin", middlewareHandler)
		mux.Handle("/admin/", middlewareHandler)
		mux.Handle("/metrics", middlewareHandler)
		mux.Handle("/api/", middlewareHandler)
	} else if config.CLIConfig.Metrics.Protected {
		protectedHandler.Handle("/", web.IndexHandler(version, proxyChecker, projectMaintenance))
		middlewareHandler := web.BasicAuthMiddleware(
			config.CLIConfig.Metrics.Username,
			config.CLIConfig.Metrics.Password,
		)(protectedHandler)
		mux.Handle("/", middlewareHandler)
	} else {
		protectedHandler.Handle("/", web.IndexHandler(version, proxyChecker, projectMaintenance))
		adminHandler := web.BasicAuthMiddleware(
			config.CLIConfig.Metrics.Username,
			config.CLIConfig.Metrics.Password,
		)(protectedHandler)
		mux.Handle("/admin", adminHandler)
		mux.Handle("/admin/", adminHandler)
		mux.Handle("/api/v1/admin/", adminHandler)
		mux.Handle("/", protectedHandler)
	}

	if !config.CLIConfig.RunOnce {
		logger.Info("Server listening on %s:%s%s",
			config.CLIConfig.Metrics.Host,
			config.CLIConfig.Metrics.Port,
			config.CLIConfig.Metrics.BasePath,
		)
		if err := http.ListenAndServe(config.CLIConfig.Metrics.Host+":"+config.CLIConfig.Metrics.Port, mux); err != nil {
			logger.Fatal("Error starting server: %v", err)
		}
	}
}

func updateConfiguration(newConfigs []*models.ProxyConfig, currentConfigs *[]*models.ProxyConfig,
	xrayRunner *xray.Runner, proxyChecker *checker.ProxyChecker) error {

	logger.Info("Subscription changed, updating configuration...")

	xray.PrepareProxyConfigs(newConfigs)

	configFile := "xray_config.json"
	configGenerator := xray.NewConfigGenerator()
	if err := xray.RestartWithConfigRollback(configFile, xrayRunner, func(candidateFile string) error {
		return configGenerator.GenerateAndSaveConfig(
			newConfigs,
			config.CLIConfig.Xray.StartPort,
			candidateFile,
			config.CLIConfig.Xray.LogLevel,
		)
	}); err != nil {
		return err
	}

	proxyChecker.UpdateProxies(newConfigs)

	*currentConfigs = newConfigs

	web.RegisterConfigEndpoints(newConfigs, proxyChecker, config.CLIConfig.Xray.StartPort)

	logger.Info("Configuration updated: %d proxies", len(newConfigs))
	return nil
}

// reachabilityTargets lists the nodes worth asking the agents about: the ones
// the checker is currently monitoring. A paused node is excluded on purpose —
// its local status is not being maintained, so an agent's answer would be
// compared against a result nobody is refreshing.
func reachabilityTargets(proxyChecker *checker.ProxyChecker) []reachability.Target {
	proxies := proxyChecker.GetProxies()
	targets := make([]reachability.Target, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		stableID := strings.TrimSpace(proxy.StableID)
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		if stableID == "" || !proxyChecker.MonitoringEnabled(stableID) {
			continue
		}
		targets = append(targets, reachability.Target{StableID: stableID, Name: proxy.Name})
	}
	return targets
}

func unavailableStableIDSet(proxyChecker *checker.ProxyChecker) map[string]bool {
	unavailable := make(map[string]bool)
	for _, proxy := range proxyChecker.GetProxies() {
		if proxy == nil {
			continue
		}
		stableID := proxy.StableID
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		details, err := proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err == nil && details.EffectiveStatus() != checker.AvailabilityStateOnline {
			unavailable[stableID] = true
		}
	}
	return unavailable
}

// allSubscriptionFeeds lists what to fetch: the environment's own subscriptions
// first, then the ones an operator added from the panel. Environment sources
// keep the checker's own client identity — response rules on the operator's
// panel are keyed on that User-Agent — while a panel-added source carries the
// client profile chosen for it.
func allSubscriptionFeeds(sources *subsource.Store) []subscription.Feed {
	feeds := subscription.FeedsFromURLs(config.CLIConfig.Subscription.URLs)
	for _, source := range sources.EnabledSources() {
		feeds = append(feeds, subscription.Feed{
			URL:      source.URL,
			Profile:  source.Profile,
			Name:     source.Name,
			SourceID: source.ID,
		})
	}
	return feeds
}

// sourceObservationPolicies is how a source's watching mode reaches the nodes
// it produced. It is reinstalled whenever the sources change, so a mode edit
// takes effect immediately instead of waiting for the next refresh.
func sourceObservationPolicies(sources *subsource.Store) map[string]observation.Policy {
	list := sources.List()
	policies := make(map[string]observation.Policy, len(list))
	for _, source := range list {
		if source.ID == "" {
			continue
		}
		policies[source.ID] = source.Policy()
	}
	return policies
}

func activeStableIDSet(proxies []*models.ProxyConfig) map[string]bool {
	active := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		stableID := strings.TrimSpace(proxy.StableID)
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		if stableID != "" {
			active[stableID] = true
		}
	}
	return active
}

func recoveredStableIDs(proxyChecker *checker.ProxyChecker, unavailableBefore map[string]bool) []string {
	if len(unavailableBefore) == 0 {
		return nil
	}
	recovered := make([]string, 0)
	for stableID := range unavailableBefore {
		details, err := proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err == nil && details.EffectiveStatus() == checker.AvailabilityStateOnline {
			recovered = append(recovered, stableID)
		}
	}
	return recovered
}
