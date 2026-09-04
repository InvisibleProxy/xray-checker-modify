package subscription

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"xray-checker/config"
	"xray-checker/logger"
	"xray-checker/models"
	"xray-checker/xray"
)

var (
	subscriptionName string
	subNameMu        sync.RWMutex
)

func GetSubscriptionName() string {
	subNameMu.RLock()
	defer subNameMu.RUnlock()
	return subscriptionName
}

func SetSubscriptionName(name string) {
	subNameMu.Lock()
	defer subNameMu.Unlock()
	subscriptionName = name
}

type subscriptionResult struct {
	URL     string
	Name    string
	Configs []*models.ProxyConfig
	Error   error
}

func InitializeConfiguration(configFile string, version string, feeds []Feed) (*[]*models.ProxyConfig, error) {
	if len(feeds) == 0 {
		feeds = FeedsFromURLs(config.CLIConfig.Subscription.URLs)
	}

	var configs []*models.ProxyConfig
	if len(feeds) == 0 {
		// Neither the environment nor the panel has a source yet. Starting with
		// no nodes is better than refusing to start: the panel comes up, and the
		// first subscription can be added there. Anything else would make the
		// admin UI reachable only once a subscription already exists.
		logger.Warn("No subscription sources configured; starting with no nodes. Add one in the admin panel under Settings → Subscriptions.")
	} else {
		var err error
		configs, err = ReadFromFeeds(feeds)
		if err != nil {
			return nil, err
		}
	}

	proxyConfigs := configs

	if config.CLIConfig.Proxy.ResolveDomains && len(configs) > 0 {
		resolved, err := ResolveDomainsForConfigs(configs)
		if err != nil {
			return nil, err
		}
		proxyConfigs = resolved
	}

	if deduplicated, dropped := xray.DeduplicateByStableID(proxyConfigs); dropped > 0 {
		logger.Info("Dropped %d duplicate node(s) listed more than once by the subscription", dropped)
		proxyConfigs = deduplicated
	}

	xray.PrepareProxyConfigs(proxyConfigs)
	if err := xray.ValidateStableIDs(proxyConfigs); err != nil {
		return nil, fmt.Errorf("invalid proxy identity: %w", err)
	}

	configGenerator := xray.NewConfigGenerator()
	if err := configGenerator.GenerateAndSaveConfig(
		proxyConfigs,
		config.CLIConfig.Xray.StartPort,
		configFile,
		config.CLIConfig.Xray.LogLevel,
	); err != nil {
		return nil, err
	}

	return &proxyConfigs, nil
}

// Feed is one subscription to fetch, together with the client identity to
// fetch it as. A feed from the environment uses the checker's own identity; a
// feed added from the panel may impersonate Happ or INCY to reach a panel that
// answers only known clients or enforces an HWID device limit.
type Feed struct {
	URL     string
	Profile ClientProfile
	// Name overrides the name the subscription reports about itself. It is the
	// label the operator gave the source in the panel.
	Name string
}

func FeedsFromURLs(urls []string) []Feed {
	feeds := make([]Feed, 0, len(urls))
	for _, url := range urls {
		feeds = append(feeds, Feed{URL: url})
	}
	return feeds
}

func ReadFromMultipleSources(urls []string) ([]*models.ProxyConfig, error) {
	return ReadFromFeeds(FeedsFromURLs(urls))
}

func ReadFromFeeds(feeds []Feed) ([]*models.ProxyConfig, error) {
	if len(feeds) == 0 {
		return nil, fmt.Errorf("no subscription URLs provided")
	}

	if len(feeds) == 1 {
		configs, name, err := ReadFromFeed(feeds[0])
		if err != nil {
			return nil, err
		}
		for _, cfg := range configs {
			cfg.SubName = name
		}
		if name != "" {
			SetSubscriptionName(name)
		}
		return configs, nil
	}

	logger.Debug("Fetching %d subscriptions in parallel", len(feeds))

	resultMap := make(map[string]subscriptionResult)
	var resultMu sync.Mutex

	var wg sync.WaitGroup
	for _, feed := range feeds {
		wg.Add(1)
		go func(f Feed) {
			defer wg.Done()
			configs, name, err := ReadFromFeed(f)
			for _, cfg := range configs {
				cfg.SubName = name
			}
			resultMu.Lock()
			resultMap[f.URL] = subscriptionResult{
				URL:     f.URL,
				Name:    name,
				Configs: configs,
				Error:   err,
			}
			resultMu.Unlock()
		}(feed)
	}

	wg.Wait()

	var allConfigs []*models.ProxyConfig
	var errors []error
	var firstName string
	successCount := 0

	for _, feed := range feeds {
		result := resultMap[feed.URL]
		if result.Error != nil {
			logger.Warn("Failed to fetch subscription %s: %v", result.URL, result.Error)
			errors = append(errors, fmt.Errorf("%s: %v", result.URL, result.Error))
			continue
		}
		logger.Debug("Fetched %d proxies from %s (name: %s)", len(result.Configs), result.URL, result.Name)
		allConfigs = append(allConfigs, result.Configs...)
		if firstName == "" && result.Name != "" {
			firstName = result.Name
		}
		successCount++
	}

	if successCount == 0 {
		return nil, fmt.Errorf("failed to fetch any subscription: %v", errors)
	}

	if firstName != "" {
		SetSubscriptionName(firstName)
	}

	for i := range allConfigs {
		allConfigs[i].Index = i
	}

	logger.Debug("Total: %d proxies from %d/%d subscriptions", len(allConfigs), successCount, len(feeds))
	return allConfigs, nil
}

func ReadFromSource(source string) ([]*models.ProxyConfig, string, error) {
	return ReadFromFeed(Feed{URL: source})
}

// ReadFromFeed fetches one subscription as the client its profile names. An
// operator-supplied name wins over the one the subscription reports, because it
// is what labels the source in the panel.
func ReadFromFeed(feed Feed) ([]*models.ProxyConfig, string, error) {
	parser := NewParserForProfile(feed.Profile)
	result, err := parser.Parse(feed.URL)
	if err != nil {
		return nil, "", err
	}
	name := result.Name
	if trimmed := strings.TrimSpace(feed.Name); trimmed != "" {
		name = trimmed
	}

	configs, dropped := dropPlaceholderNodes(result.Configs)
	logDroppedPlaceholders(feed.URL, dropped)
	if len(configs) == 0 && len(dropped) > 0 {
		// The panel answered, but with notices instead of nodes. Saying so
		// beats reporting an empty subscription the operator cannot explain.
		return nil, "", fmt.Errorf(
			"subscription returned only placeholder entries (%d), which usually means the panel refused it — most often an exhausted HWID device limit",
			len(dropped),
		)
	}
	return configs, name, nil
}

func ResolveDomainsForConfigs(configs []*models.ProxyConfig) ([]*models.ProxyConfig, error) {
	var out []*models.ProxyConfig
	for _, cfg := range configs {
		if ip := net.ParseIP(cfg.Server); ip != nil {
			out = append(out, cfg)
			continue
		}

		ips, err := net.LookupIP(cfg.Server)
		if err != nil || len(ips) == 0 {
			logger.Warn("Failed to resolve domain %s: %v", cfg.Server, err)
			out = append(out, cfg)
			continue
		}

		type resolvedConfig struct {
			config   *models.ProxyConfig
			stableID string
		}
		resolved := make([]resolvedConfig, 0, len(ips))

		for _, ip := range ips {
			clone := *cfg
			clone.Server = ip.String()
			clone.StableID = clone.GenerateStableID()
			resolved = append(resolved, resolvedConfig{
				config:   &clone,
				stableID: clone.StableID,
			})
		}

		sort.Slice(resolved, func(i, j int) bool {
			return resolved[i].stableID < resolved[j].stableID
		})

		for i, item := range resolved {
			if len(ips) > 1 {
				item.config.Name = fmt.Sprintf("%s #%d", cfg.Name, i+1)
			}
			out = append(out, item.config)
		}
	}
	return out, nil
}
