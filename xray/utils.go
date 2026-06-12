package xray

import (
	"encoding/json"
	"strings"

	"xray-checker/models"
)

func PrepareProxyConfigs(proxies []*models.ProxyConfig) {
	for i := range proxies {
		proxies[i].Index = i

		if proxies[i].StableID == "" {
			proxies[i].StableID = proxies[i].GenerateStableID()
		}
	}
}

func IsConfigsEqual(old, new []*models.ProxyConfig) bool {
	if len(old) != len(new) {
		return false
	}

	oldMap := make(map[string]string)
	newMap := make(map[string]string)

	for _, cfg := range old {
		if cfg.StableID == "" {
			cfg.StableID = cfg.GenerateStableID()
		}
		if _, exists := oldMap[cfg.StableID]; exists {
			return false
		}
		oldMap[cfg.StableID] = configSignature(cfg)
	}

	for _, cfg := range new {
		if cfg.StableID == "" {
			cfg.StableID = cfg.GenerateStableID()
		}
		if _, exists := newMap[cfg.StableID]; exists {
			return false
		}
		newMap[cfg.StableID] = configSignature(cfg)
	}

	if len(oldMap) != len(newMap) {
		return false
	}

	for stableID, oldSignature := range oldMap {
		if newMap[stableID] != oldSignature {
			return false
		}
	}

	return true
}

func PreserveStableIDs(old, new []*models.ProxyConfig) int {
	oldByStableID := uniqueProxyMap(old, func(proxy *models.ProxyConfig) string {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		return proxy.StableID
	})
	oldByGeneratedID := uniqueProxyMap(old, func(proxy *models.ProxyConfig) string {
		return proxy.GenerateStableID()
	})
	oldByStrictIdentity := uniqueProxyMap(old, strictIdentityKey)
	oldByRelaxedIdentity := uniqueProxyMap(old, relaxedIdentityKey)
	newStrictCounts := identityCounts(new, strictIdentityKey)
	newRelaxedCounts := identityCounts(new, relaxedIdentityKey)

	usedOldIDs := make(map[string]bool)
	preserved := 0

	for _, proxy := range new {
		if proxy == nil {
			continue
		}

		generatedID := proxy.GenerateStableID()
		proxy.StableID = generatedID

		if oldProxy := oldByStableID[generatedID]; oldProxy != nil {
			if assignPreservedStableID(proxy, oldProxy, generatedID, usedOldIDs) {
				preserved++
			}
			continue
		}
		if oldProxy := oldByGeneratedID[generatedID]; oldProxy != nil {
			if assignPreservedStableID(proxy, oldProxy, generatedID, usedOldIDs) {
				preserved++
			}
			continue
		}

		if key := strictIdentityKey(proxy); key != "" && newStrictCounts[key] == 1 {
			if oldProxy := oldByStrictIdentity[key]; oldProxy != nil {
				if assignPreservedStableID(proxy, oldProxy, generatedID, usedOldIDs) {
					preserved++
				}
				continue
			}
		}

		if key := relaxedIdentityKey(proxy); key != "" && newRelaxedCounts[key] == 1 {
			if oldProxy := oldByRelaxedIdentity[key]; oldProxy != nil {
				if assignPreservedStableID(proxy, oldProxy, generatedID, usedOldIDs) {
					preserved++
				}
			}
		}
	}

	return preserved
}

func assignPreservedStableID(newProxy, oldProxy *models.ProxyConfig, generatedID string, usedOldIDs map[string]bool) bool {
	if oldProxy == nil {
		return false
	}
	if oldProxy.StableID == "" {
		oldProxy.StableID = oldProxy.GenerateStableID()
	}
	if oldProxy.StableID == "" || usedOldIDs[oldProxy.StableID] {
		return false
	}
	usedOldIDs[oldProxy.StableID] = true
	newProxy.StableID = oldProxy.StableID
	return oldProxy.StableID != generatedID
}

func uniqueProxyMap(proxies []*models.ProxyConfig, keyFn func(*models.ProxyConfig) string) map[string]*models.ProxyConfig {
	result := make(map[string]*models.ProxyConfig)
	duplicates := make(map[string]bool)
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		key := keyFn(proxy)
		if key == "" {
			continue
		}
		if _, exists := result[key]; exists {
			duplicates[key] = true
			result[key] = nil
			continue
		}
		if duplicates[key] {
			continue
		}
		result[key] = proxy
	}
	return result
}

func identityCounts(proxies []*models.ProxyConfig, keyFn func(*models.ProxyConfig) string) map[string]int {
	result := make(map[string]int)
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		if key := keyFn(proxy); key != "" {
			result[key]++
		}
	}
	return result
}

func strictIdentityKey(proxy *models.ProxyConfig) string {
	name := normalizeIdentityPart(proxy.Name)
	protocol := normalizeIdentityPart(proxy.Protocol)
	if name == "" || protocol == "" {
		return ""
	}
	return strings.Join([]string{
		normalizeIdentityPart(proxy.SubName),
		protocol,
		name,
	}, "\x00")
}

func relaxedIdentityKey(proxy *models.ProxyConfig) string {
	name := normalizeIdentityPart(proxy.Name)
	protocol := normalizeIdentityPart(proxy.Protocol)
	if name == "" || protocol == "" {
		return ""
	}
	return strings.Join([]string{protocol, name}, "\x00")
}

func normalizeIdentityPart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func configSignature(proxy *models.ProxyConfig) string {
	if proxy == nil {
		return ""
	}
	clone := *proxy
	clone.Index = 0
	data, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	return string(data)
}
