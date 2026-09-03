package reachability

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
	"xray-checker/remoteprobe"
)

const (
	DefaultInterval     = time.Hour
	DefaultProbeTimeout = 2 * time.Minute
	defaultPollInterval = 500 * time.Millisecond
	// minInterval keeps a misconfiguration from turning the sweep into a
	// continuous load on every agent and every node.
	minInterval = 5 * time.Minute
)

// SessionController is the narrow slice of the remote diagnostic controller the
// sweep needs. Keeping it narrow is not only about tests: it makes visible that
// the sweep can create, read and discard diagnostic sessions, and can do
// nothing else — in particular it has no way to touch availability state.
type SessionController interface {
	Enabled() bool
	CreateSweep(remoteprobe.CreateSweepRequest) (remoteprobe.SessionView, error)
	Session(string) (remoteprobe.SessionView, bool)
	Cancel(string) error
	Delete(string) error
}

// AgentSource lists the vantage points.
type AgentSource interface {
	Snapshot() []probeagent.AgentSnapshot
}

// Target is one node worth asking about.
type Target struct {
	StableID string
	Name     string
}

type Config struct {
	Enabled bool
	// Interval is the gap between the end of one full pass and the start of the
	// next, not a fixed period. A sweep that takes longer than the interval
	// therefore slows down rather than overlapping itself.
	Interval     time.Duration
	ProbeTimeout time.Duration
	ProfileID    string
	PollInterval time.Duration
	Now          func() time.Time
	// OnSweep reports each finished pass. It exists so the caller can log a
	// sweep without this package taking a dependency on the logger, which is
	// what keeps it testable without capturing output.
	OnSweep func(Summary)
}

// Sweeper walks the node list once per agent and records what each agent saw.
//
// Concurrency follows the agents: one goroutine per agent, each visiting nodes
// in order. That is not a throughput choice but a correctness one — the
// controller refuses a second session for a node and agent that already has
// one, and an agent runs a single probe at a time anyway.
type Sweeper struct {
	config     Config
	controller SessionController
	agents     AgentSource
	targets    func() []Target
	matrix     *Matrix

	mu       sync.Mutex
	sweeping bool
	lastDone bool
}

func NewSweeper(config Config, controller SessionController, agents AgentSource, targets func() []Target, matrix *Matrix) (*Sweeper, error) {
	if config.Interval == 0 {
		config.Interval = DefaultInterval
	}
	if config.ProbeTimeout == 0 {
		config.ProbeTimeout = DefaultProbeTimeout
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}
	if strings.TrimSpace(config.ProfileID) == "" {
		// The status probe is the right default: every agent that can run a
		// diagnostic at all advertises it, and the evidence a sweep reads —
		// TCP reachability and the agent's own connectivity control — comes
		// with every observation regardless of which probe produced it.
		config.ProfileID = diagnostics.ProfileStatus
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if controller == nil || agents == nil || targets == nil || matrix == nil {
		return nil, errors.New("reachability sweeper requires a controller, agent source, target source and matrix")
	}
	// The pacing floors are only enforced for a sweep that will actually run.
	// Refusing to start over the shape of a disabled feature would turn an
	// unused setting into an outage.
	if config.Enabled && (config.Interval < minInterval || config.ProbeTimeout < 15*time.Second || config.PollInterval <= 0) {
		return nil, errors.New("invalid reachability sweep configuration")
	}
	return &Sweeper{config: config, controller: controller, agents: agents, targets: targets, matrix: matrix}, nil
}

func (s *Sweeper) Enabled() bool {
	return s != nil && s.config.Enabled && s.controller.Enabled()
}

// Run sweeps until the context is cancelled. The first pass is delayed by one
// interval so a restart does not put every agent to work while the checker is
// still establishing its own first availability results — the local side of
// every comparison would be missing.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	timer := time.NewTimer(s.config.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.SweepOnce(ctx)
		timer.Reset(s.config.Interval)
	}
}

// SweepOnce runs one full pass and reports what it managed to observe.
func (s *Sweeper) SweepOnce(ctx context.Context) Summary {
	return s.sweep(ctx, "")
}

// SweepNode re-asks every agent about a single node.
//
// It exists because confirming or clearing one finding should not cost a full
// pass over every node from every vantage point. It runs the same job, with the
// same trigger and the same profile, so its result is an ordinary observation
// rather than a second class of evidence — and because the streak only advances
// when the checker's own sample has moved on, repeating it cannot manufacture a
// confirmation.
func (s *Sweeper) SweepNode(ctx context.Context, stableID string) Summary {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return Summary{}
	}
	return s.sweep(ctx, stableID)
}

// sweep runs a pass over every node, or over one when stableID is set.
func (s *Sweeper) sweep(ctx context.Context, stableID string) Summary {
	if !s.Enabled() {
		return Summary{}
	}
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return Summary{Skipped: true}
	}
	s.sweeping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
	}()

	full := stableID == ""
	all := s.targets()
	agents := s.eligibleAgents()
	targets := all
	if !full {
		targets = nil
		for _, target := range all {
			if target.StableID == stableID {
				targets = append(targets, target)
			}
		}
		// A node that is gone, paused, or not monitored has nothing to compare
		// an agent's answer against, so there is no pass to run.
		if len(targets) == 0 {
			return Summary{Skipped: true}
		}
	}

	if full {
		names := make(map[string]string, len(all))
		for _, target := range all {
			names[target.StableID] = target.Name
		}
		// Every registered agent, not only the ones that can answer right now:
		// an agent that is merely offline keeps the last thing it saw, and the
		// matrix marks those cells stale instead of deleting them. Only a
		// revoked or removed agent loses its column, because it has stopped
		// being a vantage point at all.
		known := make(map[string]bool)
		for _, agent := range s.agents.Snapshot() {
			if !agent.Revoked {
				known[agent.AgentID] = true
			}
		}
		// Retaining before the pass, not after, keeps a node deleted mid-sweep
		// from being re-added by a result that is already meaningless. Only a
		// full pass may do this: a single-node pass knows nothing about the
		// other rows and would delete every one of them.
		s.matrix.Retain(names, known)
		// Staleness is measured from the start of this pass: whatever it does
		// not refresh from here on is last-known rather than current.
		s.matrix.BeginSweep()
	}
	if len(targets) == 0 || len(agents) == 0 {
		empty := Summary{Agents: len(agents), Nodes: len(targets)}
		empty.SaveError = s.finish(full, false)
		s.report(empty)
		return empty
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		summary = Summary{Agents: len(agents), Nodes: len(targets)}
	)
	for _, agent := range agents {
		wg.Add(1)
		go func(agentID string) {
			defer wg.Done()
			partial := s.sweepAgent(ctx, agentID, targets)
			mu.Lock()
			summary.merge(partial)
			mu.Unlock()
		}(agent.AgentID)
	}
	wg.Wait()

	// Counted now rather than per pair: a cell is only a finding relative to
	// what the other agents saw for the same node, which is known only once the
	// whole pass has landed in the matrix.
	for _, finding := range s.matrix.Findings() {
		summary.Divergent++
		if finding.Confirmed {
			summary.Confirmed++
		}
	}
	completed := ctx.Err() == nil
	summary.SaveError = s.finish(full, completed)
	summary.Completed = completed
	summary.Node = stableID
	s.report(summary)
	return summary
}

func (s *Sweeper) report(summary Summary) {
	if s.config.OnSweep != nil {
		s.config.OnSweep(summary)
	}
}

// finish records the pass and persists the matrix. A failed save is reported
// but never aborts anything: the matrix is a cache of observations that the
// next sweep rebuilds, so the only cost is losing one pass across a restart.
//
// Only a full pass moves the "last sweep" marker. A single-node recheck says
// nothing about the rest of the matrix, and letting it claim otherwise would
// make stale rows read as freshly confirmed.
func (s *Sweeper) finish(full, completed bool) error {
	if full {
		s.matrix.MarkSweep(s.config.Now().UTC())
		s.mu.Lock()
		s.lastDone = completed
		s.mu.Unlock()
	}
	return s.matrix.Save()
}

// sweepAgent visits every node from one vantage point, in order.
func (s *Sweeper) sweepAgent(ctx context.Context, agentID string, targets []Target) Summary {
	summary := Summary{}
	for _, target := range targets {
		if ctx.Err() != nil {
			return summary
		}
		outcome, stop := s.sweepPair(ctx, agentID, target)
		summary.merge(outcome)
		if stop {
			// The agent went away or the whole workflow is paused. Continuing
			// would spend one refusal per remaining node and record nothing.
			return summary
		}
	}
	return summary
}

// sweepPair asks one agent about one node. The bool reports whether the rest of
// this agent's pass should be abandoned.
func (s *Sweeper) sweepPair(ctx context.Context, agentID string, target Target) (Summary, bool) {
	view, err := s.controller.CreateSweep(remoteprobe.CreateSweepRequest{
		StableID:  target.StableID,
		AgentID:   agentID,
		ProfileID: s.config.ProfileID,
	})
	if err != nil {
		switch {
		case errors.Is(err, remoteprobe.ErrUnavailableAgent), errors.Is(err, probeagent.ErrAgentNotFound), errors.Is(err, probeagent.ErrDisabled):
			return Summary{}, true
		case errors.Is(err, remoteprobe.ErrAutomaticPaused):
			// Project maintenance, or this node is paused. Maintenance is
			// project-wide often enough that abandoning the pass is the right
			// guess; a per-node pause costs one wasted attempt on the next
			// sweep, which is cheaper than checking maintenance state here and
			// duplicating the controller's rule.
			return Summary{Skipped: true}, true
		default:
			// A node the controller cannot build a config for, or a pair that
			// already has a session. Existing cells are deliberately left
			// alone: overwriting a real finding with "we could not ask" would
			// erase the finding and its Since.
			return Summary{Errors: 1}, false
		}
	}

	sessionID := view.Session.SessionID
	session, ok := s.await(ctx, sessionID)
	if !ok {
		_ = s.controller.Cancel(sessionID)
		_ = s.controller.Delete(sessionID)
		return Summary{Timeouts: 1}, false
	}

	s.matrix.Record(target.StableID, CellFor(session, agentID, s.config.Now()))
	// The session has served its purpose. Deleting it keeps the sweep from
	// filling the operator's diagnostics list with hundreds of entries and
	// bounds the manager's in-memory state, while the cell keeps the part worth
	// remembering.
	_ = s.controller.Delete(sessionID)

	// Findings are deliberately not counted here. Whether this cell is a
	// finding depends on what the other agents saw for the same node, and the
	// pass has not finished collecting that yet.
	return Summary{Recorded: 1}, false
}

func (s *Sweeper) await(ctx context.Context, sessionID string) (diagnostics.DiagnosticSession, bool) {
	deadline := time.NewTimer(s.config.ProbeTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		view, ok := s.controller.Session(sessionID)
		if !ok {
			return diagnostics.DiagnosticSession{}, false
		}
		if view.Session.State.Terminal() {
			return view.Session, true
		}
		select {
		case <-ctx.Done():
			return diagnostics.DiagnosticSession{}, false
		case <-deadline.C:
			return diagnostics.DiagnosticSession{}, false
		case <-ticker.C:
		}
	}
}

// eligibleAgents returns the vantage points that can answer, sorted so the
// matrix columns keep a stable order between passes.
func (s *Sweeper) eligibleAgents() []probeagent.AgentSnapshot {
	all := s.agents.Snapshot()
	result := make([]probeagent.AgentSnapshot, 0, len(all))
	for _, agent := range all {
		if !agent.Enabled || agent.Revoked || !agent.Connected || agent.Health != "healthy" {
			continue
		}
		if !hasCapability(agent.Capabilities, diagnostics.CapabilityControlV1) ||
			!hasCapability(agent.Capabilities, diagnostics.CapabilityDiagnosticV1) {
			continue
		}
		result = append(result, agent)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AgentID < result[j].AgentID })
	return result
}

func hasCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Summary is what one pass managed to do. It exists so the caller can log a
// pass in one line instead of per cell.
type Summary struct {
	// Node names the single node a targeted recheck covered, and is empty for a
	// full pass. Without it a one-line log cannot say whether "1 node" means the
	// matrix has one node or that only one was rechecked.
	Node      string
	Agents    int
	Nodes     int
	Recorded  int
	Divergent int
	Confirmed int
	Timeouts  int
	Errors    int
	Skipped   bool
	Completed bool
	SaveError error
}

func (s *Summary) merge(other Summary) {
	s.Recorded += other.Recorded
	s.Divergent += other.Divergent
	s.Confirmed += other.Confirmed
	s.Timeouts += other.Timeouts
	s.Errors += other.Errors
	s.Skipped = s.Skipped || other.Skipped
}

// Snapshot renders the matrix for the API, pairing the stored cells with the
// current agent list so a column exists for an agent that has not answered yet.
func (s *Sweeper) Snapshot() View {
	if s == nil {
		return View{}
	}
	s.mu.Lock()
	sweeping, lastDone := s.sweeping, s.lastDone
	s.mu.Unlock()

	agents := s.agents.Snapshot()
	sort.Slice(agents, func(i, j int) bool { return agents[i].AgentID < agents[j].AgentID })
	columns := make([]Agent, 0, len(agents))
	for _, agent := range agents {
		if agent.Revoked {
			continue
		}
		columns = append(columns, Agent{
			AgentID:      agent.AgentID,
			DisplayName:  agent.DisplayName,
			Region:       agent.Region,
			Provider:     agent.Provider,
			NetworkGroup: agent.NetworkGroup,
			Connected:    agent.Connected && agent.Enabled,
		})
	}
	// The name belongs to the subscription, not to the matrix. Resolving it here
	// rather than trusting the stored copy means a renamed node reads correctly
	// at once, and a matrix restored from disk is labelled before the first
	// sweep has had a chance to refresh anything.
	live := make(map[string]string)
	for _, target := range s.targets() {
		if target.Name != "" {
			live[target.StableID] = target.Name
		}
	}
	rows := s.matrix.Rows()
	for i := range rows {
		if name := live[rows[i].StableID]; name != "" {
			rows[i].Name = name
		}
	}
	findings := s.matrix.Findings()
	for i := range findings {
		if name := live[findings[i].StableID]; name != "" {
			findings[i].Name = name
		}
	}
	confirmed := 0
	for _, finding := range findings {
		if finding.Confirmed {
			confirmed++
		}
	}
	return View{
		Findings:      findings,
		Enabled:       s.Enabled(),
		IntervalSec:   int(s.config.Interval / time.Second),
		ProfileID:     s.config.ProfileID,
		Agents:        columns,
		Nodes:         rows,
		LastSweepAt:   s.matrix.LastSweepAt(),
		LastSweepDone: lastDone,
		Sweeping:      sweeping,
		Divergent:     confirmed,
	}
}
