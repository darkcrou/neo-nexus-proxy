// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/dashboard"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/license"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/proxy"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "nexus",
	Short: "Smart proxy + dashboard for Claude Code",
	Long: `
███╗   ██╗███████╗██╗  ██╗██╗   ██╗███████╗
████╗  ██║██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██╔██╗ ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║╚██╗██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║ ╚████║███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝  ╚═══╝╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝

Route Claude Code to any LLM. Free, local, or cloud.
`,
}

// start command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start proxy + dashboard",
	Long:  "Start the NEXUS proxy server and dashboard",
	RunE:  runStart,
}

// add command
var addCmd = &cobra.Command{
	Use:   "add [provider] [api-key]",
	Short: "Add a provider",
	Example: `  nexus add deepseek sk-xxx
  nexus add groq gsk-xxx
  nexus add gemini AIza-xxx
  nexus add ollama`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runAdd,
}

// status command
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show provider health",
	RunE:  runStatus,
}

// logs command
var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show recent requests",
	RunE:  runLogs,
}

// cost command
var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Show cost breakdown",
	RunE:  runCost,
}

// config command
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or edit config",
	RunE:  runConfig,
}

// models command
var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Show how Claude models map to each provider",
	RunE:  runModels,
}

// doctor command
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose setup, test every provider, and suggest fixes",
	RunE:  runDoctor,
}

// top command — live terminal dashboard (htop for your LLM traffic)
var topCmd = &cobra.Command{
	Use:   "top",
	Short: "Live terminal dashboard for your NEXUS traffic",
	RunE:  runTop,
}

// version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("nexus %s (built %s)\n", Version, BuildTime)
		l := license.Active()
		fmt.Printf("license: %s (%s)\n", l.Notice, l.Edition)
	},
}

// Flags
var (
	flagProxyPort     int
	flagDashboardPort int
	flagDev           bool
	flagNoUI          bool
	flagLogLevel      string
	flagAddType       string
	flagAddBaseURL    string
	flagAddRegion     string
	flagAddProject    string
	flagAddAPIVersion string
	flagBudget            float64
	flagNoCache           bool
	flagSemanticCache     bool
	flagSemanticThreshold float64
	flagCascade           bool
	flagAdaptive          bool
	flagRedact            bool
	flagTopPort           int
	flagTopOnce           bool
	flagInspect           bool
	flagAlertWebhook      string
	flagAlertThreshold    float64
	flagMaxRequestUSD     float64
)

func init() {
	startCmd.Flags().IntVarP(&flagProxyPort, "port", "p", 3000, "Proxy port")
	startCmd.Flags().IntVarP(&flagDashboardPort, "ui", "u", 2222, "Dashboard port")
	startCmd.Flags().BoolVar(&flagDev, "dev", false, "Development mode")
	startCmd.Flags().BoolVar(&flagNoUI, "no-ui", false, "Disable dashboard")
	startCmd.Flags().StringVarP(&flagLogLevel, "log", "l", "info", "Log level (debug|info|warn|error)")
	startCmd.Flags().Float64Var(&flagBudget, "budget", 0, "Daily budget cap in USD (0 = unlimited; free/local only once exceeded)")
	startCmd.Flags().BoolVar(&flagNoCache, "no-cache", false, "Disable the response cache")
	startCmd.Flags().BoolVar(&flagSemanticCache, "semantic-cache", false, "Serve near-identical tool-less requests from cache (local embedding match)")
	startCmd.Flags().Float64Var(&flagSemanticThreshold, "semantic-threshold", 0, "Cosine threshold for the semantic cache (default 0.95)")
	startCmd.Flags().BoolVar(&flagCascade, "cascade", false, "Cheap-first cascade: try the cheapest capable model, verify, escalate on failure")
	startCmd.Flags().BoolVar(&flagAdaptive, "adaptive", false, "Learned routing: prefer the provider that historically handles each task type best")
	startCmd.Flags().BoolVar(&flagRedact, "redact", false, "Privacy firewall: mask secrets/API keys/PII before forwarding to any provider")
	startCmd.Flags().BoolVar(&flagInspect, "inspect", false, "Store full prompts/responses locally so the dashboard can inspect + replay them")
	startCmd.Flags().StringVar(&flagAlertWebhook, "alert-webhook", "", "Slack/Discord/generic webhook URL for daily-budget alerts")
	startCmd.Flags().Float64Var(&flagAlertThreshold, "alert-threshold", 0, "Fraction of the daily budget that triggers a warning (default 0.8)")
	startCmd.Flags().Float64Var(&flagMaxRequestUSD, "max-request-usd", 0, "Guardrail: downgrade a single request estimated above this to free/local")

	topCmd.Flags().IntVar(&flagTopPort, "ui", 2222, "Dashboard port to read from")
	topCmd.Flags().BoolVar(&flagTopOnce, "once", false, "Render a single frame and exit (for scripts/CI)")

	addCmd.Flags().StringVar(&flagAddType, "type", "", "Provider type: openai-compatible | azure | vertex | bedrock")
	addCmd.Flags().StringVar(&flagAddBaseURL, "base-url", "", "Base URL (custom/azure endpoint; optional for ollama)")
	addCmd.Flags().StringVar(&flagAddRegion, "region", "", "Region (AWS Bedrock / Google Vertex)")
	addCmd.Flags().StringVar(&flagAddProject, "project", "", "Project ID (Google Vertex)")
	addCmd.Flags().StringVar(&flagAddAPIVersion, "api-version", "", "API version (Azure OpenAI)")

	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(addCmd)
	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(costCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(topCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(codeCmd)
	rootCmd.AddCommand(benchCmd)
	rootCmd.AddCommand(reportCmd)
	rootCmd.AddCommand(versionCmd)

	reportCmd.Flags().StringVar(&flagReportPeriod, "period", "month", "Period: today | week | month")

	codeCmd.Flags().IntVar(&flagCodePort, "port", 3000, "Proxy port NEXUS should use")
	benchCmd.Flags().IntVar(&flagBenchPort, "port", 3000, "Proxy port to benchmark against")
	benchCmd.Flags().IntVar(&flagBenchN, "n", 10, "How many recent captured requests to benchmark")
	benchCmd.Flags().StringVar(&flagBenchProviders, "providers", "", "Comma-separated providers to benchmark (default: all configured)")
	benchCmd.Flags().StringVar(&flagBenchPrompt, "prompt", "", "Benchmark a single ad-hoc prompt instead of captured traffic")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		color.Red("Error: %v", err)
		os.Exit(1)
	}
}

// ─── Command implementations ───────────────────────────────────────────────

func runStart(cmd *cobra.Command, args []string) error {
	if flagDev {
		flagLogLevel = "debug"
	}
	setupLogging(flagLogLevel)

	// Shared storage + event broker (the broker feeds the dashboard live feed).
	db, err := storage.New(storage.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer db.Close()

	broker := dashboard.NewSSEBroker()

	cfg := &proxy.Config{
		Port:           flagProxyPort,
		DashboardPort:  flagDashboardPort,
		LogLevel:       flagLogLevel,
		DailyBudgetUSD:    flagBudget,
		DisableCache:      flagNoCache,
		SemanticCache:     flagSemanticCache,
		SemanticThreshold: flagSemanticThreshold,
		Cascade:           flagCascade,
		Adaptive:          flagAdaptive,
		Redact:            flagRedact,
		Inspect:           flagInspect,
		AlertWebhook:      flagAlertWebhook,
		AlertThreshold:    flagAlertThreshold,
		MaxRequestUSD:     flagMaxRequestUSD,
	}

	adminToken := os.Getenv("NEXUS_DASHBOARD_ADMIN_TOKEN")
	var qwenSession *qwen.Service
	if !flagNoUI && adminToken != "" {
		qwenSession, err = qwen.Open(filepath.Dir(config.DefaultPath()))
		if err != nil {
			log.Warn().Err(err).Msg("Dashboard Qwen sign-in unavailable")
		} else {
			defer qwenSession.Close()
			cfg.QwenSession = qwenSession
		}
	}
	proxySrv, err := proxy.New(cfg, db, broker)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}

	// Cancel the root context on Ctrl+C / SIGTERM for a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Dashboard server (optional). Shares the broker + DB with the proxy.
	var dash *dashboard.Server
	if !flagNoUI {
		dash = dashboard.NewServer(flagDashboardPort, flagProxyPort, broker, db)
		dash.ConfigureQwen(ctx, qwenSession, adminToken, config.DefaultPath(), proxySrv.QwenReadiness)
		dash.ConfigureCatalog(func() []dashboard.ProviderCatalogEntry {
			src := proxySrv.ProviderCatalog()
			out := make([]dashboard.ProviderCatalogEntry, len(src))
			for i, e := range src {
				out[i] = dashboard.ProviderCatalogEntry{
					Name: e.Name, Models: e.Models, Discovered: e.Discovered,
					Unsupported: e.Unsupported, Err: e.Err, Warming: e.Warming,
				}
			}
			return out
		})
		go func() {
			if err := dash.Start(); err != nil && err != http.ErrServerClosed {
				log.Error().Err(err).Msg("Dashboard server error")
			}
		}()
	}

	printReady(flagProxyPort, flagDashboardPort, !flagNoUI)

	// Run the proxy. Start blocks until ctx is cancelled or a fatal error occurs.
	runErr := proxySrv.Start(ctx)

	// Graceful shutdown.
	fmt.Println()
	color.Yellow("→ Shutting down NEXUS...")
	if dash != nil {
		if err := dash.Shutdown(); err != nil {
			log.Warn().Err(err).Msg("Dashboard shutdown error")
		}
	}
	color.Green("✓ Stopped")
	return runErr
}

func runAdd(cmd *cobra.Command, args []string) error {
	name := strings.ToLower(args[0])
	apiKey := ""
	if len(args) > 1 {
		apiKey = args[1]
	}

	pc := config.Provider{Name: name, APIKey: apiKey, Type: flagAddType, BaseURL: flagAddBaseURL}

	// A comma-separated key becomes a rotating key pool (free-tier load balancing).
	if strings.Contains(apiKey, ",") {
		var keys []string
		for _, p := range strings.Split(apiKey, ",") {
			if p = strings.TrimSpace(p); p != "" {
				keys = append(keys, p)
			}
		}
		pc.APIKeys = keys
		pc.APIKey = ""
		apiKey = ""
	}

	if flagAddType != "" {
		// Custom / enterprise provider (openai-compatible, azure, vertex, bedrock).
		pc.Region = flagAddRegion
		pc.Project = flagAddProject
		pc.APIVersion = flagAddAPIVersion
		if pc.Tier == "" {
			pc.Tier = "standard"
		}
		if _, err := providers.New(specFromConfig(pc)); err != nil {
			return fmt.Errorf("invalid provider config: %w", err)
		}
	} else {
		pc.Tier = providers.DefaultTier(name)
		pc.Models = providers.DefaultModels(name)
		if name == "ollama" && pc.BaseURL == "" {
			pc.BaseURL = "http://localhost:11434"
		}
		if _, err := providers.FromConfig(name, apiKey, pc.BaseURL, pc.Models); err != nil {
			return fmt.Errorf("unknown provider %q — for a custom endpoint use --type (openai-compatible|azure|vertex|bedrock)", name)
		}
	}

	path := config.DefaultPath()
	if err := config.Update(path, func(cfg *config.Config) (bool, error) {
		cfg.Upsert(pc)
		return true, nil
	}); err != nil {
		return err
	}

	color.Green("✓ Added provider: %s (tier: %s)", name, pc.Tier)
	if len(pc.APIKeys) > 1 {
		color.White("  Key pool: %d keys (round-robin, 429 → next key)", len(pc.APIKeys))
	}
	color.White("  Config: %s", path)
	return nil
}

func runStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		color.Yellow("No providers configured. Add one, e.g.:  nexus add groq <key>")
		return nil
	}

	color.Cyan("Provider health:\n")
	for _, pc := range cfg.Providers {
		impl, err := providers.New(specFromConfig(pc))
		if err != nil {
			color.Red("  ✗ %-11s %v", pc.Name, err)
			continue
		}
		if herr := checkProviderHealth(pc, impl); herr != nil {
			color.Red("  ● %-11s [%-8s] DOWN  (%v)", impl.Name(), impl.Tier(), herr)
		} else {
			color.Green("  ● %-11s [%-8s] OK", impl.Name(), impl.Tier())
		}
	}
	return nil
}

// runModels shows how each provider maps the three Claude tiers.
func runModels(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		color.Yellow("No providers configured. Add one, e.g.:  nexus add groq <key>")
		return nil
	}
	color.Cyan("Model mapping (Claude model → provider model):\n")
	for _, pc := range cfg.Providers {
		impl, err := providers.New(specFromConfig(pc))
		if err != nil {
			continue
		}
		color.White("  %-11s [%s]", impl.Name(), impl.Tier())
		fmt.Printf("      haiku  → %s\n", impl.MapModel("claude-haiku-4-5"))
		fmt.Printf("      sonnet → %s\n", impl.MapModel("claude-sonnet-4-6"))
		fmt.Printf("      opus   → %s\n", impl.MapModel("claude-opus-4-5"))
	}
	return nil
}

// specFromConfig builds a providers.Spec from a config entry, resolving env keys.
func runDoctor(cmd *cobra.Command, args []string) error {
	color.Cyan("🩺 NEXUS doctor\n")
	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	color.White("  Config:   %s", cfgPath)
	color.White("  Database: %s", storage.DefaultDBPath())

	disc := config.DiscoverFromEnv(cfg.Providers)
	if len(disc) > 0 {
		color.Green("\n  Auto-discovered %d provider(s) from your environment:", len(disc))
		for _, d := range disc {
			color.White("    • %s (from $%s)", d.Name, strings.TrimPrefix(d.APIKey, "env:"))
		}
	}
	all := append(append([]config.Provider{}, cfg.Providers...), disc...)

	if len(all) == 0 {
		color.Yellow("\n  No providers configured.")
		color.White("  Add one:   nexus add groq <key>        # free tier")
		color.White("  Or set an env var (e.g. GROQ_API_KEY) and re-run — NEXUS auto-detects it.")
		return nil
	}

	color.Cyan("\n  Providers:")
	var free, premium int
	for _, pc := range all {
		impl, err := providers.New(specFromConfig(pc))
		if err != nil {
			color.Red("    ✗ %-12s config error: %v", pc.Name, err)
			continue
		}
		start := time.Now()
		hcErr := checkProviderHealth(pc, impl)
		ms := time.Since(start).Milliseconds()
		switch impl.Tier() {
		case "free", "local":
			free++
		case "premium":
			premium++
		}
		if hcErr != nil {
			color.Red("    ✗ %-12s %-8s unreachable (%v)", pc.Name, impl.Tier(), hcErr)
		} else {
			color.Green("    ✓ %-12s %-8s %dms", pc.Name, impl.Tier(), ms)
		}
	}

	color.Cyan("\n  Checks:")
	if free > 0 {
		color.Green("    ✓ free/local tier available — simple tasks cost $0")
	} else {
		color.Yellow("    ! no free/local provider — add groq, gemini or ollama for $0 simple tasks")
	}
	if premium > 0 {
		color.Green("    ✓ premium tier available — complex tasks stay high-quality")
	} else {
		color.Yellow("    ! no premium provider — add anthropic for the hardest tasks")
	}
	color.Green("    ✓ %d provider(s) ready", len(all))
	color.White("\n  Tip: nexus start --cascade --redact --semantic-cache   # max savings + privacy")
	return nil
}

func specFromConfig(pc config.Provider) providers.Spec {
	key := config.ResolveKey(pc.APIKey)
	if key == "" && len(pc.APIKeys) > 0 {
		key = config.ResolveKey(pc.APIKeys[0])
	}
	return providers.Spec{
		Name:        pc.Name,
		Type:        pc.Type,
		APIKey:      key,
		BaseURL:     pc.BaseURL,
		Models:      pc.Models,
		Tier:        pc.Tier,
		ModelMap:    pc.ModelMap,
		InputPer1M:  pc.InputPer1M,
		OutputPer1M: pc.OutputPer1M,
		OffPeakInputPer1M:  pc.OffPeakInputPer1M,
		OffPeakOutputPer1M: pc.OffPeakOutputPer1M,
		OffPeakStartUTC:    pc.OffPeakStartUTC,
		OffPeakEndUTC:      pc.OffPeakEndUTC,
		Region:      pc.Region,
		Project:     pc.Project,
		APIVersion:  pc.APIVersion,
	}
}

func runLogs(cmd *cobra.Command, args []string) error {
	db, err := storage.New(storage.DefaultDBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	reqs, err := db.GetRecentRequests(20)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		color.Yellow("No requests logged yet.")
		return nil
	}

	color.Cyan("Recent requests:\n")
	for _, q := range reqs {
		fmt.Printf("  %s  %-9s  %-8s  %-18s  %d→%d tok  $%.5f  %dms  [%d]\n",
			q.CreatedAt.Local().Format("15:04:05"), q.Provider, q.Complexity, q.ModelAsked,
			q.InputTokens, q.OutputTokens, q.CostUSD, q.LatencyMS, q.Status)
	}
	return nil
}

func runCost(cmd *cobra.Command, args []string) error {
	db, err := storage.New(storage.DefaultDBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	today, _ := db.GetStats("today")
	week, _ := db.GetStats("week")
	forecast, _ := db.GetCostForecast()

	color.Cyan("Cost breakdown:\n")
	if today != nil {
		color.White("  Today:    $%.4f  (%d requests, %d tokens)",
			today.TotalCostUSD, today.TotalRequests, today.TotalInputTokens+today.TotalOutputTokens)
	}
	if week != nil {
		color.White("  Last 7d:  $%.4f  (%d requests)", week.TotalCostUSD, week.TotalRequests)
	}
	color.White("  Forecast: $%.2f / month", forecast)

	if bd, _ := db.GetProviderBreakdown(); len(bd) > 0 {
		color.Cyan("\n  By provider:")
		for _, p := range bd {
			color.White("    %-10s  $%.4f  (%d req)", p.Provider, p.TotalCostUSD, p.Requests)
		}
	}
	return nil
}

func runConfig(cmd *cobra.Command, args []string) error {
	path := config.DefaultPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		color.Yellow("No config yet at %s", path)
		color.White("  Create one by adding a provider:  nexus add groq <key>")
		return nil
	}
	color.Cyan("Config: %s", path)
	return nil
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// setupLogging configures the global zerolog logger with a pretty console writer.
func setupLogging(level string) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"})
}

// printReady prints the human-friendly "running" banner with connection hints.
func printReady(proxyPort, dashPort int, ui bool) {
	color.Green("\n✓ NEXUS running")
	color.White("  Proxy:     http://localhost:%d", proxyPort)
	if ui {
		color.White("  Dashboard: http://localhost:%d", dashPort)
	}
	color.Yellow("\n  Connect Claude Code:")
	color.White("  export ANTHROPIC_BASE_URL=http://localhost:%d", proxyPort)
	color.White("  export ANTHROPIC_API_KEY=nexus-local")

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		color.HiBlack("\n  Tip: set ANTHROPIC_API_KEY in this shell so NEXUS can reach Anthropic.")
	}
	fmt.Println()
}
