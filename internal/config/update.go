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
	path, err := configTarget(path)
	if err != nil {
		return err
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

// Follow file symlinks so an update preserves a dotfile-managed config link.
// Stop at the final target even when the target file is still missing.
func configTarget(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || (err == nil && info.Mode()&os.ModeSymlink == 0) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", fmt.Errorf("too many symlinks resolving NEXUS config")
}
