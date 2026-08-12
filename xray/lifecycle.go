package xray

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type LifecycleRunner interface {
	Start() error
	Stop() error
}

// RestartWithConfigRollback generates a candidate beside the active Xray
// config, swaps it only after generation succeeds, and restores the previous
// file and process if Xray rejects the candidate.
func RestartWithConfigRollback(configFile string, runner LifecycleRunner, generate func(string) error) error {
	if configFile == "" || runner == nil || generate == nil {
		return fmt.Errorf("config file, runner, and generator are required")
	}
	absConfig, err := filepath.Abs(configFile)
	if err != nil {
		return fmt.Errorf("resolve Xray config path: %w", err)
	}
	if _, err := os.Stat(absConfig); err != nil {
		return fmt.Errorf("read last-known-good Xray config: %w", err)
	}
	suffix := fmt.Sprintf(".%d", time.Now().UnixNano())
	candidate := absConfig + ".refresh-candidate" + suffix
	lastGood := absConfig + ".refresh-last-good" + suffix
	defer os.Remove(candidate)

	if err := generate(candidate); err != nil {
		return fmt.Errorf("generate candidate Xray config: %w", err)
	}
	if err := runner.Stop(); err != nil {
		return fmt.Errorf("stop Xray before refresh: %w", err)
	}
	if err := os.Rename(absConfig, lastGood); err != nil {
		restartErr := runner.Start()
		return errors.Join(fmt.Errorf("preserve last-known-good Xray config: %w", err), restartErr)
	}
	if err := os.Rename(candidate, absConfig); err != nil {
		restoreErr := os.Rename(lastGood, absConfig)
		restartErr := runner.Start()
		return errors.Join(fmt.Errorf("activate candidate Xray config: %w", err), restoreErr, restartErr)
	}
	if err := runner.Start(); err != nil {
		removeErr := os.Remove(absConfig)
		restoreErr := os.Rename(lastGood, absConfig)
		restartErr := runner.Start()
		return errors.Join(fmt.Errorf("start Xray with refreshed config: %w", err), removeErr, restoreErr, restartErr)
	}
	_ = os.Remove(lastGood)
	return nil
}
