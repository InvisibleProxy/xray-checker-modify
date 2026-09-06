package reachability

import (
	"sort"
	"strings"

	"xray-checker/diagnostics"
)

// Preference ranks vantage points for one node, best first, from what the sweep
// last recorded about that node.
//
// It answers a question the agent registry cannot: the registry knows who is
// connected, which is enough to pick someone free and not enough to pick
// someone whose answer will mean anything. An agent reaches a node along its own
// path, so an agent that has never reached this node is a poor witness to
// whether it is slow — and an agent that reaches it quickly is the closest thing
// to a second measurement of the same thing.
//
// Three classes, in order: agents whose current cell shows they reached the
// node, fastest first; agents with nothing current to say, which includes every
// agent added since the last pass; and agents whose current cell shows they
// could not reach it. That last group is ranked last rather than dropped on
// purpose — an agent that failed an hour ago is still a better vantage point
// than no diagnostic at all, and the caller may have exactly one agent free.
//
// Agents this matrix knows nothing about keep the order they arrived in, so a
// caller that has not swept yet gets its own ordering back unchanged.
func (m *Matrix) PreferAgents(stableID string, agentIDs []string) []string {
	if m == nil || len(agentIDs) == 0 {
		return nil
	}
	stableID = strings.TrimSpace(stableID)

	m.mu.RLock()
	generation := m.sweepGeneration
	row := make(map[string]Cell, len(m.cells[stableID]))
	for agentID, cell := range m.cells[stableID] {
		row[agentID] = cell
	}
	m.mu.RUnlock()

	type candidate struct {
		agentID string
		class   int
		latency int64
		arrival int
	}
	candidates := make([]candidate, 0, len(agentIDs))
	for index, agentID := range agentIDs {
		ranked := candidate{agentID: agentID, class: classUnknown, arrival: index}
		cell, found := row[strings.TrimSpace(agentID)]
		// A cell the last full pass did not refresh describes a moment that has
		// passed. It is not evidence about now in either direction, which is the
		// same rule the matrix applies everywhere else it reads a cell.
		if found && !(generation > 0 && cell.Generation != generation) {
			switch cell.AgentStatus {
			case diagnostics.ProbeStatusOnline:
				ranked.class = classReached
				ranked.latency = cell.LatencyMillis
			case diagnostics.ProbeStatusOffline, diagnostics.ProbeStatusProxyFailure:
				ranked.class = classFailed
			}
		}
		candidates = append(candidates, ranked)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].class != candidates[j].class {
			return candidates[i].class < candidates[j].class
		}
		// A reached cell without a latency reading is still a reached cell, but
		// it cannot claim to be the fastest one, so it sorts behind every cell
		// that measured something.
		if candidates[i].class == classReached && candidates[i].latency != candidates[j].latency {
			if candidates[i].latency == 0 {
				return false
			}
			if candidates[j].latency == 0 {
				return true
			}
			return candidates[i].latency < candidates[j].latency
		}
		return candidates[i].arrival < candidates[j].arrival
	})
	ordered := make([]string, 0, len(candidates))
	for _, ranked := range candidates {
		ordered = append(ordered, ranked.agentID)
	}
	return ordered
}

const (
	classReached = iota
	classUnknown
	classFailed
)
