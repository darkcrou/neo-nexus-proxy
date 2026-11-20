// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
)

// The Providers tab lists every configured provider with the models it serves.
// These tests specify the enriched /api/providers surface: config facts, the
// live-catalog → configured → defaults model ladder with an honest source label,
// per-provider discovery state, and the guarantee that no key value ever leaves
// the server. The legacy name/tier/healthy fields stay for the overview chips.

func saveProvidersConfig(t *testing.T, provs ...config.Provider) {
	t.Helper()
	cfg := &config.Config{Providers: provs}
	cfg.Proxy.Port = 3000
	cfg.Dashboard.Port = 2222
	cfg.Routing.Strategy = "auto"
	if err := config.Save("", cfg); err != nil {
		t.Fatal(err)
	}
}

func getProviders(t *testing.T, s *Server) ([]map[string]interface{}, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleProviders(rec, httptest.NewRequest("GET", "/api/providers", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	var envelope struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v — body %s", err, body)
	}
	return envelope.Providers, body
}

func findProvider(t *testing.T, list []map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	for _, p := range list {
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("provider %q missing from %v", name, list)
	return nil
}

func TestProvidersEmptyOnFreshHome(t *testing.T) {
	isolateNexusHome(t)
	s := NewServer(0, 0, nil, nil)

	list, body := getProviders(t, s)
	if len(list) != 0 {
		t.Errorf("providers = %d, want 0 on a fresh home", len(list))
	}
	if !strings.Contains(body, `"providers":[]`) {
		t.Errorf("empty list must marshal as [], got %s", body)
	}
}

func TestProvidersReportConfigFactsWithoutLeakingKeys(t *testing.T) {
	isolateNexusHome(t)
	t.Setenv("GEMINI_API_KEY", "env-secret-value")
	saveProvidersConfig(t,
		config.Provider{Name: "groq", APIKey: "gsk_literal_secret"},
		config.Provider{Name: "acme", Type: "openai-compatible", BaseURL: "http://localhost:9999/v1",
			APIKeys: []string{"pool-k1", "pool-k2"}, Tier: "premium"},
		config.Provider{Name: "ollama", BaseURL: "http://localhost:11434"},
		config.Provider{Name: "gemini", APIKey: "env:GEMINI_API_KEY"},
	)
	s := NewServer(0, 0, nil, nil)

	list, body := getProviders(t, s)

	groq := findProvider(t, list, "groq")
	if groq["family"] != "groq" || groq["tier"] != "free" {
		t.Errorf("groq family/tier = %v/%v, want groq/free (default tier)", groq["family"], groq["tier"])
	}
	if groq["keys"] != float64(1) || groq["key_source"] != "config" {
		t.Errorf("groq keys = %v/%v, want 1/config", groq["keys"], groq["key_source"])
	}

	acme := findProvider(t, list, "acme")
	if acme["family"] != "openai-compatible" || acme["tier"] != "premium" {
		t.Errorf("acme family/tier = %v/%v, want openai-compatible/premium", acme["family"], acme["tier"])
	}
	if acme["keys"] != float64(2) || acme["key_source"] != "config" {
		t.Errorf("acme keys = %v/%v, want the 2-key pool", acme["keys"], acme["key_source"])
	}
	if acme["base_url"] != "http://localhost:9999/v1" {
		t.Errorf("acme base_url = %v, want the configured URL", acme["base_url"])
	}

	ollama := findProvider(t, list, "ollama")
	if ollama["tier"] != "local" || ollama["keys"] != float64(0) || ollama["key_source"] != "none" {
		t.Errorf("ollama = %v, want local tier and no keys", ollama)
	}

	gemini := findProvider(t, list, "gemini")
	if gemini["keys"] != float64(1) || gemini["key_source"] != "env" || gemini["key_env"] != "GEMINI_API_KEY" {
		t.Errorf("gemini keys = %v/%v/%v, want 1/env/GEMINI_API_KEY", gemini["keys"], gemini["key_source"], gemini["key_env"])
	}

	for _, secret := range []string{"gsk_literal_secret", "pool-k1", "pool-k2", "env-secret-value"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks credential material %q", secret)
		}
	}
}

func TestProvidersLegacyFieldsPreserved(t *testing.T) {
	isolateNexusHome(t)
	saveProvidersConfig(t, config.Provider{Name: "groq", APIKey: "gsk_x"})
	s := NewServer(0, 0, nil, nil)

	list, _ := getProviders(t, s)
	p := findProvider(t, list, "groq")
	if p["name"] != "groq" || p["tier"] != "free" || p["healthy"] != true {
		t.Errorf("legacy fields = %v, want name/tier/healthy for the overview chips", p)
	}
}

func TestProvidersModelLadder(t *testing.T) {
	isolateNexusHome(t)
	saveProvidersConfig(t,
		config.Provider{Name: "groq", APIKey: "k"},                                      // live listing wins
		config.Provider{Name: "deepseek", APIKey: "k", Models: []string{"my-deepseek"}}, // seeded from config
		config.Provider{Name: "openai", APIKey: "k"},                                    // seeded from family defaults
		config.Provider{Name: "gemini", APIKey: "k"},                                    // no catalog entry at all
		config.Provider{Name: "mistral", APIKey: "k"},                                   // entry present but empty
	)
	s := NewServer(0, 0, nil, nil)
	s.ConfigureCatalog(func() []ProviderCatalogEntry {
		return []ProviderCatalogEntry{
			{Name: "groq", Models: []string{"live-llama"}, Discovered: true, Warming: true},
			{Name: "deepseek", Models: []string{"my-deepseek"}, Warming: true},
			{Name: "openai", Models: []string{"gpt-4o", "gpt-4o-mini"}, Warming: true},
			{Name: "mistral", Warming: true},
		}
	})

	list, _ := getProviders(t, s)

	cases := []struct {
		name, source, discovery string
		models                  []string
	}{
		{"groq", "live", "ok", []string{"live-llama"}},
		{"deepseek", "configured", "pending", []string{"my-deepseek"}},
		{"openai", "defaults", "pending", []string{"gpt-4o", "gpt-4o-mini"}},
		{"gemini", "defaults", "not_loaded", []string{"gemini-2.0-flash"}},
		{"mistral", "defaults", "pending", []string{"mistral-large-latest", "mistral-small-latest"}},
	}
	for _, c := range cases {
		p := findProvider(t, list, c.name)
		if p["model_source"] != c.source || p["discovery"] != c.discovery {
			t.Errorf("%s source/discovery = %v/%v, want %s/%s", c.name, p["model_source"], p["discovery"], c.source, c.discovery)
		}
		got, _ := p["models"].([]interface{})
		if len(got) != len(c.models) {
			t.Errorf("%s models = %v, want %v", c.name, p["models"], c.models)
			continue
		}
		for i, m := range c.models {
			if got[i] != m {
				t.Errorf("%s models = %v, want %v", c.name, p["models"], c.models)
				break
			}
		}
	}
}

func TestProvidersCatalogLookupByFamily(t *testing.T) {
	isolateNexusHome(t)
	// `nexus add groq-us … --type groq` registers under the family name, so the
	// catalog entry is keyed "groq" while config calls the instance "groq-us".
	saveProvidersConfig(t, config.Provider{Name: "groq-us", Type: "groq", APIKey: "k"})
	s := NewServer(0, 0, nil, nil)
	s.ConfigureCatalog(func() []ProviderCatalogEntry {
		return []ProviderCatalogEntry{
			{Name: "groq", Models: []string{"live-model"}, Discovered: true, Warming: true},
		}
	})

	list, _ := getProviders(t, s)
	p := findProvider(t, list, "groq-us")
	if p["model_source"] != "live" || p["family"] != "groq" || p["tier"] != "free" {
		t.Errorf("groq-us = %v, want the family's live catalog, family groq, free tier", p)
	}
}

func TestProvidersDiscoveryStates(t *testing.T) {
	isolateNexusHome(t)
	saveProvidersConfig(t,
		config.Provider{Name: "ok-p", Type: "openai-compatible", BaseURL: "http://ok"},
		config.Provider{Name: "pending-p", Type: "openai-compatible", BaseURL: "http://pending"},
		config.Provider{Name: "unsupported-p", Type: "openai-compatible", BaseURL: "http://unsupported"},
		config.Provider{Name: "failed-p", Type: "openai-compatible", BaseURL: "http://failed"},
		config.Provider{Name: "off-p", Type: "openai-compatible", BaseURL: "http://off"},
		config.Provider{Name: "missing-p", Type: "openai-compatible", BaseURL: "http://missing"},
	)
	s := NewServer(0, 0, nil, nil)
	s.ConfigureCatalog(func() []ProviderCatalogEntry {
		return []ProviderCatalogEntry{
			{Name: "ok-p", Models: []string{"m"}, Discovered: true, Warming: true},
			{Name: "pending-p", Warming: true},
			{Name: "unsupported-p", Unsupported: true, Warming: true},
			{Name: "failed-p", Err: "connection refused", Warming: true},
			{Name: "off-p"}, // catalog built and seeded, warming disabled
		}
	})

	list, body := getProviders(t, s)
	want := map[string]string{
		"ok-p": "ok", "pending-p": "pending", "unsupported-p": "unsupported",
		"failed-p": "failed", "off-p": "off", "missing-p": "not_loaded",
	}
	for name, state := range want {
		if p := findProvider(t, list, name); p["discovery"] != state {
			t.Errorf("%s discovery = %v, want %s", name, p["discovery"], state)
		}
	}
	// An unknown family has no defaults: models must still be an array, not null.
	if p := findProvider(t, list, "missing-p"); p["models"] == nil {
		t.Error("models must marshal as [] when nothing is known")
	}
	if !strings.Contains(body, `"models":[]`) {
		t.Errorf("empty model list must marshal as [], got %s", body)
	}
}

func TestProvidersWithoutCatalogCallback(t *testing.T) {
	isolateNexusHome(t)
	saveProvidersConfig(t, config.Provider{Name: "groq", APIKey: "k"})
	s := NewServer(0, 0, nil, nil) // no ConfigureCatalog — static ladder only

	list, _ := getProviders(t, s)
	p := findProvider(t, list, "groq")
	if p["model_source"] != "defaults" || p["discovery"] != "not_loaded" {
		t.Errorf("groq = %v, want family defaults with no live catalog wired", p)
	}
	models, _ := p["models"].([]interface{})
	if len(models) == 0 {
		t.Error("groq defaults must be listed even without a catalog callback")
	}
}
