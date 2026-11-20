package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdatePreservesConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual.toml")
	link := filepath.Join(dir, "config.toml")
	if err := Save(target, Default()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual.toml", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Update(link, func(cfg *Config) (bool, error) {
		cfg.Upsert(Provider{Name: "test-provider"})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("config symlink was replaced", err)
	}
	cfg, err := Load(target)
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].Name != "test-provider" {
		t.Fatal("symlink target was not updated", err)
	}
}
