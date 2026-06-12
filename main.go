package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/config"
	"xray-checker/logger"
	"xray-checker/metrics"
	"xray-checker/models"
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

	speedTestManager := speedtest.NewManager(
		proxyChecker,
		config.CLIConfig.Xray.StartPort,
		"data/speedtest_schedule.json",
		speedtest.TestConfig{URL: config.CLIConfig.SpeedTest.URL},
	)
	if err := speedTestManager.Load(); err != nil {
		logger.Warn("Failed to load speed test schedule: %v", err)
	}

	telegramService := telegram.NewService(
		"data/telegram_config.json",
		proxyChecker,
		speedTestManager,
		config.CLIConfig.Xray.StartPort,
	)
	if err := telegramService.Load(); err != nil {
		logger.Warn("Failed to load Telegram settings: %v", err)
	}
	speedTestManager.SetReporter(telegramService)
	telegramService.Start()
	defer telegramService.Stop()

	speedTestManager.StartScheduler()
	defer speedTestManager.Stop()

	runCheckIteration := func() {
		logger.Info("Starting proxy check iteration")
		proxyChecker.CheckAllProxies()
		go telegramService.NotifyNodeStatuses()

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

	subscriptionRefreshLock := make(chan struct{}, 1)
	refreshSubscription := func(source string) (web.AdminSubscriptionRefreshResult, error) {
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

		if xray.IsConfigsEqual(*proxyConfigs, newConfigs) {
			logger.Info("Subscriptions checked, no changes")
			return web.AdminSubscriptionRefreshResult{
				Updated: false,
				Count:   len(newConfigs),
				Message: "Subscriptions checked, no changes",
			}, nil
		}

		if err := updateConfiguration(newConfigs, proxyConfigs, xrayRunner, proxyChecker); err != nil {
			return web.AdminSubscriptionRefreshResult{}, err
		}

		return web.AdminSubscriptionRefreshResult{
			Updated: true,
			Count:   len(newConfigs),
			Message: "Configuration updated",
		}, nil
	}

	if config.CLIConfig.Subscription.Update {
		updateScheduler := gocron.NewScheduler(time.UTC)
		updateScheduler.Every(config.CLIConfig.Subscription.UpdateInterval).Seconds().WaitForSchedule().Do(func() {
			if _, err := refreshSubscription("scheduled"); err != nil {
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
	protectedHandler.Handle("/api/v1/admin/subscription/refresh", web.AdminSubscriptionRefreshHandler(func() (web.AdminSubscriptionRefreshResult, error) {
		return refreshSubscription("manual")
	}))
	protectedHandler.Handle("/api/v1/admin/speed-tests/run", web.AdminSpeedTestRunHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests/node-url", web.AdminSpeedTestNodeURLHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests/history", web.AdminSpeedTestHistoryHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/speed-tests", web.AdminSpeedTestSnapshotHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/schedules", web.AdminScheduleHandler(speedTestManager))
	protectedHandler.Handle("/api/v1/admin/telegram/test", web.AdminTelegramTestHandler(telegramService))
	protectedHandler.Handle("/api/v1/admin/telegram", web.AdminTelegramHandler(telegramService))

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
	if err := configGenerator.GenerateAndSaveConfig(
		newConfigs,
		config.CLIConfig.Xray.StartPort,
		configFile,
		config.CLIConfig.Xray.LogLevel,
	); err != nil {
		return err
	}

	if err := xrayRunner.Stop(); err != nil {
		return err
	}

	if err := xrayRunner.Start(); err != nil {
		return err
	}

	proxyChecker.UpdateProxies(newConfigs)

	*currentConfigs = newConfigs

	web.RegisterConfigEndpoints(newConfigs, proxyChecker, config.CLIConfig.Xray.StartPort)

	logger.Info("Configuration updated: %d proxies", len(newConfigs))
	return nil
}
