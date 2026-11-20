// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
)

// setupMarkerName lives next to the config and is created when the user finishes
// or skips the setup wizard, so it never re-appears on subsequent visits.
const setupMarkerName = "setup-done"

func setupMarkerPath() string {
	return filepath.Join(filepath.Dir(config.DefaultPath()), setupMarkerName)
}

// firstRun reports whether the dashboard should show the setup wizard. True when
// (a) the config exists but has no providers AND (b) the user hasn't already
// finished/skipped the wizard.
func firstRun(cfg *config.Config) bool {
	if cfg != nil && len(cfg.Providers) > 0 {
		return false
	}
	if _, err := os.Stat(setupMarkerPath()); err == nil {
		return false
	}
	return true
}

// loadCfg reads the current config, returning sane defaults on a missing file.
func loadCfg() *config.Config {
	cfg, err := config.Load("")
	if err != nil || cfg == nil {
		return config.Default()
	}
	return cfg
}

// ── GET /api/setup/status ─────────────────────────────────────────────────
// Reports first-run state + what's already configured / discoverable from env.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	cfg := loadCfg()

	// Names of providers already saved in config.
	configured := []string{}
	for _, p := range cfg.Providers {
		configured = append(configured, p.Name)
	}
	sort.Strings(configured)

	// Names of providers discovered via env vars but NOT yet saved.
	discoverable := []string{}
	for _, p := range config.DiscoverFromEnv(cfg.Providers) {
		discoverable = append(discoverable, p.Name)
	}
	sort.Strings(discoverable)

	// Recommended starter set — cheapest/free first, then a great $0.20-ish backup.
	recommended := []map[string]string{
		{"name": "groq", "tier": "free", "note": "fast Llama-3.3, free tier"},
		{"name": "gemini", "tier": "free", "note": "great free tier"},
		{"name": "deepseek", "tier": "standard", "note": "best $/token for coding"},
		{"name": "anthropic", "tier": "premium", "note": "fallback for hard tasks"},
	}

	writeJSON(w, map[string]interface{}{
		"first_run":      firstRun(cfg),
		"configured":     configured,
		"discoverable":   discoverable,
		"recommended":    recommended,
		"platform":       runtime.GOOS,
		"proxy_port":     cfg.Proxy.Port,
		"dashboard_port": cfg.Dashboard.Port,
		"config_path":    config.DefaultPath(),
	})
}

// ── POST /api/setup/test ──────────────────────────────────────────────────
// Body: {"name": "...", "api_key": "..."}. Constructs the provider and runs its
// HealthCheck. Returns {"ok": bool, "error": "..."}.
type setupTestReq struct {
	Name   string `json:"name"`
	APIKey string `json:"api_key"`
}

func (s *Server) handleSetupTest(w http.ResponseWriter, r *http.Request) {
	var req setupTestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "invalid json"})
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if name == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "missing name"})
		return
	}
	p, err := providers.FromConfig(name, strings.TrimSpace(req.APIKey), "", nil)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if err := p.HealthCheck(); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ── POST /api/setup/save ──────────────────────────────────────────────────
// Body: {"providers": [{"name": "...", "api_key": "..."}], "skip": false}.
// Skipping just writes the marker. Otherwise we merge into config (replacing
// any existing entry for the same provider name) and persist.
type setupSaveReq struct {
	Providers []setupTestReq `json:"providers"`
	Skip      bool           `json:"skip"`
}

func (s *Server) handleSetupSave(w http.ResponseWriter, r *http.Request) {
	var req setupSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	saved := 0
	if !req.Skip {
		err := config.Update("", func(cfg *config.Config) (bool, error) {
			// Build a name→index map so we replace duplicates rather than appending.
			idx := map[string]int{}
			for i, p := range cfg.Providers {
				idx[strings.ToLower(p.Name)] = i
			}
			for _, in := range req.Providers {
				name := strings.ToLower(strings.TrimSpace(in.Name))
				key := strings.TrimSpace(in.APIKey)
				if name == "" || key == "" {
					continue
				}
				// Reject unknown names so we don't silently write garbage.
				if _, err := providers.FromConfig(name, "test", "", nil); err != nil {
					continue
				}
				p := config.Provider{Name: name, APIKey: key}
				if i, ok := idx[name]; ok {
					cfg.Providers[i] = p
				} else {
					cfg.Providers = append(cfg.Providers, p)
					idx[name] = len(cfg.Providers) - 1
				}
				saved++
			}
			return true, nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Mark setup done so the wizard never reappears (skip or finish).
	_ = os.MkdirAll(filepath.Dir(setupMarkerPath()), 0o755)
	_ = os.WriteFile(setupMarkerPath(), []byte("ok\n"), 0o644)

	writeJSON(w, map[string]interface{}{
		"ok":           true,
		"saved":        saved,
		"config_path":  config.DefaultPath(),
		"restart_hint": "restart NEXUS so the proxy picks up your new providers: `nexus start`",
	})
}
