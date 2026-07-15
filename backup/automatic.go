package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/logger"
)

const (
	automaticPrefix     = "xray-checker-auto-"
	automaticTimeLayout = "20060102T150405Z"
	maxAutomaticFiles   = 7
	automaticMaxAge     = 7 * 24 * time.Hour
)

type AutomaticResult struct {
	Created bool
	Path    string
	Removed []string
}

type automaticArchive struct {
	Name      string
	CreatedAt time.Time
}

type AutomaticScheduler struct {
	creator   *Creator
	backupDir string
	now       func() time.Time

	mu       sync.Mutex
	startMu  sync.Mutex
	started  bool
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewAutomaticScheduler(creator *Creator, backupDir string) *AutomaticScheduler {
	return &AutomaticScheduler{
		creator:   creator,
		backupDir: backupDir,
		now:       time.Now,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

func (s *AutomaticScheduler) Start() {
	if s == nil {
		return
	}
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return
	}
	s.started = true
	s.startMu.Unlock()
	go s.loop()
}

func (s *AutomaticScheduler) Stop() {
	if s == nil {
		return
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

func (s *AutomaticScheduler) RunOnce() (AutomaticResult, error) {
	if s == nil || s.creator == nil {
		return AutomaticResult{}, fmt.Errorf("automatic backup creator is required")
	}
	if s.backupDir == "" {
		return AutomaticResult{}, fmt.Errorf("automatic backup directory is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nowFunc := s.now
	if nowFunc == nil {
		nowFunc = time.Now
	}
	now := nowFunc().UTC()
	if err := os.MkdirAll(s.backupDir, 0700); err != nil {
		return AutomaticResult{}, fmt.Errorf("create automatic backup directory: %w", err)
	}
	removed, err := s.prune(now)
	if err != nil {
		return AutomaticResult{}, err
	}

	archives, err := s.archives()
	if err != nil {
		return AutomaticResult{}, err
	}
	day := now.Format("20060102")
	for _, archive := range archives {
		if archive.CreatedAt.Format("20060102") == day {
			return AutomaticResult{Path: filepath.Join(s.backupDir, archive.Name), Removed: removed}, nil
		}
	}

	tmp, err := os.CreateTemp(s.backupDir, ".xray-checker-auto-*.tmp")
	if err != nil {
		return AutomaticResult{}, fmt.Errorf("create automatic backup file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := s.creator.createAt(tmp, now); err != nil {
		return AutomaticResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		return AutomaticResult{}, fmt.Errorf("sync automatic backup: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return AutomaticResult{}, fmt.Errorf("close automatic backup: %w", err)
	}

	name := automaticPrefix + now.Format(automaticTimeLayout) + ".zip"
	finalPath := filepath.Join(s.backupDir, name)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return AutomaticResult{}, fmt.Errorf("publish automatic backup: %w", err)
	}
	cleanup = false

	removedAfterCreate, err := s.prune(now)
	removed = append(removed, removedAfterCreate...)
	if err != nil {
		return AutomaticResult{Created: true, Path: finalPath, Removed: removed}, err
	}
	return AutomaticResult{Created: true, Path: finalPath, Removed: removed}, nil
}

func (s *AutomaticScheduler) loop() {
	defer close(s.doneCh)
	for {
		result, err := s.RunOnce()
		if err != nil {
			logger.Warn("Failed to create automatic backup: %v", err)
		} else if result.Created {
			logger.Info("Created automatic backup: %s", result.Path)
		}
		for _, name := range result.Removed {
			logger.Info("Removed expired automatic backup: %s", name)
		}

		nowFunc := s.now
		if nowFunc == nil {
			nowFunc = time.Now
		}
		now := nowFunc().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 5, 0, 0, time.UTC).AddDate(0, 0, 1)
		wait := next.Sub(now)
		if wait <= 0 {
			wait = time.Hour
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-s.stopCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (s *AutomaticScheduler) prune(now time.Time) ([]string, error) {
	archives, err := s.archives()
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-automaticMaxAge)
	removed := make([]string, 0)
	kept := make([]automaticArchive, 0, len(archives))
	for _, archive := range archives {
		if archive.CreatedAt.Before(cutoff) {
			if err := os.Remove(filepath.Join(s.backupDir, archive.Name)); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("remove expired automatic backup %s: %w", archive.Name, err)
			}
			removed = append(removed, archive.Name)
			continue
		}
		kept = append(kept, archive)
	}
	if len(kept) <= maxAutomaticFiles {
		return removed, nil
	}
	for _, archive := range kept[maxAutomaticFiles:] {
		if err := os.Remove(filepath.Join(s.backupDir, archive.Name)); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove excess automatic backup %s: %w", archive.Name, err)
		}
		removed = append(removed, archive.Name)
	}
	return removed, nil
}

func (s *AutomaticScheduler) archives() ([]automaticArchive, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return nil, fmt.Errorf("list automatic backups: %w", err)
	}
	archives := make([]automaticArchive, 0, len(entries))
	for _, entry := range entries {
		createdAt, ok := parseAutomaticArchiveName(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect automatic backup %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		archives = append(archives, automaticArchive{Name: entry.Name(), CreatedAt: createdAt})
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].CreatedAt.After(archives[j].CreatedAt)
	})
	return archives, nil
}

func parseAutomaticArchiveName(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, automaticPrefix) || !strings.HasSuffix(name, ".zip") {
		return time.Time{}, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, automaticPrefix), ".zip")
	createdAt, err := time.Parse(automaticTimeLayout, value)
	return createdAt, err == nil
}
