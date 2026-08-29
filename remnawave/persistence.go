package remnawave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func DecodeConfig(data []byte) (ConfigFile, error) {
	var config ConfigFile
	if err := decodeSingleJSON(data, &config); err != nil {
		return ConfigFile{}, fmt.Errorf("decode Remnawave announce config: %w", err)
	}
	if err := normalizeConfig(&config); err != nil {
		return ConfigFile{}, err
	}
	if err := validateConfig(config); err != nil {
		return ConfigFile{}, err
	}
	return config, nil
}

func DecodeRuntime(data []byte) (RuntimeFile, error) {
	var runtime RuntimeFile
	if err := decodeSingleJSON(data, &runtime); err != nil {
		return RuntimeFile{}, fmt.Errorf("decode Remnawave announce runtime: %w", err)
	}
	normalizeRuntime(&runtime)
	if err := validateRuntime(runtime); err != nil {
		return RuntimeFile{}, err
	}
	return runtime, nil
}

// ValidateConfigData is used by backup restore to apply the same typed schema
// checks as the live service.
func ValidateConfigData(data []byte) error {
	_, err := DecodeConfig(data)
	return err
}

func decodeSingleJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("contains trailing JSON data")
	} else if err != io.EOF {
		return fmt.Errorf("contains invalid trailing JSON data: %w", err)
	}
	return nil
}

func readConfigFile(path string) (ConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultConfig(), nil
		}
		return ConfigFile{}, fmt.Errorf("read Remnawave announce config: %w", err)
	}
	return DecodeConfig(data)
}

func readRuntimeFile(path string) (RuntimeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultRuntime(), nil
		}
		return RuntimeFile{}, fmt.Errorf("read Remnawave announce runtime: %w", err)
	}
	return DecodeRuntime(data)
}

func writeConfigFile(path string, config ConfigFile, now time.Time) error {
	config.Version = ConfigVersion
	config.UpdatedAt = now.UTC()
	if err := normalizeConfig(&config); err != nil {
		return err
	}
	if err := validateConfig(config); err != nil {
		return err
	}
	return writeJSONAtomic(path, config)
}

func writeRuntimeFile(path string, runtime RuntimeFile, now time.Time) error {
	runtime.Version = RuntimeVersion
	runtime.UpdatedAt = now.UTC()
	normalizeRuntime(&runtime)
	if err := validateRuntime(runtime); err != nil {
		return err
	}
	return writeJSONAtomic(path, runtime)
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create Remnawave state directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Remnawave state: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".remnawave-*.tmp")
	if err != nil {
		return fmt.Errorf("create Remnawave state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect Remnawave state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write Remnawave state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync Remnawave state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Remnawave state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish Remnawave state: %w", err)
	}
	return nil
}
