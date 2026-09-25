// Package verdictlog keeps the verdicts of agent diagnostics after the sessions
// that produced them are gone.
//
// A diagnostic session lives in memory and is evicted within hours; the speed
// probes are copied next to the measurement that asked for them, but nothing
// kept the answer to a proxy failure or an unreachable node, and nothing let an
// operator ask how often an alert turned out to be the checker's own path. This
// journal is that record: one line per final verdict, in its own file.
//
// It is evidence, not state. Nothing operational reads it, it is not part of a
// backup, and losing it loses history only. Its lines carry identifiers,
// bounded codes and rates — never a URL, a node config or a raw error — so it is
// as safe to export as the admin API that serves it.
package verdictlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRetention = 90 * 24 * time.Hour
	// DefaultMaxBytes caps the file between compactions. A verdict line is a
	// few hundred bytes, so this is years of a small fleet's alerts; the cap is
	// there for a runaway, not for ordinary use.
	DefaultMaxBytes = 16 * 1024 * 1024
	// compactEvery bounds how many lines are appended before retention is
	// applied again, so a long-running process does not wait for a restart to
	// forget what it should.
	compactEvery = 500
	maxTextRunes = 200
	// maxQueryLimit bounds one API answer.
	maxQueryLimit     = 2000
	defaultQueryLimit = 200
)

// Sources of a verdict.
const (
	SourceSpeed        = "speed"
	SourceAvailability = "availability"
	SourceSweep        = "sweep"
)

// Entry is one final verdict. Empty fields are omitted, so a line only carries
// what the diagnostic that wrote it actually knew.
type Entry struct {
	// At is when the verdict became final.
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	// Trigger is the diagnostic trigger; Kind and Outcome say which automation
	// asked and what it was asking about.
	Trigger   string `json:"trigger,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	StableID  string `json:"stableId"`
	Node      string `json:"node,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Region    string `json:"region,omitempty"`
	Provider  string `json:"provider,omitempty"`
	// Verdict is the comparison's answer, in the vocabulary of the source that
	// produced it; Detail is its fixed explanation.
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
	// The checker's side of the comparison.
	LocalStatus      string    `json:"localStatus,omitempty"`
	LocalFailureCode string    `json:"localFailureCode,omitempty"`
	LocalMbps        float64   `json:"localMbps,omitempty"`
	ThresholdMbps    float64   `json:"thresholdMbps,omitempty"`
	LocalServerID    string    `json:"localServerId,omitempty"`
	LocalAt          time.Time `json:"localAt,omitempty"`
	// The agent's side.
	AgentStatus       string    `json:"agentStatus,omitempty"`
	AgentFailureCode  string    `json:"agentFailureCode,omitempty"`
	AgentFailureStage string    `json:"agentFailureStage,omitempty"`
	AgentMbps         int64     `json:"agentMbps,omitempty"`
	AgentServerID     string    `json:"agentServerId,omitempty"`
	AgentLatencyMs    int64     `json:"agentLatencyMs,omitempty"`
	AgentTCPReached   *bool     `json:"agentTcpReached,omitempty"`
	AgentAt           time.Time `json:"agentAt,omitempty"`
}

// Recorder is the write side. The automations hold it, so a nil journal —
// a deployment that did not configure one — is simply one that forgets.
type Recorder interface {
	Record(Entry) error
}

// Journal is an append-only JSON-lines file with retention.
type Journal struct {
	path      string
	retention time.Duration
	maxBytes  int64
	now       func() time.Time

	mu       sync.Mutex
	appended int
}

type Config struct {
	Path      string
	Retention time.Duration
	MaxBytes  int64
	Now       func() time.Time
}

func New(config Config) (*Journal, error) {
	config.Path = strings.TrimSpace(config.Path)
	if config.Path == "" {
		return nil, errors.New("verdict journal path is required")
	}
	if config.Retention <= 0 {
		config.Retention = DefaultRetention
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = DefaultMaxBytes
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Journal{path: config.Path, retention: config.Retention, maxBytes: config.MaxBytes, now: config.Now}, nil
}

// Record appends one verdict. It is safe for concurrent use.
func (j *Journal) Record(entry Entry) error {
	if j == nil {
		return nil
	}
	entry = bounded(entry)
	if entry.StableID == "" || entry.Verdict == "" || entry.Source == "" {
		return errors.New("verdict entry needs a node, a source and a verdict")
	}
	if entry.At.IsZero() {
		entry.At = j.now()
	}
	entry.At = entry.At.UTC()
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode verdict: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return fmt.Errorf("create verdict journal directory: %w", err)
	}
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open verdict journal: %w", err)
	}
	_, writeErr := file.Write(append(line, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("append verdict: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close verdict journal: %w", closeErr)
	}
	j.appended++
	if j.appended >= compactEvery {
		return j.compactLocked()
	}
	return nil
}

// Filter narrows a query. Zero values match everything.
type Filter struct {
	StableID string
	Source   string
	Verdict  string
	From     time.Time
	To       time.Time
	Limit    int
}

// Query returns matching verdicts, newest first.
func (j *Journal) Query(filter Filter) ([]Entry, error) {
	if j == nil {
		return nil, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	j.mu.Lock()
	entries, err := j.readLocked()
	j.mu.Unlock()
	if err != nil {
		return nil, err
	}
	matched := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if filter.StableID != "" && entry.StableID != filter.StableID {
			continue
		}
		if filter.Source != "" && entry.Source != filter.Source {
			continue
		}
		if filter.Verdict != "" && entry.Verdict != filter.Verdict {
			continue
		}
		if !filter.From.IsZero() && entry.At.Before(filter.From) {
			continue
		}
		if !filter.To.IsZero() && !entry.At.Before(filter.To) {
			continue
		}
		matched = append(matched, entry)
	}
	sort.SliceStable(matched, func(a, b int) bool { return matched[a].At.After(matched[b].At) })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

// Compact applies retention and the size cap now. It runs at startup and after
// every few hundred appends; a failure leaves the file as it was.
func (j *Journal) Compact() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.compactLocked()
}

func (j *Journal) compactLocked() error {
	j.appended = 0
	entries, err := j.readLocked()
	if err != nil {
		return err
	}
	cutoff := j.now().Add(-j.retention)
	kept := make([][]byte, 0, len(entries))
	size := int64(0)
	// Newest first, so the size cap drops the oldest lines.
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].At.After(entries[b].At) })
	for _, entry := range entries {
		if entry.At.Before(cutoff) {
			continue
		}
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if size+int64(len(line))+1 > j.maxBytes {
			break
		}
		size += int64(len(line)) + 1
		kept = append(kept, line)
	}
	var buffer bytes.Buffer
	for index := len(kept) - 1; index >= 0; index-- {
		buffer.Write(kept[index])
		buffer.WriteByte('\n')
	}
	temporary := j.path + ".tmp"
	if err := os.WriteFile(temporary, buffer.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write compacted verdict journal: %w", err)
	}
	if err := os.Rename(temporary, j.path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace verdict journal: %w", err)
	}
	return nil
}

// readLocked reads every parseable line. A torn last line — a crash in the
// middle of an append — or a line from an unknown future format is skipped
// rather than failing the whole read: the journal is history, and one bad line
// should not hide the rest of it.
func (j *Journal) readLocked() ([]Entry, error) {
	file, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open verdict journal: %w", err)
	}
	defer file.Close()
	var entries []Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil || entry.StableID == "" || entry.At.IsZero() {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read verdict journal: %w", err)
	}
	return entries, nil
}

// bounded trims every free-text field. The writers already use fixed phrases
// and identifiers; this is the backstop that keeps one oversized value from
// becoming a multi-megabyte line.
func bounded(entry Entry) Entry {
	for _, field := range []*string{
		&entry.Source, &entry.Trigger, &entry.Kind, &entry.Outcome, &entry.StableID, &entry.Node,
		&entry.SessionID, &entry.AgentID, &entry.AgentName, &entry.Region, &entry.Provider,
		&entry.Verdict, &entry.Detail, &entry.LocalStatus, &entry.LocalFailureCode, &entry.LocalServerID,
		&entry.AgentStatus, &entry.AgentFailureCode, &entry.AgentFailureStage, &entry.AgentServerID,
	} {
		*field = truncate(strings.TrimSpace(*field))
	}
	return entry
}

func truncate(value string) string {
	runes := []rune(value)
	if len(runes) <= maxTextRunes {
		return value
	}
	return string(runes[:maxTextRunes-1]) + "…"
}
