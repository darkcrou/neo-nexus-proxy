// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Update serializes NEXUS config edits across the dashboard and CLI processes.
// Return false from edit when the file already reflects the requested change.
func Update(path string, edit func(*Config) (bool, error)) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockConfig(lock); err != nil {
		return fmt.Errorf("lock NEXUS config: %w", err)
	}
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	changed, err := edit(cfg)
	if err != nil || !changed {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	err = toml.NewEncoder(temp).Encode(cfg)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
