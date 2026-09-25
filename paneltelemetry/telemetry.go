// Package paneltelemetry keeps the Remnawave panel's view of each node where an
// alert can read it.
//
// The checker sees a node from one place, its own network. The panel sees the
// same node differently: it holds the control connection to it, and it counts
// the clients actually using it. Put next to a down or slow alert, that answers
// questions the checker's own evidence cannot — whether clients are still
// connected, whether the panel still reaches the node, whether the node is out
// of memory — without asking an operator to open another console at night.
//
// It is read-only and evidence only. The poller reads GET /api/nodes with the
// checker's token (scope nodes:list); nothing here writes to the panel, and no
// value from it decides whether an alert is sent.
package paneltelemetry

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultInterval = time.Minute
	// historyWindow is how far back the online count is kept. An alert asks
	// "how many clients did this node have before it failed"; a failure the
	// alert is about started at most a few check intervals ago, and a reminder
	// hours later has nothing to compare against by then anyway.
	historyWindow = 3 * time.Hour
	// staleAfter is how old a snapshot may be before an alert stops quoting it
	// as current.
	staleAfter = 5 * time.Minute
	// forbiddenBackoff paces the retries after the panel refused the token: a
	// missing scope does not fix itself between two polls, and a refusal every
	// minute would only fill the log.
	forbiddenBackoff = 30 * time.Minute
	maxStatusMessage = 120
)

// Node is the subset of the panel's node record the checker reads. The JSON
// names follow the panel's NodesSchema, so the client decodes straight into it.
type Node struct {
	UUID              string        `json:"uuid"`
	Name              string        `json:"name"`
	Address           string        `json:"address"`
	IsConnected       bool          `json:"isConnected"`
	IsDisabled        bool          `json:"isDisabled"`
	IsConnecting      bool          `json:"isConnecting"`
	LastStatusMessage *string       `json:"lastStatusMessage"`
	LastStatusChange  string        `json:"lastStatusChange"`
	XrayUptime        float64       `json:"xrayUptime"`
	UsersOnline       int           `json:"usersOnline"`
	IPs               []NodeIP      `json:"ips"`
	System            *NodeSystem   `json:"system"`
	Versions          *NodeVersions `json:"versions"`
}

type NodeIP struct {
	IP string `json:"ip"`
}

type NodeSystem struct {
	Info  NodeSystemInfo  `json:"info"`
	Stats NodeSystemStats `json:"stats"`
}

type NodeSystemInfo struct {
	CPUs        int     `json:"cpus"`
	MemoryTotal float64 `json:"memoryTotal"`
}

type NodeSystemStats struct {
	MemoryFree float64        `json:"memoryFree"`
	MemoryUsed float64        `json:"memoryUsed"`
	LoadAvg    []float64      `json:"loadAvg"`
	Interface  *NodeInterface `json:"interface"`
}

type NodeInterface struct {
	RxBytesPerSec float64 `json:"rxBytesPerSec"`
	TxBytesPerSec float64 `json:"txBytesPerSec"`
}

type NodeVersions struct {
	Xray string `json:"xray"`
}

// Source is the panel. The Remnawave client satisfies it.
type Source interface {
	GetNodes(context.Context) ([]Node, error)
}

// ErrForbidden is what a Source returns when the panel refused the token.
var ErrForbidden = errors.New("the Remnawave API token may not read nodes; grant it the nodes:list scope")

// Status is what an alert quotes about one node.
type Status struct {
	Name        string
	Connected   bool
	Connecting  bool
	Disabled    bool
	Message     string
	UsersOnline int
	// UsersOnlineBefore is the online count at the latest sample taken before
	// the moment an alert asked about, and HasBefore says whether there was one.
	UsersOnlineBefore int
	HasBefore         bool
	// MemoryUsedPercent is zero when the panel has no stats for the node.
	MemoryUsedPercent float64
	Load1             float64
	CPUs              int
	RxMbps            float64
	TxMbps            float64
	XrayUptime        time.Duration
	XrayVersion       string
	FetchedAt         time.Time
}

// State is the poller's own health, for the admin API and the log.
type State struct {
	Enabled   bool      `json:"enabled"`
	FetchedAt time.Time `json:"fetchedAt,omitempty"`
	Nodes     int       `json:"nodes"`
	Error     string    `json:"error,omitempty"`
}

type sample struct {
	at     time.Time
	online int
}

type Config struct {
	Interval time.Duration
	Now      func() time.Time
	// OnError reports a failed poll once per distinct error, so a persistent
	// problem is one log line rather than one a minute.
	OnError func(error)
}

// Poller refreshes the panel's node list and keeps a short online history.
type Poller struct {
	source Source
	config Config

	mu        sync.RWMutex
	nodes     []Node
	fetchedAt time.Time
	lastErr   error
	history   map[string][]sample
	retryAt   time.Time
}

func NewPoller(source Source, config Config) *Poller {
	if source == nil {
		return nil
	}
	if config.Interval <= 0 {
		config.Interval = DefaultInterval
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Poller{source: source, config: config, history: make(map[string][]sample)}
}

// Run polls until ctx ends. The first poll is immediate: an alert right after a
// restart is exactly when "how many clients were on this node" matters.
func (p *Poller) Run(ctx context.Context) {
	if p == nil {
		return
	}
	ticker := time.NewTicker(p.config.Interval)
	defer ticker.Stop()
	for {
		p.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Refresh polls once.
func (p *Poller) Refresh(ctx context.Context) {
	if p == nil {
		return
	}
	now := p.config.Now()
	p.mu.RLock()
	waiting := now.Before(p.retryAt)
	p.mu.RUnlock()
	if waiting {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	nodes, err := p.source.GetNodes(requestCtx)
	cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		changed := p.lastErr == nil || p.lastErr.Error() != err.Error()
		p.lastErr = err
		if errors.Is(err, ErrForbidden) {
			p.retryAt = now.Add(forbiddenBackoff)
		}
		if changed && p.config.OnError != nil {
			p.config.OnError(err)
		}
		return
	}
	p.lastErr = nil
	p.retryAt = time.Time{}
	p.nodes = append([]Node(nil), nodes...)
	p.fetchedAt = now
	cutoff := now.Add(-historyWindow)
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		seen[node.UUID] = true
		kept := p.history[node.UUID][:0]
		for _, entry := range p.history[node.UUID] {
			if !entry.at.Before(cutoff) {
				kept = append(kept, entry)
			}
		}
		p.history[node.UUID] = append(kept, sample{at: now, online: node.UsersOnline})
	}
	for uuid := range p.history {
		if !seen[uuid] {
			delete(p.history, uuid)
		}
	}
}

// State reports whether the poller is working.
func (p *Poller) State() State {
	if p == nil {
		return State{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	state := State{Enabled: true, FetchedAt: p.fetchedAt, Nodes: len(p.nodes)}
	if p.lastErr != nil {
		state.Error = p.lastErr.Error()
	}
	return state
}

// NodeStatus finds the panel node a checker node runs on, by the address the
// subscription publishes, and describes it. before is the moment the online
// count should be compared against — the start of an outage — and may be zero.
//
// The match is by address because that is the one thing both sides agree on: a
// subscription host is named for clients, a panel node for operators, and the
// same server carries several hosts. A node published under a name that is not
// the panel's own address is not matched, which leaves the alert without the
// line rather than quoting the wrong node.
func (p *Poller) NodeStatus(server string, before time.Time) (Status, bool) {
	if p == nil {
		return Status{}, false
	}
	server = normalizeHost(server)
	if server == "" {
		return Status{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.fetchedAt.IsZero() || p.config.Now().Sub(p.fetchedAt) > staleAfter {
		return Status{}, false
	}
	for _, node := range p.nodes {
		if !nodeServes(node, server) {
			continue
		}
		return p.statusLocked(node, before), true
	}
	return Status{}, false
}

func (p *Poller) statusLocked(node Node, before time.Time) Status {
	status := Status{
		Name: node.Name, Connected: node.IsConnected, Connecting: node.IsConnecting, Disabled: node.IsDisabled,
		UsersOnline: node.UsersOnline, XrayUptime: time.Duration(node.XrayUptime) * time.Second,
		FetchedAt: p.fetchedAt,
	}
	if node.LastStatusMessage != nil {
		status.Message = boundedMessage(*node.LastStatusMessage)
	}
	if node.Versions != nil {
		status.XrayVersion = strings.TrimSpace(node.Versions.Xray)
	}
	if system := node.System; system != nil {
		status.CPUs = system.Info.CPUs
		total := system.Info.MemoryTotal
		if total <= 0 {
			total = system.Stats.MemoryUsed + system.Stats.MemoryFree
		}
		if total > 0 && system.Stats.MemoryUsed >= 0 {
			status.MemoryUsedPercent = 100 * system.Stats.MemoryUsed / total
		}
		if len(system.Stats.LoadAvg) > 0 {
			status.Load1 = system.Stats.LoadAvg[0]
		}
		if iface := system.Stats.Interface; iface != nil {
			status.RxMbps = iface.RxBytesPerSec * 8 / 1_000_000
			status.TxMbps = iface.TxBytesPerSec * 8 / 1_000_000
		}
	}
	if !before.IsZero() {
		// The latest sample taken before the moment asked about, so a count
		// from the outage itself does not pose as the baseline.
		samples := p.history[node.UUID]
		index := sort.Search(len(samples), func(i int) bool { return !samples[i].at.Before(before) })
		if index > 0 {
			status.UsersOnlineBefore = samples[index-1].online
			status.HasBefore = true
		}
	}
	return status
}

func nodeServes(node Node, server string) bool {
	if normalizeHost(node.Address) == server {
		return true
	}
	address, err := netip.ParseAddr(server)
	if err != nil {
		return false
	}
	for _, ip := range node.IPs {
		if candidate, err := netip.ParseAddr(strings.TrimSpace(ip.IP)); err == nil && candidate.Unmap() == address.Unmap() {
			return true
		}
	}
	return false
}

func normalizeHost(value string) string {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if address, err := netip.ParseAddr(strings.Trim(value, "[]")); err == nil {
		return address.Unmap().String()
	}
	return value
}

// boundedMessage keeps the panel's status line short and on one line: it can
// quote a connection error, which is useful to an operator and has no business
// being a paragraph in an alert.
func boundedMessage(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxStatusMessage {
		return string(runes[:maxStatusMessage-1]) + "…"
	}
	return value
}
