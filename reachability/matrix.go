// Package reachability answers a question the controller cannot answer alone:
// is a node reachable from somewhere other than here?
//
// The checker's availability loop observes every node from a single vantage
// point. That is enough to tell a dead node from a live one, but not enough to
// tell a dead node from one that is merely unreachable along one path. Those
// look identical locally and are opposite problems: the first is the operator's
// to fix, the second is a network between a user and a node that is perfectly
// healthy.
//
// A sweep asks each connected probe agent the same question about each node and
// keeps the last answer per pair. The interesting output is not a cell but a
// disagreement: a node that answers here and not there, or the reverse.
//
// Nothing here writes operational state. The matrix never changes Availability,
// downtime, incidents, speed-test scheduling or Remnawave, and no verdict feeds
// back into the checker. It is a second opinion an operator reads, which is the
// same boundary every other remote observation in this project respects.
package reachability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/diagnostics"
)

const StateVersion = 1

// Verdict names what the two vantage points said, and deliberately stops there.
// "Unreachable from that agent" is an observation; "blocked" would be a theory
// about why, and the evidence in a single cell cannot distinguish censorship
// from a routing fault, a provider null-route or a node that refuses one
// network. FailureCode and FailureStage carry what is known about the flavour.
type Verdict string

const (
	// VerdictUnknown covers every case where the pair produced no comparable
	// answer: the job expired, the session was cancelled, or the agent's own
	// connectivity control failed. It is not a synonym for "not yet swept" —
	// a node with no cell at all is simply absent from the matrix.
	VerdictUnknown Verdict = "unknown"
	// VerdictAgreedUp and VerdictAgreedDown are the uninteresting majority.
	VerdictAgreedUp   Verdict = "agreed_up"
	VerdictAgreedDown Verdict = "agreed_down"
	// VerdictAgentOnlyFailure is the case the sweep exists for: the node
	// answers the controller and refuses this agent.
	VerdictAgentOnlyFailure Verdict = "agent_only_failure"
	// VerdictLocalOnlyFailure is the mirror image, and just as useful: the node
	// answers the agent and refuses the controller, which means the fault is on
	// the controller's own path and not in the node.
	VerdictLocalOnlyFailure Verdict = "local_only_failure"
)

// Divergent reports whether the two vantage points disagreed. Only a
// disagreement is worth an operator's attention; agreement, in either
// direction, is already visible in the ordinary availability view.
func (v Verdict) Divergent() bool {
	return v == VerdictAgentOnlyFailure || v == VerdictLocalOnlyFailure
}

// Cell is the last comparable answer for one node and one agent.
type Cell struct {
	AgentID string  `json:"agentId"`
	Verdict Verdict `json:"verdict"`
	// AgentStatus and LocalStatus are kept verbatim rather than folded into the
	// verdict, because "proxy_failure here, offline there" and "offline both
	// ways" are the same verdict and different problems.
	AgentStatus diagnostics.ProbeStatus `json:"agentStatus"`
	LocalStatus diagnostics.ProbeStatus `json:"localStatus"`
	CheckedAt   time.Time               `json:"checkedAt"`
	// LocalCheckedAt is when the controller's side of the comparison was
	// sampled. A divergence against a stale local result is worth less than one
	// against a fresh sample, and only this field makes that visible.
	LocalCheckedAt time.Time                `json:"localCheckedAt,omitempty"`
	LatencyMillis  int64                    `json:"latencyMillis,omitempty"`
	FailureCode    string                   `json:"failureCode,omitempty"`
	FailureStage   diagnostics.FailureStage `json:"failureStage,omitempty"`
	// TCPReached separates a path that carries no packets at all from one that
	// completes a TCP handshake and then fails inside the tunnel. The first
	// points at the network in front of the node, the second at the node.
	TCPReached bool `json:"tcpReached"`
	// Detail explains an unknown verdict. It is a fixed phrase chosen from this
	// package, never a transport error, so nothing unbounded reaches the state
	// file or the API.
	Detail string `json:"detail,omitempty"`
	// Since is when this verdict was first observed. It survives repeated
	// sweeps that reach the same conclusion, which is what turns a cell into
	// "unreachable from Frankfurt for the last two hours".
	Since time.Time `json:"since"`
	// Streak counts consecutive sweeps that reached this verdict. It is the
	// hysteresis: see Confirmed for why a single disagreement is not a finding.
	Streak int `json:"streak"`
	// Unreachable marks a vantage point that could not reach a node some other
	// vantage point proved alive in the same sweep. It is derived from the whole
	// row when the matrix is read, never stored.
	//
	// It exists because Verdict compares each agent against the checker, and
	// that baseline is wrong exactly when the checker is the odd one out: a node
	// the checker cannot reach and one agent can is "agreed_down" for every
	// other agent that also cannot reach it, which buries the finding those
	// agents are reporting.
	Unreachable bool `json:"unreachable,omitempty"`
	// Stale marks an observation the last full pass did not refresh, which is
	// what an agent that goes away leaves behind. The cell is kept because the
	// last thing a vantage point saw is still worth reading, but it stops being
	// evidence about now: it takes no part in Alive, Unreachable or findings.
	// Derived on read like Unreachable, never stored.
	Stale bool `json:"stale,omitempty"`
	// Generation is the full pass that wrote this cell. Stored, unlike Stale,
	// because it is the fact from which staleness is derived.
	Generation int `json:"generation,omitempty"`
}

// NodeRow is one node's answers, ordered by agent for stable presentation.
type NodeRow struct {
	StableID string `json:"stableId"`
	Name     string `json:"name,omitempty"`
	Cells    []Cell `json:"cells"`
	// Alive reports that at least one vantage point — the checker or any agent —
	// reached the node. It is what turns another vantage point's failure into a
	// finding rather than an ordinary outage everybody agrees on.
	Alive bool `json:"alive"`
	// LocalUnreachable marks the checker as the vantage point that cannot reach
	// a live node. It is a property of the row, not of any one agent: reporting
	// it per cell would repeat the same fact once per agent that can reach it.
	LocalUnreachable bool `json:"localUnreachable,omitempty"`
	// LocalSince and LocalStreak describe how long that has been true, taken
	// from the longest-running cell that observed it.
	LocalSince  time.Time `json:"localSince,omitempty"`
	LocalStreak int       `json:"localStreak,omitempty"`
}

// View is the whole matrix as the API and the admin UI consume it.
type View struct {
	Enabled     bool      `json:"enabled"`
	IntervalSec int       `json:"intervalSeconds"`
	ProfileID   string    `json:"profileId"`
	Agents      []Agent   `json:"agents"`
	Nodes       []NodeRow `json:"nodes"`
	// Findings is the derived list the operator actually acts on: every vantage
	// point, checker included, that cannot reach a node another vantage point
	// proved alive.
	Findings      []Finding `json:"findings"`
	LastSweepAt   time.Time `json:"lastSweepAt,omitempty"`
	LastSweepDone bool      `json:"lastSweepCompleted"`
	Sweeping      bool      `json:"sweeping"`
	// Divergent counts only confirmed disagreements, so a caller can decide
	// whether the matrix is worth opening without reading every cell, and does
	// not act on a single unrepeated sample.
	Divergent int `json:"divergentCells"`
}

// Agent is the column header: enough to label a vantage point without
// republishing the agent registry.
type Agent struct {
	AgentID      string `json:"agentId"`
	DisplayName  string `json:"displayName,omitempty"`
	Region       string `json:"region,omitempty"`
	Provider     string `json:"provider,omitempty"`
	NetworkGroup string `json:"networkGroup,omitempty"`
	Connected    bool   `json:"connected"`
}

type stateFile struct {
	Version     int       `json:"version"`
	UpdatedAt   time.Time `json:"updatedAt"`
	LastSweepAt time.Time `json:"lastSweepAt,omitempty"`
	// SweepGeneration counts full passes. A cell carries the generation that
	// wrote it, so "did the last pass refresh this?" is an equality check rather
	// than a comparison of timestamps that were taken moments apart. Absent in
	// files written before staleness was tracked, which reads as zero and marks
	// nothing stale until the first pass of the new build.
	SweepGeneration int                        `json:"sweepGeneration,omitempty"`
	Nodes           map[string]map[string]Cell `json:"nodes"`
	// Names labels the rows. Without it a restart shows a matrix of StableIDs
	// until the next full sweep repopulates them, which is a whole interval of
	// unreadable output. It is a cache of the subscription's own names, so a
	// live one always wins over it.
	Names map[string]string `json:"names,omitempty"`
}

// Matrix owns the cells. It is safe for concurrent use: the sweeper writes from
// one goroutine per agent while the admin API reads.
type Matrix struct {
	path string
	now  func() time.Time

	mu              sync.RWMutex
	cells           map[string]map[string]Cell
	names           map[string]string
	lastSweepAt     time.Time
	sweepGeneration int
}

func NewMatrix(path string) *Matrix {
	return &Matrix{
		path:  path,
		now:   time.Now,
		cells: make(map[string]map[string]Cell),
		names: make(map[string]string),
	}
}

func (m *Matrix) Load() error {
	if m == nil || m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode reachability state: %w", err)
	}
	if state.Version != StateVersion {
		return fmt.Errorf("unsupported reachability state version %d", state.Version)
	}
	cells := make(map[string]map[string]Cell, len(state.Nodes))
	for stableID, row := range state.Nodes {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		kept := make(map[string]Cell, len(row))
		for agentID, cell := range row {
			agentID = strings.TrimSpace(agentID)
			// A cell whose agent or verdict did not survive encoding is dropped
			// rather than repaired: a matrix that invents a verdict is worse
			// than one with a hole, because the hole is visible.
			if agentID == "" || !knownVerdict(cell.Verdict) {
				continue
			}
			cell.AgentID = agentID
			kept[agentID] = cell
		}
		if len(kept) > 0 {
			cells[stableID] = kept
		}
	}
	names := make(map[string]string, len(state.Names))
	for stableID, name := range state.Names {
		// Only for rows that survived: a name without cells labels nothing.
		if _, ok := cells[strings.TrimSpace(stableID)]; ok {
			names[strings.TrimSpace(stableID)] = name
		}
	}
	m.mu.Lock()
	m.cells = cells
	m.names = names
	m.lastSweepAt = state.LastSweepAt
	m.sweepGeneration = state.SweepGeneration
	m.mu.Unlock()
	return nil
}

func knownVerdict(value Verdict) bool {
	switch value {
	case VerdictUnknown, VerdictAgreedUp, VerdictAgreedDown, VerdictAgentOnlyFailure, VerdictLocalOnlyFailure:
		return true
	default:
		return false
	}
}

// Record stores one observation, preserving Since when the verdict is unchanged.
func (m *Matrix) Record(stableID string, cell Cell) Cell {
	stableID = strings.TrimSpace(stableID)
	cell.AgentID = strings.TrimSpace(cell.AgentID)
	if m == nil || stableID == "" || cell.AgentID == "" {
		return cell
	}
	if cell.CheckedAt.IsZero() {
		cell.CheckedAt = m.currentTime()
	}
	cell.CheckedAt = cell.CheckedAt.UTC()
	m.mu.Lock()
	cell.Generation = m.sweepGeneration
	row, ok := m.cells[stableID]
	if !ok {
		row = make(map[string]Cell)
		m.cells[stableID] = row
	}
	if previous, exists := row[cell.AgentID]; exists && previous.Verdict == cell.Verdict && !previous.Since.IsZero() {
		cell.Since = previous.Since
		// The streak counts independent observations, not repeated ones. Two
		// observations compared against the same local sample are one piece of
		// evidence read twice, and the streak exists precisely to outlast a
		// stale local result — re-reading that same stale result cannot do it.
		//
		// This is also what makes an operator's targeted recheck safe: pressing
		// it repeatedly cannot confirm a finding that the periodic sweep would
		// not have confirmed on its own.
		if cell.LocalCheckedAt.After(previous.LocalCheckedAt) {
			cell.Streak = previous.Streak + 1
		} else {
			cell.Streak = previous.Streak
		}
	} else {
		cell.Since = cell.CheckedAt
		cell.Streak = 1
	}
	row[cell.AgentID] = cell
	m.mu.Unlock()
	return cell
}

// BeginSweep opens a new full pass. Cells that this pass does not rewrite keep
// the previous generation, which is how an agent that stopped answering is told
// apart from one that answered a moment ago.
func (m *Matrix) BeginSweep() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sweepGeneration++
	m.mu.Unlock()
}

// Retain drops rows and cells for nodes and agents that no longer exist, and
// remembers the display names so a row can be labelled without the caller
// holding the proxy list. Without it a deleted node keeps a stale verdict
// forever, which reads as a live finding.
//
// The agent set is every agent still registered, not the ones that can answer
// right now. An agent that is merely offline keeps its cells: the last thing it
// saw is the only record of that vantage point, and deleting it would erase the
// history exactly when an operator wants to look at it. Rows marks those cells
// stale so they stop counting as evidence.
func (m *Matrix) Retain(nodes map[string]string, agents map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for stableID, row := range m.cells {
		if _, ok := nodes[stableID]; !ok {
			delete(m.cells, stableID)
			continue
		}
		for agentID := range row {
			if !agents[agentID] {
				delete(row, agentID)
			}
		}
		if len(row) == 0 {
			delete(m.cells, stableID)
		}
	}
	m.names = make(map[string]string, len(nodes))
	for stableID, name := range nodes {
		m.names[stableID] = name
	}
}

// MarkSweep records when a full pass finished, which is what tells an operator
// whether an empty matrix means "all good" or "never ran".
func (m *Matrix) MarkSweep(at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.lastSweepAt = at.UTC()
	m.mu.Unlock()
}

// Rows returns the matrix ordered by node then agent.
func (m *Matrix) Rows() []NodeRow {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stableIDs := make([]string, 0, len(m.cells))
	for stableID := range m.cells {
		stableIDs = append(stableIDs, stableID)
	}
	sort.Strings(stableIDs)
	generation := m.sweepGeneration
	rows := make([]NodeRow, 0, len(stableIDs))
	for _, stableID := range stableIDs {
		row := m.cells[stableID]
		cells := make([]Cell, 0, len(row))
		for _, cell := range row {
			cell.Stale = generation > 0 && cell.Generation != generation
			cells = append(cells, cell)
		}
		sort.Slice(cells, func(i, j int) bool { return cells[i].AgentID < cells[j].AgentID })
		rows = append(rows, deriveRow(NodeRow{StableID: stableID, Name: m.names[stableID], Cells: cells}))
	}
	return rows
}

// deriveRow answers the question a single cell cannot: was this node alive
// anywhere during the sweep? Once one vantage point has reached it, every other
// vantage point that could not is reporting a reachability problem — including
// the checker itself, and including agents whose pairwise verdict says the
// checker agreed with them.
func deriveRow(row NodeRow) NodeRow {
	localUp := false
	for _, cell := range row.Cells {
		// An unknown cell is not evidence in either direction: its agent could
		// not answer, or could not vouch for its own connectivity. A stale one
		// describes a moment that has passed, so it says nothing about now
		// either — otherwise an agent that left would keep its last failure
		// alive as a finding forever.
		if cell.Verdict == VerdictUnknown || cell.Stale {
			continue
		}
		if cell.LocalStatus == diagnostics.ProbeStatusOnline {
			localUp = true
		}
		if cell.AgentStatus == diagnostics.ProbeStatusOnline {
			row.Alive = true
		}
	}
	row.Alive = row.Alive || localUp
	if !row.Alive {
		return row
	}
	for i, cell := range row.Cells {
		if cell.Verdict == VerdictUnknown || cell.Stale {
			continue
		}
		row.Cells[i].Unreachable = cell.AgentStatus != diagnostics.ProbeStatusOnline
		if localUp || cell.LocalStatus == diagnostics.ProbeStatusOnline {
			continue
		}
		// The checker cannot reach a node an agent just reached. Keep the
		// longest-running observation of that, so the row reports how long it
		// has been true rather than when it was last re-observed.
		row.LocalUnreachable = true
		if cell.Streak > row.LocalStreak {
			row.LocalStreak = cell.Streak
		}
		if row.LocalSince.IsZero() || cell.Since.Before(row.LocalSince) {
			row.LocalSince = cell.Since
		}
	}
	return row
}

// Finding is one vantage point that could not reach a node another vantage
// point proved alive. The checker is a vantage point like any other, which is
// why AgentID can be empty.
type Finding struct {
	StableID string `json:"stableId"`
	Name     string `json:"name,omitempty"`
	// AgentID is empty when the vantage point is the checker itself.
	AgentID      string                   `json:"agentId,omitempty"`
	Local        bool                     `json:"local"`
	Status       diagnostics.ProbeStatus  `json:"status"`
	FailureCode  string                   `json:"failureCode,omitempty"`
	FailureStage diagnostics.FailureStage `json:"failureStage,omitempty"`
	TCPReached   bool                     `json:"tcpReached"`
	Since        time.Time                `json:"since"`
	Streak       int                      `json:"streak"`
	Confirmed    bool                     `json:"confirmed"`
}

// Findings lists every vantage point that cannot reach a live node, confirmed
// ones first and then newest. This is the shape a summary wants; Rows is the
// shape a table wants.
//
// The checker contributes at most one entry per node. Emitting it per cell
// would repeat the same fact once for every agent that can reach the node.
func (m *Matrix) Findings() []Finding {
	if m == nil {
		return nil
	}
	var result []Finding
	for _, row := range m.Rows() {
		if !row.Alive {
			continue
		}
		if row.LocalUnreachable {
			result = append(result, Finding{
				StableID: row.StableID, Name: row.Name, Local: true,
				Status: localStatusOf(row), Since: row.LocalSince, Streak: row.LocalStreak,
				Confirmed: row.LocalStreak >= confirmSweeps,
			})
		}
		for _, cell := range row.Cells {
			if !cell.Unreachable {
				continue
			}
			result = append(result, Finding{
				StableID: row.StableID, Name: row.Name, AgentID: cell.AgentID,
				Status: cell.AgentStatus, FailureCode: cell.FailureCode, FailureStage: cell.FailureStage,
				TCPReached: cell.TCPReached, Since: cell.Since, Streak: cell.Streak,
				Confirmed: cell.Confirmed(),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Confirmed != result[j].Confirmed {
			return result[i].Confirmed
		}
		if !result[i].Since.Equal(result[j].Since) {
			return result[i].Since.After(result[j].Since)
		}
		if result[i].StableID != result[j].StableID {
			return result[i].StableID < result[j].StableID
		}
		return result[i].AgentID < result[j].AgentID
	})
	return result
}

// localStatusOf reads the checker's own status, which every cell in the row
// carries identically.
func localStatusOf(row NodeRow) diagnostics.ProbeStatus {
	for _, cell := range row.Cells {
		if cell.Verdict != VerdictUnknown && !cell.Stale {
			return cell.LocalStatus
		}
	}
	return diagnostics.ProbeStatusUnknown
}

func (m *Matrix) LastSweepAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastSweepAt
}

func (m *Matrix) currentTime() time.Time {
	if m != nil && m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

// Save persists the matrix. It is called once per sweep rather than per cell:
// the state is a cache of observations that can be rebuilt by sweeping again,
// so losing the last pass to a crash costs one interval, not correctness.
func (m *Matrix) Save() error {
	if m == nil || m.path == "" {
		return nil
	}
	m.mu.RLock()
	state := stateFile{
		Version:         StateVersion,
		UpdatedAt:       m.currentTime(),
		LastSweepAt:     m.lastSweepAt,
		SweepGeneration: m.sweepGeneration,
		Nodes:           make(map[string]map[string]Cell, len(m.cells)),
		Names:           make(map[string]string, len(m.names)),
	}
	for stableID, row := range m.cells {
		copied := make(map[string]Cell, len(row))
		for agentID, cell := range row {
			copied[agentID] = cell
		}
		state.Nodes[stableID] = copied
		if name := m.names[stableID]; name != "" {
			state.Names[stableID] = name
		}
	}
	m.mu.RUnlock()
	return writeStateFile(m.path, state)
}

func writeStateFile(path string, state stateFile) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode reachability state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create reachability state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".reachability-*.tmp")
	if err != nil {
		return fmt.Errorf("create reachability state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set reachability state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write reachability state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close reachability state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace reachability state: %w", err)
	}
	return nil
}
