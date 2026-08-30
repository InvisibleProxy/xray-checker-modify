package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"xray-checker/backup"
	"xray-checker/checker"
	"xray-checker/config"
	"xray-checker/logger"
	"xray-checker/metrics"
	"xray-checker/models"
	"xray-checker/nodearchive"
	"xray-checker/nodemerge"
	"xray-checker/probeagent"
	remnawaveannounce "xray-checker/remnawave"
	"xray-checker/speedtest"
	"xray-checker/subscription"
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

	if err := web.InitAssetLoader(config.CLIConfig.Web.CustomAssetsPath); err != nil {
		logger.Fatal("Failed to initialize custom assets: %v", err)
	}

	geoManager := xray.NewGeoFileManager("")
	if err := geoManager.EnsureGeoFiles(); err != nil {
		logger.Fatal("Failed to ensure geo files: %v", err)
	}

	configFile := "xray_config.json"
	proxyConfigs, err := subscription.InitializeConfiguration(configFile, version)
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
	backupCreator := backup.NewCreator("data", version)
	backupRestorer := backup.NewRestorer("data")
	probeAgentRegistry, err := probeagent.NewRegistry(probeagent.RegistryConfig{
		Path:                 config.CLIConfig.RemoteDiagnostics.RegistryPath,
		Enabled:              config.CLIConfig.RemoteDiagnostics.Enabled,
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

	xrayLifecycle := &sync.RWMutex{}
	proxyChecker.SetRunGate(xrayLifecycle.RLocker())
	speedTestManager := speedtest.NewManager(
		proxyChecker,
		config.CLIConfig.Xray.StartPort,
		"data/speedtest_schedule.json",
		speedtest.TestConfig{URL: config.CLIConfig.SpeedTest.URL},
	)
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
	handleStateLoadError("Telegram", telegramService.Load())
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
	if err := nodeArchive.RecordAvailability(); err != nil {
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
	runAvailabilityCheck := func(stableIDs []string, allowMaintenance bool) error {
		var recovered []string
		var checkErr error
		if len(stableIDs) == 0 {
			offlineBefore := offlineStableIDSet(proxyChecker)
			proxyChecker.CheckAllProxies()
			recovered = recoveredStableIDs(proxyChecker, offlineBefore)
		} else {
			var report checker.AvailabilityCheckReport
			if allowMaintenance {
				report, checkErr = proxyChecker.CheckProxiesByStableIDsIncludingMaintenance(stableIDs)
			} else {
				report, checkErr = proxyChecker.CheckProxiesByStableIDs(stableIDs)
			}
			recovered = report.RecoveredStableIDs()
		}
		if err := nodeArchive.RecordAvailability(); err != nil {
			logger.Warn("Failed to record node availability after manual check: %v", err)
		}
		remnawaveService.Trigger()
		notifyRecoveredNodes(recovered)
		return checkErr
	}
	runManualAvailabilityCheck := func(stableIDs []string) error {
		return runAvailabilityCheck(stableIDs, false)
	}
	runAdminAvailabilityCheck := func(stableIDs []string) error {
		return runAvailabilityCheck(stableIDs, true)
	}
	telegramService.SetAvailabilityCheckFunc(runManualAvailabilityCheck)
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

	speedTestManager.SetReporter(telegramService)
	telegramService.Start()

	speedTestManager.StartScheduler()
	defer speedTestManager.Stop()
	defer telegramService.Stop()

	runCheckIteration := func() {
		logger.Info("Starting proxy check iteration")
		offlineBefore := offlineStableIDSet(proxyChecker)
		proxyChecker.CheckAllProxies()
		recovered := recoveredStableIDs(proxyChecker, offlineBefore)
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
					if err != nil {
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

	subscriptionRefreshLock := make(chan struct{}, 1)
	refreshSubscription := func(source string, force bool, confirmationToken string) (web.AdminSubscriptionRefreshResult, error) {
		select {
		case subscriptionRefreshLock <- struct{}{}:
			defer func() { <-subscriptionRefreshLock }()
		default:
			return web.AdminSubscriptionRefreshResult{}, fmt.Errorf("subscription refresh already running")
		}

		logger.Info("Checking subscriptions for updates (%s)...", source)
		newConfigs, err := subscription.ReadFromMultipleSources(config.CLIConfig.Subscription.URLs)
		if err != nil {
			return web.AdminSubscriptionRefreshResult{}, fmt.Errorf("fetch subscriptions: %w", err)
		}

		if config.CLIConfig.Proxy.ResolveDomains {
			resolved, err := subscription.ResolveDomainsForConfigs(newConfigs)
			if err != nil {
				logger.Error("Error resolving domains: %v", err)
			} else {
				newConfigs = resolved
			}
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
			return refreshResult, nil
		}

		xrayLifecycle.Lock()
		err = updateConfiguration(newConfigs, proxyConfigs, xrayRunner, proxyChecker)
		if err != nil {
			xrayLifecycle.Unlock()
			return web.AdminSubscriptionRefreshResult{}, err
		}
		if err := nodeArchive.SyncProxies(*proxyConfigs); err != nil {
			logger.Warn("Failed to sync node registry after subscription update: %v", err)
		}
		proxyChecker.ReplaceMaintenanceModes(nodeArchive.ActiveMaintenanceStableIDs())
		xrayLifecycle.Unlock()
		if err := telegramService.PruneInactiveMutedNodes(); err != nil {
			logger.Warn("Failed to prune inactive muted Telegram nodes: %v", err)
		}
		remnawaveService.Trigger()

		refreshResult.Updated = true
		refreshResult.Message = "Configuration updated"
		return refreshResult, nil
	}

	if config.CLIConfig.Subscription.Update {
		updateScheduler := gocron.NewScheduler(time.UTC)
		updateScheduler.Every(config.CLIConfig.Subscription.UpdateInterval).Seconds().WaitForSchedule().Do(func() {
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

	web.RegisterConfigEndpoints(*proxyConfigs, proxyChecker, config.CLIConfig.Xray.StartPort)

	protectedHandler := http.NewServeMux()
	protectedHandler.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	protectedHandler.Handle("/config/", web.ConfigStatusHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/proxies/", web.APIProxyHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/proxies", web.APIProxiesHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/config", web.APIConfigHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/status", web.APIStatusHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/system/info", web.APISystemInfoHandler(version, startTime))
	protectedHandler.Handle("/api/v1/system/ip", web.APISystemIPHandler(proxyChecker))
	protectedHandler.Handle("/api/v1/docs", web.APIDocsHandler())
	protectedHandler.Handle("/api/v1/openapi.yaml", web.APIOpenAPIHandler())
	protectedHandler.Handle("/admin", web.AdminHandler())
	protectedHandler.Handle("/admin/", web.AdminHandler())
	protectedHandler.Handle("/api/v1/admin/proxies", web.AdminProxiesHandler(proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/admin/proxies/check", web.AdminProxyCheckHandler(runAdminAvailabilityCheck, proxyChecker, config.CLIConfig.Xray.StartPort))
	protectedHandler.Handle("/api/v1/admin/subscription/refresh", web.AdminSubscriptionRefreshHandler(func(request web.AdminSubscriptionRefreshRequest) (web.AdminSubscriptionRefreshResult, error) {
		return refreshSubscription("manual", request.Force, request.ConfirmationToken)
	}))
	protectedHandler.Handle("/api/v1/admin/backup", web.AdminBackupHandler(backupCreator))
	protectedHandler.Handle("/api/v1/admin/backup/restore", web.AdminBackupRestoreHandler(backupRestorer, nodeMergeCoordinator.AcquireRestoreGuard))
	protectedHandler.Handle("/api/v1/admin/speed-tests/run", web.AdminSpeedTestRunHandler(speedTestManager, runAdminAvailabilityCheck))
	protectedHandler.Handle("/api/v1/admin/speed-tests/node-url", web.AdminSpeedTestNodeURLHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests/history", web.AdminSpeedTestHistoryHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests", web.AdminSpeedTestSnapshotHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/availability/history", web.AdminAvailabilityHistoryHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/geo", web.AdminNodesOverviewGeoHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/merge/preview", web.AdminNodesOverviewMergePreviewHandler(nodeMergeCoordinator))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/merge", web.AdminNodesOverviewMergeHandler(nodeMergeCoordinator))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/delete", web.AdminNodesOverviewDeleteHandler(nodeArchive, speedTestManager))
	protectedHandler.Handle("/api/v1/admin/nodes-overview/maintenance", web.AdminNodeMaintenanceHandler(setNodeMaintenance))
	protectedHandler.Handle("/api/v1/admin/nodes-overview", web.AdminNodesOverviewHandler(nodeArchive, speedTestManager))
	protectedHandler.Handle("/api/v1/admin/incidents", web.AdminIncidentsHandler(nodeArchive))
	protectedHandler.Handle("/api/v1/admin/schedules", web.AdminScheduleHandler(speedTestManager, nodeArchive))
	protectedHandler.Handle("/api/v1/admin/telegram/test", web.AdminTelegramTestHandler(telegramService))
	protectedHandler.Handle("/api/v1/admin/telegram", web.AdminTelegramHandler(telegramService))
	protectedHandler.Handle("/api/v1/admin/remnawave/sync", web.AdminRemnawaveSyncHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/remnawave", web.AdminRemnawaveHandler(remnawaveService))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents/reissue", web.AdminDiagnosticAgentReissueHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents/revoke", web.AdminDiagnosticAgentRevokeHandler(probeAgentRegistry))
	protectedHandler.Handle("/api/v1/admin/diagnostic-agents", web.AdminDiagnosticAgentsHandler(probeAgentRegistry))

	if config.CLIConfig.Web.Public {
		mux.Handle("/", web.IndexHandler(version, proxyChecker))
		mux.Handle("/config/", web.ConfigStatusHandler(proxyChecker))
		middlewareHandler := web.BasicAuthMiddleware(
			config.CLIConfig.Metrics.Username,
			config.CLIConfig.Metrics.Password,
		)(protectedHandler)
		mux.Handle("/admin", middlewareHandler)
		mux.Handle("/admin/", middlewareHandler)
		mux.Handle("/metrics", middlewareHandler)
		mux.Handle("/api/", middlewareHandler)
	} else if config.CLIConfig.Metrics.Protected {
		protectedHandler.Handle("/", web.IndexHandler(version, proxyChecker))
		middlewareHandler := web.BasicAuthMiddleware(
			config.CLIConfig.Metrics.Username,
			config.CLIConfig.Metrics.Password,
		)(protectedHandler)
		mux.Handle("/", middlewareHandler)
	} else {
		protectedHandler.Handle("/", web.IndexHandler(version, proxyChecker))
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

func offlineStableIDSet(proxyChecker *checker.ProxyChecker) map[string]bool {
	offline := make(map[string]bool)
	for _, proxy := range proxyChecker.GetProxies() {
		if proxy == nil {
			continue
		}
		stableID := proxy.StableID
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		details, err := proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err == nil && details.IsOffline() {
			offline[stableID] = true
		}
	}
	return offline
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

func recoveredStableIDs(proxyChecker *checker.ProxyChecker, offlineBefore map[string]bool) []string {
	if len(offlineBefore) == 0 {
		return nil
	}
	recovered := make([]string, 0)
	for stableID := range offlineBefore {
		details, err := proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err == nil && details.EffectiveStatus() == checker.AvailabilityStateOnline {
			recovered = append(recovered, stableID)
		}
	}
	return recovered
}
