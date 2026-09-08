// Package speedprobe keeps the automatic agent probe with the measurement that
// caused it.
//
// The evidence already existed: an unresolved speed test could start an
// isolated diagnostic session, and its verdict was written into the Telegram
// alert. But the alert is not where a failure is investigated. It is read once,
// on a phone, and the operator who opens the node a day later — after the
// alert was muted, filtered out, or never sent because Telegram is off — finds
// a failed measurement with nothing beside it.
//
// So the probe is recorded here instead, against the measurement it belongs
// to, and Telegram keeps reading the same coordinator for its own copy. The two
// consumers stay independent: whether an alert goes out decides nothing about
// what the history remembers.
//
// What is recorded stays evidence. This package writes into one field of a
// stored result and touches nothing else — no status, no threshold, no
// counter — so a node reads exactly as it did before the agent answered.
package speedprobe

import (
	"context"
	"reflect"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/logger"
	"xray-checker/speedtest"
)

// DefaultWait bounds how long a probe is followed. A diagnostic job expires
// after remoteprobe's job TTL — five minutes by default — so a session that has
// answered nothing by then never will, and waiting longer only holds a
// goroutine open for an answer that cannot arrive.
const DefaultWait = 6 * time.Minute

// Coordinator is the automatic diagnostics this recorder drives. It is the same
// coordinator Telegram uses: a session started for one is the session the other
// reads, which is what keeps a node from being probed twice for one failure.
type Coordinator interface {
	Enabled() bool
	StartSpeedDiagnostics(speedtest.RunReport, float64) map[string]agentautomation.Handle
	Annotations(map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic
	Await(context.Context, map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic
	AwaitIdle(context.Context, []string)
}

// History is the narrow write this package is allowed. It is deliberately the
// whole surface: a recorder that could reach further into the speed-test
// manager would be a diagnostic result with operational consequences.
type History interface {
	RecordAgentProbe(stableID string, checkedAt time.Time, probe speedtest.AgentDiagnostic) (bool, error)
}

type Config struct {
	// Threshold reports the global low-speed threshold. A result that carries
	// its own threshold is judged by that one instead, so this is only the
	// fallback for measurements taken before per-node thresholds existed.
	Threshold func() float64
	Wait      time.Duration
}

type Recorder struct {
	coordinator Coordinator
	history     History
	threshold   func() float64
	wait        time.Duration
	stop        chan struct{}
}

func New(config Config, coordinator Coordinator, history History) *Recorder {
	if coordinator == nil || history == nil {
		return nil
	}
	if config.Wait <= 0 {
		config.Wait = DefaultWait
	}
	return &Recorder{
		coordinator: coordinator,
		history:     history,
		threshold:   config.Threshold,
		wait:        config.Wait,
		stop:        make(chan struct{}),
	}
}

// Stop releases the waits still in flight. A probe that has not answered by
// shutdown stays in history as it was last written — started, and unfinished —
// which is the truthful record of what happened.
func (r *Recorder) Stop() {
	if r == nil {
		return
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
}

// RunSpeedProbes diagnoses the failed and slow measurements of one finished run.
//
// The probe is written twice on purpose. The first write lands as soon as the
// session exists, so a node opened while the agent is still working shows the
// question it was asked rather than nothing at all; the second replaces it with
// the answer. A record that only appeared at the end would leave the minutes in
// between looking like no probe was ever attempted.
func (r *Recorder) RunSpeedProbes(report speedtest.RunReport) {
	if r == nil || !r.coordinator.Enabled() {
		return
	}
	handles := r.coordinator.StartSpeedDiagnostics(report, r.globalThreshold())
	if len(handles) == 0 {
		return
	}
	measured := measuredAt(report.Results, handles)
	if len(measured) == 0 {
		return
	}

	written := r.record(measured, r.coordinator.Annotations(handles), nil)

	ctx, cancel := context.WithTimeout(context.Background(), r.wait)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	r.record(measured, r.coordinator.Await(ctx, handles), written)
}

// AwaitIdleNodes holds a starting run until the agents have stopped measuring
// the nodes it is about to measure.
//
// An agent probe transfers as much as the run it followed, over the node's own
// uplink, so two of them at once produce two rates and neither describes the
// node. The agent cannot be called off mid-job, which leaves waiting as the only
// way to get a clean measurement. It is bounded by the same window the recorder
// follows a probe for: past the job deadline the agent stops on its own, so
// there is nothing left to wait for.
func (r *Recorder) AwaitIdleNodes(stableIDs []string) {
	if r == nil || !r.coordinator.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.wait)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	startedAt := time.Now()
	r.coordinator.AwaitIdle(ctx, stableIDs)
	// Only a wait long enough to delay the run is worth a line: an operator who
	// pressed Run and waited needs to see why, and a run that started at once
	// has nothing to explain.
	if waited := time.Since(startedAt); waited > time.Second {
		logger.Info("Speed test waited %s for agent probes to finish measuring the same nodes", waited.Round(time.Second))
	}
}

func (r *Recorder) globalThreshold() float64 {
	if r.threshold == nil {
		return 0
	}
	threshold := r.threshold()
	if threshold < 0 {
		return 0
	}
	return threshold
}

// record stores every probe that changed since the previous write and reports
// what is now on record. Rewriting an unchanged probe would persist the whole
// result file again to say nothing new.
func (r *Recorder) record(
	measured map[string]time.Time,
	probes map[string]speedtest.AgentDiagnostic,
	written map[string]speedtest.AgentDiagnostic,
) map[string]speedtest.AgentDiagnostic {
	if len(probes) == 0 {
		return written
	}
	current := make(map[string]speedtest.AgentDiagnostic, len(probes))
	for stableID, probe := range written {
		current[stableID] = probe
	}
	for stableID, probe := range probes {
		checkedAt, ok := measured[stableID]
		if !ok {
			continue
		}
		if previous, seen := current[stableID]; seen && reflect.DeepEqual(previous, probe) {
			continue
		}
		recorded, err := r.history.RecordAgentProbe(stableID, checkedAt, probe)
		if err != nil {
			logger.Warn("Failed to store the agent probe for %s: %v", stableID, err)
			continue
		}
		if !recorded {
			// The measurement is gone: history retention trimmed it, an operator
			// deleted it, or a node merge moved it. Nothing to attach to, and
			// nothing worth an operator's attention.
			continue
		}
		current[stableID] = probe
	}
	return current
}

// measuredAt pairs each diagnosed node with the measurement being diagnosed.
// Candidate selection takes a node's first result in the report, so this does
// too: pairing the probe with a later result would file the agent's answer
// against a measurement it was never asked about.
func measuredAt(results []speedtest.Result, handles map[string]agentautomation.Handle) map[string]time.Time {
	measured := make(map[string]time.Time, len(handles))
	for _, result := range results {
		if _, ok := handles[result.StableID]; !ok {
			continue
		}
		if _, seen := measured[result.StableID]; seen || result.CheckedAt.IsZero() {
			continue
		}
		measured[result.StableID] = result.CheckedAt
	}
	return measured
}
