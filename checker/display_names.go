package checker

import (
	"strings"

	"xray-checker/models"
)

// Display names are the operator's own labels for nodes, kept apart from the
// names the subscription supplies.
//
// A panel is free to name its outbounds after its routing topology —
// "proxy-01-nl-edge-host-01" is a description of where a tunnel goes, not
// a name anyone wants to read down a list of thirty. The operator can put their
// own words on a node without the checker rewriting anything: the subscription
// name stays exactly as it arrived, in the archive, in the effective config a
// refresh compares and in every place identity is decided.

// ApplyDisplayNames replaces every stored label. It is called after startup and
// after each refresh, from the archive that owns them.
func (pc *ProxyChecker) ApplyDisplayNames(names map[string]string) {
	applied := make(map[string]string, len(names))
	for stableID, name := range names {
		stableID = strings.TrimSpace(stableID)
		name = strings.TrimSpace(name)
		if stableID == "" || name == "" {
			continue
		}
		applied[stableID] = name
	}
	pc.displayNamesMu.Lock()
	pc.displayNames = applied
	pc.displayNamesMu.Unlock()
}

// DisplayName returns the operator's label for a node, empty when they set none.
func (pc *ProxyChecker) DisplayName(stableID string) string {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return ""
	}
	pc.displayNamesMu.RLock()
	defer pc.displayNamesMu.RUnlock()
	return pc.displayNames[stableID]
}

// LabelFor is what a node is called on screen: the operator's label when there
// is one, and the name the subscription gave otherwise.
func (pc *ProxyChecker) LabelFor(stableID string, subscriptionName string) string {
	if label := pc.DisplayName(stableID); label != "" {
		return label
	}
	return subscriptionName
}

// Label is LabelFor for a proxy in hand.
func (pc *ProxyChecker) Label(proxy *models.ProxyConfig) string {
	if proxy == nil {
		return ""
	}
	return pc.LabelFor(proxy.StableID, proxy.Name)
}
