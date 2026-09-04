package subscription

import (
	"net"
	"strings"

	"xray-checker/logger"
	"xray-checker/models"
)

// Panels answer a refused subscription with content, not only with an error.
// A Remnawave deployment whose device limit is exhausted returns a normal
// subscription whose "nodes" are notices — "device limit reached", a link to
// the vendor's Telegram — pointed at a documentation address.
//
// Those entries parse like any other node, so without this filter they enter
// monitoring, sit offline forever, and push the real nodes out of the archive
// on the next refresh. The test is deliberately about the address rather than
// the name: notice text differs per panel and per language, while an address
// reserved for documentation can never be a working node.
var placeholderNetworks = func() []*net.IPNet {
	// RFC 5737 (IPv4 documentation) and RFC 3849 (IPv6 documentation).
	cidrs := []string{
		"192.0.2.0/24",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"2001:db8::/32",
	}
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			networks = append(networks, network)
		}
	}
	return networks
}()

// placeholderDomains are reserved by RFC 2606 and RFC 6761 and cannot host a
// real node either.
var placeholderDomains = []string{
	"example.com",
	"example.net",
	"example.org",
	"example.edu",
	".invalid",
	".test",
}

// IsPlaceholderServer reports whether an address can only ever be a stand-in.
func IsPlaceholderServer(server string) bool {
	server = strings.ToLower(strings.TrimSpace(server))
	if server == "" {
		return false
	}
	if ip := net.ParseIP(server); ip != nil {
		for _, network := range placeholderNetworks {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, domain := range placeholderDomains {
		if server == domain || strings.HasSuffix(server, "."+strings.TrimPrefix(domain, ".")) {
			return true
		}
	}
	return false
}

// dropPlaceholderNodes removes stand-in entries and reports how many went. When
// every node was one, the caller is told plainly: the subscription was answered
// but carries no monitorable node, which is what an exhausted device limit
// looks like from here.
func dropPlaceholderNodes(configs []*models.ProxyConfig) ([]*models.ProxyConfig, []string) {
	kept := make([]*models.ProxyConfig, 0, len(configs))
	var dropped []string
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		if IsPlaceholderServer(cfg.Server) {
			dropped = append(dropped, cfg.Name)
			continue
		}
		kept = append(kept, cfg)
	}
	return kept, dropped
}

func logDroppedPlaceholders(url string, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	names := dropped
	if len(names) > 3 {
		names = names[:3]
	}
	logger.Warn(
		"Subscription %s returned %d placeholder entr(y/ies) pointing at a documentation address; ignoring them: %s",
		url, len(dropped), strings.Join(names, ", "),
	)
}
