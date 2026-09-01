package web

import (
	"net/http"
	"sync"
	"time"
)

// Subscription refresh phases, in the order they run. They exist so a refresh
// that takes a while can say which part is taking it, both while it runs and
// afterwards: "slow" is not actionable, "the panel took 40s to answer" is.
const (
	RefreshPhaseFetching  = "fetching"
	RefreshPhaseResolving = "resolving"
	RefreshPhaseComparing = "comparing"
	RefreshPhaseApplying  = "applying"
	RefreshPhaseFinishing = "finishing"
)

type RefreshPhaseTiming struct {
	Phase      string `json:"phase"`
	DurationMs int64  `json:"durationMs"`
}

type AdminSubscriptionRefreshProgress struct {
	Running    bool                 `json:"running"`
	Source     string               `json:"source,omitempty"`
	Phase      string               `json:"phase,omitempty"`
	ElapsedMs  int64                `json:"elapsedMs"`
	StartedAt  string               `json:"startedAt,omitempty"`
	FinishedAt string               `json:"finishedAt,omitempty"`
	Failed     bool                 `json:"failed,omitempty"`
	Phases     []RefreshPhaseTiming `json:"phases,omitempty"`
}

// SubscriptionRefreshTracker records which phase a refresh is in. It is written
// by the refresh itself and read by polling, so it never takes part in the
// refresh decision and a reader can never block one.
type SubscriptionRefreshTracker struct {
	mu         sync.RWMutex
	now        func() time.Time
	running    bool
	source     string
	phase      string
	startedAt  time.Time
	phaseStart time.Time
	finishedAt time.Time
	failed     bool
	phases     []RefreshPhaseTiming
}

func NewSubscriptionRefreshTracker() *SubscriptionRefreshTracker {
	return &SubscriptionRefreshTracker{now: time.Now}
}

func (t *SubscriptionRefreshTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Begin discards the previous run's timings: only the newest refresh is of
// interest, and keeping older ones would suggest they describe the current one.
func (t *SubscriptionRefreshTracker) Begin(source string) {
	if t == nil {
		return
	}
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = true
	t.source = source
	t.phase = ""
	t.startedAt = now
	t.phaseStart = now
	t.finishedAt = time.Time{}
	t.failed = false
	t.phases = nil
}

// Phase closes the previous phase and opens the named one.
func (t *SubscriptionRefreshTracker) Phase(phase string) {
	if t == nil {
		return
	}
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closePhaseLocked(now)
	t.phase = phase
	t.phaseStart = now
}

func (t *SubscriptionRefreshTracker) closePhaseLocked(now time.Time) {
	if t.phase == "" {
		return
	}
	t.phases = append(t.phases, RefreshPhaseTiming{
		Phase:      t.phase,
		DurationMs: maxDuration(now.Sub(t.phaseStart)),
	})
}

func (t *SubscriptionRefreshTracker) Done(failed bool) {
	if t == nil {
		return
	}
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closePhaseLocked(now)
	t.phase = ""
	t.running = false
	t.failed = failed
	t.finishedAt = now
}

func (t *SubscriptionRefreshTracker) Snapshot() AdminSubscriptionRefreshProgress {
	if t == nil {
		return AdminSubscriptionRefreshProgress{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	progress := AdminSubscriptionRefreshProgress{
		Running: t.running,
		Source:  t.source,
		Phase:   t.phase,
		Failed:  t.failed,
		Phases:  append([]RefreshPhaseTiming(nil), t.phases...),
	}
	if t.startedAt.IsZero() {
		return progress
	}
	progress.StartedAt = t.startedAt.UTC().Format(time.RFC3339)
	end := t.finishedAt
	if t.running {
		end = t.clock()
	}
	progress.ElapsedMs = maxDuration(end.Sub(t.startedAt))
	if !t.finishedAt.IsZero() {
		progress.FinishedAt = t.finishedAt.UTC().Format(time.RFC3339)
	}
	return progress
}

func maxDuration(value time.Duration) int64 {
	if value < 0 {
		return 0
	}
	return value.Milliseconds()
}

func AdminSubscriptionRefreshProgressHandler(tracker *SubscriptionRefreshTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, tracker.Snapshot())
	}
}
