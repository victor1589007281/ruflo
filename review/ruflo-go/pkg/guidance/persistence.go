package guidance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Save writes bundle to path as JSON using an atomic rename.
func Save(bundle PolicyBundle, path string) error {
	if path == "" {
		return fmt.Errorf("guidance: empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".policy-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// Load reads a PolicyBundle from JSON at path.
func Load(path string) (PolicyBundle, error) {
	var b PolicyBundle
	if path == "" {
		return b, fmt.Errorf("guidance: empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	return b, nil
}
