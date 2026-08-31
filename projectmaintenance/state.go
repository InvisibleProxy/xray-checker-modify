package projectmaintenance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const StateVersion = 1

// ErrEnabled is returned by automatic workflows that are disabled while the
// checker is in project-wide maintenance. Explicit admin probes may opt in.
var ErrEnabled = fmt.Errorf("project maintenance is enabled")

type Snapshot struct {
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since,omitempty"`
}

type StateFile struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Enabled   bool      `json:"enabled"`
	Since     time.Time `json:"since,omitempty"`
}

type Manager struct {
	path string
	now  func() time.Time

	mu      sync.RWMutex
	enabled bool
	since   time.Time
	updated time.Time
}

func NewManager(path string) *Manager {
	return &Manager{path: path, now: time.Now}
}

func (m *Manager) Load() error {
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
	var state StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode project maintenance state: %w", err)
	}
	if state.Version != StateVersion {
		return fmt.Errorf("unsupported project maintenance state version %d", state.Version)
	}
	if !state.Enabled {
		state.Since = time.Time{}
	} else if state.Since.IsZero() {
		state.Since = state.UpdatedAt
		if state.Since.IsZero() {
			state.Since = m.currentTime()
		}
	}
	m.mu.Lock()
	m.enabled = state.Enabled
	m.since = state.Since
	m.updated = state.UpdatedAt
	m.mu.Unlock()
	return nil
}

func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{Enabled: m.enabled, Since: m.since}
}

func (m *Manager) Enabled() bool {
	return m.Snapshot().Enabled
}

func (m *Manager) Set(enabled bool) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("project maintenance manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled == enabled {
		return Snapshot{Enabled: m.enabled, Since: m.since}, nil
	}

	now := m.currentTime().UTC()
	since := time.Time{}
	if enabled {
		since = now
	}
	state := StateFile{
		Version:   StateVersion,
		UpdatedAt: now,
		Enabled:   enabled,
		Since:     since,
	}
	if err := writeStateFile(m.path, state); err != nil {
		return Snapshot{Enabled: m.enabled, Since: m.since}, err
	}
	m.enabled = enabled
	m.since = since
	m.updated = now
	return Snapshot{Enabled: m.enabled, Since: m.since}, nil
}

func (m *Manager) currentTime() time.Time {
	if m != nil && m.now != nil {
		return m.now()
	}
	return time.Now()
}

func writeStateFile(path string, state StateFile) error {
	if path == "" {
		return fmt.Errorf("project maintenance state path is required")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project maintenance state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create project maintenance state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".project-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create project maintenance state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set project maintenance state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write project maintenance state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close project maintenance state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace project maintenance state: %w", err)
	}
	return nil
}
