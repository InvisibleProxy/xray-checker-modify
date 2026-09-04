// Package subsource owns the subscription sources an operator adds from the
// admin panel, alongside the ones the environment provides.
//
// It exists because a source is more than a URL. A third-party Remnawave panel
// answers only clients it recognises, and refuses the subscription entirely
// when its HWID device limit is on. Which client to impersonate, and under
// which HWID, is therefore a property of one source — the operator's own panel
// is keyed on the checker's own User-Agent and must keep it, while a foreign
// panel has to be approached as Happ or INCY.
//
// Environment sources stay in the environment: they are the deployment's own
// configuration, they load before any file is read, and the checker must keep
// starting from them alone. This package only adds to that list.
package subsource

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/subscription"
)

const StateVersion = 1

// Source is one subscription feed the operator added from the panel.
type Source struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
	// Profile carries the client identity and HWID used to fetch this source.
	Profile   subscription.ClientProfile `json:"profile"`
	CreatedAt time.Time                  `json:"createdAt"`
	UpdatedAt time.Time                  `json:"updatedAt"`
}

type stateFile struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Sources   []Source  `json:"sources"`
}

type Store struct {
	path string

	mu      sync.RWMutex
	sources []Source
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// Load reads the persisted sources. A missing file is an empty list, not an
// error: a deployment that never added a source through the panel is the norm.
func (s *Store) Load() error {
	if s == nil || s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse subscription sources: %w", err)
	}
	if state.Version != StateVersion {
		return fmt.Errorf("unsupported subscription source schema version %d", state.Version)
	}

	sources := make([]Source, 0, len(state.Sources))
	for _, source := range state.Sources {
		normalized, err := normalize(source)
		if err != nil {
			// One malformed entry must not cost the operator every other
			// source; it is dropped and the rest still load.
			continue
		}
		sources = append(sources, normalized)
	}
	sortSources(sources)

	s.mu.Lock()
	s.sources = sources
	s.mu.Unlock()
	return nil
}

// List returns every stored source, enabled or not.
func (s *Store) List() []Source {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Source(nil), s.sources...)
}

// EnabledSources returns the sources that should be fetched.
func (s *Store) EnabledSources() []Source {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	enabled := make([]Source, 0, len(s.sources))
	for _, source := range s.sources {
		if source.Enabled {
			enabled = append(enabled, source)
		}
	}
	return enabled
}

// Add stores a new source. An HWID is generated when the profile needs one and
// the operator did not supply it, because the value has to stay stable: the
// remote panel ties a device slot to it, and a new value on every fetch would
// claim a new slot until the limit is reached.
func (s *Store) Add(input Source) (Source, error) {
	if s == nil {
		return Source{}, fmt.Errorf("subscription source store is unavailable")
	}

	normalized, err := normalize(input)
	if err != nil {
		return Source{}, err
	}
	if normalized.Profile.HWID == "" && profileNeedsHWID(normalized.Profile.Profile) {
		hwid, err := subscription.GenerateHWID()
		if err != nil {
			return Source{}, err
		}
		normalized.Profile.HWID = hwid
	}

	now := time.Now().UTC()
	normalized.CreatedAt = now
	normalized.UpdatedAt = now
	if normalized.ID == "" {
		normalized.ID = newID(now)
	}

	s.mu.Lock()
	for _, existing := range s.sources {
		if existing.URL == normalized.URL {
			s.mu.Unlock()
			return Source{}, fmt.Errorf("this subscription URL is already added")
		}
	}
	s.sources = append(s.sources, normalized)
	sortSources(s.sources)
	sources := append([]Source(nil), s.sources...)
	s.mu.Unlock()

	if err := s.save(sources); err != nil {
		return Source{}, err
	}
	return normalized, nil
}

// Update replaces a stored source, keeping its identity and creation time.
func (s *Store) Update(id string, input Source) (Source, error) {
	if s == nil {
		return Source{}, fmt.Errorf("subscription source store is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Source{}, fmt.Errorf("source id is required")
	}

	// The API masks URLs, so an edit that does not retype one means "keep it".
	// Resolving that before validation lets every other field be edited without
	// the operator having to paste the subscription token again.
	if strings.TrimSpace(input.URL) == "" {
		s.mu.RLock()
		for _, existing := range s.sources {
			if existing.ID == id {
				input.URL = existing.URL
				break
			}
		}
		s.mu.RUnlock()
	}

	normalized, err := normalize(input)
	if err != nil {
		return Source{}, err
	}

	s.mu.Lock()
	index := -1
	for i, existing := range s.sources {
		if existing.ID == id {
			index = i
			continue
		}
		if existing.URL == normalized.URL {
			s.mu.Unlock()
			return Source{}, fmt.Errorf("this subscription URL is already added")
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return Source{}, fmt.Errorf("subscription source not found")
	}

	previous := s.sources[index]
	normalized.ID = previous.ID
	normalized.CreatedAt = previous.CreatedAt
	normalized.UpdatedAt = time.Now().UTC()
	// A profile that still needs an HWID keeps the one it already had, so
	// editing an unrelated field does not release the panel's device slot.
	if normalized.Profile.HWID == "" && profileNeedsHWID(normalized.Profile.Profile) {
		if previous.Profile.HWID != "" {
			normalized.Profile.HWID = previous.Profile.HWID
		} else {
			hwid, err := subscription.GenerateHWID()
			if err != nil {
				s.mu.Unlock()
				return Source{}, err
			}
			normalized.Profile.HWID = hwid
		}
	}
	s.sources[index] = normalized
	sortSources(s.sources)
	sources := append([]Source(nil), s.sources...)
	s.mu.Unlock()

	if err := s.save(sources); err != nil {
		return Source{}, err
	}
	return normalized, nil
}

func (s *Store) Delete(id string) error {
	if s == nil {
		return fmt.Errorf("subscription source store is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("source id is required")
	}

	s.mu.Lock()
	remaining := make([]Source, 0, len(s.sources))
	found := false
	for _, source := range s.sources {
		if source.ID == id {
			found = true
			continue
		}
		remaining = append(remaining, source)
	}
	if !found {
		s.mu.Unlock()
		return fmt.Errorf("subscription source not found")
	}
	s.sources = remaining
	sources := append([]Source(nil), s.sources...)
	s.mu.Unlock()

	return s.save(sources)
}

func (s *Store) save(sources []Source) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{
		Version:   StateVersion,
		UpdatedAt: time.Now().UTC(),
		Sources:   sources,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func normalize(source Source) (Source, error) {
	source.ID = strings.TrimSpace(source.ID)
	source.URL = strings.TrimSpace(source.URL)
	source.Name = strings.TrimSpace(source.Name)
	if source.URL == "" {
		return Source{}, fmt.Errorf("subscription URL is required")
	}
	if !strings.HasPrefix(source.URL, "http://") && !strings.HasPrefix(source.URL, "https://") {
		return Source{}, fmt.Errorf("subscription URL must start with http:// or https://")
	}

	source.Profile.Profile = subscription.NormalizeClientProfileName(source.Profile.Profile)
	source.Profile.UserAgent = strings.TrimSpace(source.Profile.UserAgent)
	source.Profile.HWID = strings.TrimSpace(source.Profile.HWID)
	source.Profile.DeviceOS = strings.TrimSpace(source.Profile.DeviceOS)
	source.Profile.OSVersion = strings.TrimSpace(source.Profile.OSVersion)
	source.Profile.DeviceModel = strings.TrimSpace(source.Profile.DeviceModel)
	source.Profile.Locale = strings.TrimSpace(source.Profile.Locale)

	if err := subscription.ValidateHWID(source.Profile.HWID); err != nil {
		return Source{}, err
	}
	if source.Profile.Profile == subscription.ClientProfileCustom && source.Profile.UserAgent == "" {
		return Source{}, fmt.Errorf("a custom client profile needs a User-Agent")
	}
	return source, nil
}

// profileNeedsHWID reports whether a profile should carry a generated HWID. The
// checker profile does not: it has a fixed one, deliberately shared, so the
// operator's own panel sees a single device.
func profileNeedsHWID(profile string) bool {
	return profile != subscription.ClientProfileChecker
}

func sortSources(sources []Source) {
	sort.SliceStable(sources, func(i, j int) bool {
		if !sources[i].CreatedAt.Equal(sources[j].CreatedAt) {
			return sources[i].CreatedAt.Before(sources[j].CreatedAt)
		}
		return sources[i].ID < sources[j].ID
	})
}

func newID(now time.Time) string {
	return fmt.Sprintf("src-%d", now.UnixNano())
}
