package xray

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type lifecycleRunnerStub struct {
	starts         int
	stops          int
	failFirstStart bool
}

func (r *lifecycleRunnerStub) Start() error {
	r.starts++
	if r.failFirstStart && r.starts == 1 {
		return errors.New("candidate rejected")
	}
	return nil
}

func (r *lifecycleRunnerStub) Stop() error {
	r.stops++
	return nil
}

func TestRestartWithConfigRollbackRestoresRejectedCandidate(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "xray_config.json")
	if err := os.WriteFile(configFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunnerStub{failFirstStart: true}
	err := RestartWithConfigRollback(configFile, runner, func(candidate string) error {
		return os.WriteFile(candidate, []byte("new"), 0600)
	})
	if err == nil {
		t.Fatal("rejected candidate returned nil error")
	}
	data, readErr := os.ReadFile(configFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "old" {
		t.Fatalf("active config = %q, want last-known-good", data)
	}
	if runner.starts != 2 || runner.stops != 1 {
		t.Fatalf("runner starts/stops = %d/%d, want 2/1", runner.starts, runner.stops)
	}
}

func TestRestartWithConfigRollbackKeepsAcceptedCandidate(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "xray_config.json")
	if err := os.WriteFile(configFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunnerStub{}
	if err := RestartWithConfigRollback(configFile, runner, func(candidate string) error {
		return os.WriteFile(candidate, []byte("new"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("active config = %q, want candidate", data)
	}
}

func TestRestartWithConfigRollbackDoesNotStopForGenerationFailure(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "xray_config.json")
	if err := os.WriteFile(configFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunnerStub{}
	err := RestartWithConfigRollback(configFile, runner, func(string) error {
		return errors.New("invalid candidate")
	})
	if err == nil {
		t.Fatal("generation failure returned nil")
	}
	data, readErr := os.ReadFile(configFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "old" || runner.starts != 0 || runner.stops != 0 {
		t.Fatalf("old config or runner changed: config=%q starts=%d stops=%d", data, runner.starts, runner.stops)
	}
}
