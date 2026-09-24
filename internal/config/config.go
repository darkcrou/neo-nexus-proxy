// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk NEXUS configuration (~/.nexus/config.toml).
type Config struct {
	Proxy     Proxy      `toml:"proxy"`
	Dashboard Dashboard  `toml:"dashboard"`
	Routing   Routing    `toml:"routing"`
	Providers []Provider `toml:"providers"`
	Rules     []Rule     `toml:"rules"`
}

// Rule is a declarative routing override. When all of its (non-empty) match
// conditions hold, the request is pinned to UseProvider or restricted to UseTier.
type Rule struct {
	WhenModelContains  string `toml:"when_model_contains,omitempty"`
	WhenPromptContains string `toml:"when_prompt_contains,omitempty"`
	WhenComplexity     string `toml:"when_complexity,omitempty"` // simple|standard|complex|critical
	WhenHasTools       *bool  `toml:"when_has_tools,omitempty"`
	UseProvider        string `toml:"use_provider,omitempty"`
	UseTier            string `toml:"use_tier,omitempty"`
}

type Proxy struct {
	Port int `toml:"port"`
}

type Dashboard struct {
	Port int `toml:"port"`
}

type Routing struct {
	Strategy          string  `toml:"strategy"`                    // auto | manual | cheapest | fastest
	DailyBudgetUSD    float64 `toml:"daily_budget_usd,omitempty"`  // 0 = unlimited; over budget → free/local only
	SemanticCache     bool    `toml:"semantic_cache,omitempty"`    // near-match response caching for tool-less requests
	SemanticThreshold float64 `toml:"semantic_threshold,omitempty"` // cosine threshold (0 ⇒ 0.95)
	Cascade           bool    `toml:"cascade,omitempty"`           // cheap-first cascade with verification
	Adaptive          bool    `toml:"adaptive,omitempty"`          // learned routing: prefer historically-best provider per task
	Redact            bool    `toml:"redact,omitempty"`            // privacy firewall: mask secrets/PII before forwarding
	Inspect           bool    `toml:"inspect,omitempty"`           // capture full prompts/responses for the inspector + replay
	AlertWebhook      string  `toml:"alert_webhook,omitempty"`     // Slack/Discord/generic webhook for budget alerts
	AlertThreshold    float64 `toml:"alert_threshold,omitempty"`   // fraction of budget that triggers a warning (0 ⇒ 0.8)
	MaxRequestUSD     float64 `toml:"max_request_usd,omitempty"`   // guardrail: downgrade a single request estimated above this to free/local
}

// Provider is a single configured LLM provider.
type Provider struct {
	Name    string `toml:"name"`
	Type    string `toml:"type,omitempty"`     // "openai-compatible" for a custom endpoint; empty = built-in by name
	APIKey  string `toml:"api_key,omitempty"`  // literal, or "env:VAR_NAME" to read from the environment
	// APIKeys is an optional pool of keys for the same provider. NEXUS round-robins
	// across them and skips any that hit a rate limit (429) — so multiple free-tier
	// keys behave like one bigger free quota. Each may be literal or "env:VAR".
	APIKeys []string `toml:"api_keys,omitempty"`
	BaseURL string   `toml:"base_url,omitempty"` // required for custom/ollama providers
	Models  []string `toml:"models,omitempty"`
	Tier    string   `toml:"tier,omitempty"`

	// ModelMap optionally overrides which provider model a Claude model maps to,
	// e.g. {"claude-sonnet-4-6" = "llama-3.3-70b"}. Use "default" as a catch-all.
	ModelMap map[string]string `toml:"model_map,omitempty"`
	// VisionModel optionally sets the provider model ID to use for image-bearing
	// requests. Entirely operator-supplied — NEXUS does not guess this.
	VisionModel string  `toml:"vision_model,omitempty"`
	InputPer1M  float64 `toml:"input_per_1m,omitempty"`  // optional pricing override (USD/1M)
	OutputPer1M float64 `toml:"output_per_1m,omitempty"` // optional pricing override (USD/1M)

	// Optional off-peak pricing (e.g. DeepSeek discount window), UTC hours.
	OffPeakInputPer1M  float64 `toml:"off_peak_input_per_1m,omitempty"`
	OffPeakOutputPer1M float64 `toml:"off_peak_output_per_1m,omitempty"`
	OffPeakStartUTC    int     `toml:"off_peak_start_utc,omitempty"`
	OffPeakEndUTC      int     `toml:"off_peak_end_utc,omitempty"`

	// Enterprise providers:
	Region     string `toml:"region,omitempty"`      // AWS Bedrock / Google Vertex region
	Project    string `toml:"project,omitempty"`     // Google Vertex project ID
	APIVersion string `toml:"api_version,omitempty"` // Azure OpenAI api-version
}

// DefaultPath returns ~/.nexus/config.toml.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nexus", "config.toml")
}

// Default returns a config with sensible zero-config defaults.
func Default() *Config {
	return &Config{
		Proxy:     Proxy{Port: 3000},
		Dashboard: Dashboard{Port: 2222},
		Routing:   Routing{Strategy: "auto"},
	}
}

// Load reads the config from path (or DefaultPath if empty). A missing file is
// not an error — defaults are returned so NEXUS works with zero config.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	if cfg.Proxy.Port == 0 {
		cfg.Proxy.Port = 3000
	}
	if cfg.Dashboard.Port == 0 {
		cfg.Dashboard.Port = 2222
	}
	if cfg.Routing.Strategy == "" {
		cfg.Routing.Strategy = "auto"
	}
	return cfg, nil
}

// Save writes the config to path (or DefaultPath if empty), creating the
// parent directory if needed.
func Save(path string, cfg *Config) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

// Upsert adds a provider, or replaces an existing one with the same name.
func (c *Config) Upsert(p Provider) {
	for i, existing := range c.Providers {
		if strings.EqualFold(existing.Name, p.Name) {
			c.Providers[i] = p
			return
		}
	}
	c.Providers = append(c.Providers, p)
}

// knownEnvKeys maps a provider name to the environment variables commonly used
// for its API key, so NEXUS can auto-discover providers with zero config.
var knownEnvKeys = []struct {
	name string
	vars []string
}{
	{"anthropic", []string{"ANTHROPIC_API_KEY"}},
	{"openai", []string{"OPENAI_API_KEY"}},
	{"groq", []string{"GROQ_API_KEY"}},
	{"deepseek", []string{"DEEPSEEK_API_KEY"}},
	{"gemini", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	{"mistral", []string{"MISTRAL_API_KEY"}},
	{"together", []string{"TOGETHER_API_KEY", "TOGETHER_AI_API_KEY"}},
	{"openrouter", []string{"OPENROUTER_API_KEY"}},
	{"cohere", []string{"COHERE_API_KEY"}},
	{"xai", []string{"XAI_API_KEY", "GROK_API_KEY"}},
	{"fireworks", []string{"FIREWORKS_API_KEY"}},
	{"perplexity", []string{"PERPLEXITY_API_KEY"}},
	{"deepinfra", []string{"DEEPINFRA_API_KEY"}},
	{"cerebras", []string{"CEREBRAS_API_KEY"}},
	{"sambanova", []string{"SAMBANOVA_API_KEY"}},
	{"nvidia", []string{"NVIDIA_API_KEY"}},
	{"moonshot", []string{"MOONSHOT_API_KEY"}},
	{"zhipu", []string{"ZHIPU_API_KEY"}},
	{"ai21", []string{"AI21_API_KEY"}},
	{"novita", []string{"NOVITA_API_KEY"}},
	{"hyperbolic", []string{"HYPERBOLIC_API_KEY"}},
	{"nebius", []string{"NEBIUS_API_KEY"}},
	{"lambda", []string{"LAMBDA_API_KEY", "LAMBDA_CLOUD_API_KEY"}},
	{"baseten", []string{"BASETEN_API_KEY"}},
	{"featherless", []string{"FEATHERLESS_API_KEY"}},
	{"kluster", []string{"KLUSTER_API_KEY"}},
	{"venice", []string{"VENICE_API_KEY"}},
	{"friendli", []string{"FRIENDLI_TOKEN", "FRIENDLI_API_KEY"}},
	{"chutes", []string{"CHUTES_API_KEY"}},
}

// DiscoverFromEnv returns providers whose API keys are present in the
// environment but not already configured. The key is stored as "env:VAR" so it
// is resolved at runtime and never persisted. Values equal to "nexus-local"
// (Claude Code's placeholder when pointed at NEXUS) are ignored.
func DiscoverFromEnv(existing []Provider) []Provider {
	have := map[string]bool{}
	for _, p := range existing {
		have[strings.ToLower(p.Name)] = true
	}
	var out []Provider
	for _, k := range knownEnvKeys {
		if have[k.name] {
			continue
		}
		for _, v := range k.vars {
			val := os.Getenv(v)
			if val == "" || val == "nexus-local" {
				continue
			}
			out = append(out, Provider{Name: k.name, APIKey: "env:" + v})
			break
		}
	}
	return out
}

// ResolveKey resolves an api_key value: "env:VAR" reads VAR from the
// environment; anything else is returned verbatim.
func ResolveKey(v string) string {
	if strings.HasPrefix(v, "env:") {
		return os.Getenv(strings.TrimPrefix(v, "env:"))
	}
	return v
}
