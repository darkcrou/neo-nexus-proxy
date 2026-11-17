package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.Port != 3000 || cfg.Dashboard.Port != 2222 || cfg.Routing.Strategy != "auto" {
		t.Errorf("expected zero-config defaults, got %+v", cfg)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Upsert(Provider{Name: "groq", APIKey: "gsk-x", Tier: "free", Models: []string{"llama-3.3-70b-versatile"}})

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "groq" || got.Providers[0].APIKey != "gsk-x" {
		t.Errorf("roundtrip mismatch: %+v", got.Providers)
	}
	if len(got.Providers[0].Models) != 1 {
		t.Errorf("models not persisted: %+v", got.Providers[0].Models)
	}
}

func TestUpsertReplacesCaseInsensitive(t *testing.T) {
	cfg := Default()
	cfg.Upsert(Provider{Name: "groq", APIKey: "old"})
	cfg.Upsert(Provider{Name: "Groq", APIKey: "new"}) // same provider, different case

	if len(cfg.Providers) != 1 {
		t.Fatalf("expected 1 provider after upsert, got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].APIKey != "new" {
		t.Errorf("upsert should replace existing, got key %q", cfg.Providers[0].APIKey)
	}
}

func TestResolveKey(t *testing.T) {
	t.Setenv("NEXUS_TEST_KEY", "secret123")
	if got := ResolveKey("env:NEXUS_TEST_KEY"); got != "secret123" {
		t.Errorf("env key not resolved: %q", got)
	}
	if got := ResolveKey("sk-literal"); got != "sk-literal" {
		t.Errorf("literal key changed: %q", got)
	}
}

func TestModelMapRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Upsert(Provider{
		Name: "custom", Type: "openai-compatible", BaseURL: "https://x/v1",
		ModelMap: map[string]string{"claude-sonnet-4-6": "big-model"}, InputPer1M: 1.5,
	})
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Providers[0]
	if p.Type != "openai-compatible" || p.ModelMap["claude-sonnet-4-6"] != "big-model" || p.InputPer1M != 1.5 {
		t.Errorf("custom fields not persisted: %+v", p)
	}
}

// writeConfig writes a config file into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCatalogDefaultsWhenSectionAbsent pins D-11: an absent [catalog] table means
// enabled with a 300s TTL, because polling provider endpoints must be opt-out
// rather than opt-in for a zero-config install.
func TestCatalogDefaultsWhenSectionAbsent(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[proxy]\nport = 3001\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Catalog.Enabled {
		t.Error("an absent [catalog] section must mean enabled")
	}
	if cfg.Catalog.TTLSeconds != 300 {
		t.Errorf("ttl_seconds = %d, want 300", cfg.Catalog.TTLSeconds)
	}
}

func TestCatalogExplicitDisable(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[catalog]\nenabled = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalog.Enabled {
		t.Error("enabled = false must disable the catalog")
	}
	if cfg.Catalog.TTLSeconds != 300 {
		t.Errorf("ttl_seconds = %d, want the 300 default", cfg.Catalog.TTLSeconds)
	}
}

func TestCatalogExplicitTTL(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[catalog]\nttl_seconds = 45\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Catalog.Enabled {
		t.Error("setting ttl_seconds alone must not disable the catalog")
	}
	if cfg.Catalog.TTLSeconds != 45 {
		t.Errorf("ttl_seconds = %d, want 45", cfg.Catalog.TTLSeconds)
	}
}

func TestCatalogNonPositiveTTLUsesDefault(t *testing.T) {
	for _, ttl := range []string{"0", "-5"} {
		cfg, err := Load(writeConfig(t, "[catalog]\nttl_seconds = "+ttl+"\n"))
		if err != nil {
			t.Fatalf("ttl_seconds = %s: %v", ttl, err)
		}
		if cfg.Catalog.TTLSeconds != 300 {
			t.Errorf("ttl_seconds = %s: got %d, want 300", ttl, cfg.Catalog.TTLSeconds)
		}
	}
}
