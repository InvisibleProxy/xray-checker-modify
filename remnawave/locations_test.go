package remnawave

import (
	"fmt"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

// evaluateLocationsFixture wires the minimum a location evaluation needs: every
// member is a visible host with a known proxy, so only availability decides.
type locationFixture struct {
	locations  map[string]AnnounceLocation
	visible    map[string]bool
	hosts      map[string]Host
	proxies    map[string]*models.ProxyConfig
	statuses   map[string]checker.ProxyStatusDetails
	failedFor  time.Duration
	now        time.Time
	membersSeq int
}

func newLocationFixture(now time.Time) *locationFixture {
	return &locationFixture{
		locations: map[string]AnnounceLocation{},
		visible:   map[string]bool{},
		hosts:     map[string]Host{},
		proxies:   map[string]*models.ProxyConfig{},
		statuses:  map[string]checker.ProxyStatusDetails{},
		failedFor: 30 * time.Minute,
		now:       now,
	}
}

// add registers one member of a location: a node at server:port, either online or
// confirmed down for long enough to count as an outage.
func (f *locationFixture) add(locationKey, server string, port int, online bool) {
	f.membersSeq++
	stableID := fmt.Sprintf("%s-%d", server, f.membersSeq)
	hostUUID := "host-" + stableID

	location, ok := f.locations[locationKey]
	if !ok {
		location = AnnounceLocation{PublicLabel: locationKey, Members: map[string]string{}}
		f.locations[locationKey] = location
	}
	location.Members[stableID] = hostUUID

	f.visible[hostUUID] = true
	f.hosts[hostUUID] = Host{UUID: hostUUID}
	f.proxies[stableID] = &models.ProxyConfig{StableID: stableID, Server: server, Port: port}
	if online {
		f.statuses[stableID] = checker.ProxyStatusDetails{Online: true}
	} else {
		f.statuses[stableID] = checker.ProxyStatusDetails{ProxyFailureSince: f.now.Add(-f.failedFor)}
	}
}

func (f *locationFixture) evaluate() map[string]groupEvaluation {
	observations := map[string]int{}
	for stableID := range f.proxies {
		observations[stableID] = 10
	}
	return evaluateLocations(
		f.locations,
		Policy{OutageMinutes: 5, MinimumFailures: 2},
		f.visible,
		f.hosts,
		f.proxies,
		f.statuses,
		map[string]bool{},
		observations,
		map[string]bool{},
		f.now,
	)
}

// A host exposed over two transports is one server. Losing one transport leaves
// the server reachable, so the location must not announce partial availability.
func TestEvaluateLocationsKeepsLocationHealthyWhenOneTransportOfAServerFails(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	f := newLocationFixture(now)
	f.add("nl", "144.31.86.63", 8443, true)
	f.add("nl", "144.31.86.63", 2096, false)

	if got := f.evaluate()["nl"].State; got != groupHealthy {
		t.Fatalf("State = %q, want %q", got, groupHealthy)
	}
}

// Once every transport of one server is gone, that server is lost and the
// location is partially available again.
func TestEvaluateLocationsReportsPartialWhenAWholeServerIsLost(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	f := newLocationFixture(now)
	f.add("nl", "144.31.86.63", 8443, true)
	f.add("nl", "144.31.86.63", 2096, true)
	f.add("nl", "138.124.3.225", 8443, false)
	f.add("nl", "138.124.3.225", 2096, false)

	if got := f.evaluate()["nl"].State; got != groupPartial {
		t.Fatalf("State = %q, want %q", got, groupPartial)
	}
}

// Every server gone is still a full outage.
func TestEvaluateLocationsReportsDownWhenEveryServerIsLost(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	f := newLocationFixture(now)
	f.add("nl", "144.31.86.63", 8443, false)
	f.add("nl", "144.31.86.63", 2096, false)
	f.add("nl", "138.124.3.225", 8443, false)

	if got := f.evaluate()["nl"].State; got != groupDown {
		t.Fatalf("State = %q, want %q", got, groupDown)
	}
}

// Single-transport locations keep their previous behaviour exactly.
func TestEvaluateLocationsStillReportsPartialForSingleTransportServers(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	f := newLocationFixture(now)
	f.add("de", "31.76.38.179", 443, true)
	f.add("de", "83.219.249.142", 4443, false)

	if got := f.evaluate()["de"].State; got != groupPartial {
		t.Fatalf("State = %q, want %q", got, groupPartial)
	}
}

// A member left without a Host UUID is paired from the topology: for a panel-served
// subscription the node's address and port are the host's own.
func TestResolveMemberHostsPairsByEndpoint(t *testing.T) {
	locations := map[string]AnnounceLocation{
		"nl": {Members: map[string]string{"node-tcp": "", "node-xhttp": ""}},
	}
	proxies := map[string]*models.ProxyConfig{
		"node-tcp":   {StableID: "node-tcp", Server: "144.31.86.63", Port: 8443},
		"node-xhttp": {StableID: "node-xhttp", Server: "144.31.86.63", Port: 2096},
	}
	hosts := []Host{
		{UUID: "host-tcp", Address: "144.31.86.63", Port: 8443},
		{UUID: "host-xhttp", Address: "144.31.86.63", Port: 2096},
		{UUID: "host-other", Address: "138.124.3.225", Port: 8443},
	}

	got := resolveMemberHosts(locations, proxies, hosts)["nl"].Members

	if got["node-tcp"] != "host-tcp" {
		t.Errorf("node-tcp paired with %q, want host-tcp", got["node-tcp"])
	}
	if got["node-xhttp"] != "host-xhttp" {
		t.Errorf("node-xhttp paired with %q, want host-xhttp", got["node-xhttp"])
	}
}

// An explicit pairing is the operator's decision and must survive resolution.
func TestResolveMemberHostsKeepsExplicitPairing(t *testing.T) {
	locations := map[string]AnnounceLocation{
		"nl": {Members: map[string]string{"node": "host-chosen"}},
	}
	proxies := map[string]*models.ProxyConfig{"node": {StableID: "node", Server: "1.2.3.4", Port: 443}}
	hosts := []Host{{UUID: "host-derived", Address: "1.2.3.4", Port: 443}}

	if got := resolveMemberHosts(locations, proxies, hosts)["nl"].Members["node"]; got != "host-chosen" {
		t.Fatalf("pairing = %q, want the operator's host-chosen", got)
	}
}

// Two hosts on one endpoint cannot be told apart from the node alone, so the
// member stays unpaired rather than being attached to an arbitrary one.
func TestResolveMemberHostsLeavesAmbiguousEndpointUnpaired(t *testing.T) {
	locations := map[string]AnnounceLocation{
		"nl": {Members: map[string]string{"node": ""}},
	}
	proxies := map[string]*models.ProxyConfig{"node": {StableID: "node", Server: "1.2.3.4", Port: 443}}
	hosts := []Host{
		{UUID: "host-a", Address: "1.2.3.4", Port: 443},
		{UUID: "host-b", Address: "1.2.3.4", Port: 443},
	}

	if got := resolveMemberHosts(locations, proxies, hosts)["nl"].Members["node"]; got != "" {
		t.Fatalf("pairing = %q, want the member left unpaired", got)
	}
}

type suggestProxySource struct{ proxies []*models.ProxyConfig }

func (s suggestProxySource) GetProxies() []*models.ProxyConfig { return s.proxies }
func (s suggestProxySource) GetProxyStatusDetailsIncludingMaintenance(string) (checker.ProxyStatusDetails, error) {
	return checker.ProxyStatusDetails{}, nil
}
func (s suggestProxySource) MonitoringEnabled(string) bool { return true }

// The panel already groups hosts with tags, so the checker can report those
// groupings instead of asking for every node to be paired by hand. It never picks
// which tags are locations - BALANCER_NL usually is, EU usually is not.
func TestSuggestLocationsGroupsNodesByHostTag(t *testing.T) {
	service := &Service{
		proxySource: suggestProxySource{proxies: []*models.ProxyConfig{
			{StableID: "nl-tcp", Name: "🇳🇱 Нидерланды", Server: "144.31.86.63", Port: 8443},
			{StableID: "nl-xhttp", Name: "🇳🇱 Нидерланды xHTTP", Server: "144.31.86.63", Port: 2096},
			{StableID: "de-tcp", Name: "🇩🇪 Германия #2", Server: "31.76.38.179", Port: 443},
			{StableID: "stranger", Name: "Other subscription", Server: "10.0.0.1", Port: 443},
		}},
		topology: Topology{Hosts: []Host{
			{UUID: "h-nl-tcp", Remark: "NL", Address: "144.31.86.63", Port: 8443, Tags: []string{"BALANCER_NL", "EU"}},
			{UUID: "h-nl-xhttp", Remark: "NL xHTTP", Address: "144.31.86.63", Port: 2096, Tags: []string{"BALANCER_NL", "EU"}},
			{UUID: "h-de", Remark: "DE", Address: "31.76.38.179", Port: 443, Tags: []string{"BALANCER_GE", "EU"}},
		}},
	}

	suggestion := service.SuggestLocations()

	byTag := map[string][]string{}
	for _, candidate := range suggestion.Candidates {
		for _, member := range candidate.Members {
			byTag[candidate.Tag] = append(byTag[candidate.Tag], member.StableID)
		}
	}
	if got := byTag["BALANCER_NL"]; len(got) != 2 {
		t.Errorf("BALANCER_NL members = %v, want both Dutch nodes", got)
	}
	if got := byTag["BALANCER_GE"]; len(got) != 1 {
		t.Errorf("BALANCER_GE members = %v, want the single German node", got)
	}
	if got := byTag["EU"]; len(got) != 3 {
		t.Errorf("EU members = %v, want every tagged node", got)
	}

	if len(suggestion.Unmatched) != 1 || suggestion.Unmatched[0].StableID != "stranger" {
		t.Fatalf("unmatched = %+v, want only the node with no host", suggestion.Unmatched)
	}

	// Candidates carry the pairing so the admin can show what a location covers.
	for _, candidate := range suggestion.Candidates {
		for _, member := range candidate.Members {
			if member.HostUUID == "" {
				t.Errorf("%s in %s has no paired host", member.StableID, candidate.Tag)
			}
		}
	}
}
