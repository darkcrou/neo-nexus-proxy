package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog/log"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// Config holds proxy server configuration
type Config struct {
	Port           int
	DashboardPort  int
	LogLevel       string
	ConfigPath     string
	DailyBudgetUSD float64 // overrides config.toml routing.daily_budget_usd when > 0
	DisableCache   bool    // turn off the response cache
	SemanticCache  bool    // enable near-match (semantic) response caching
	SemanticThreshold float64 // cosine threshold for semantic cache (0 ⇒ 0.95)
	Cascade        bool    // cheap-first cascade with verification
	Adaptive       bool    // learned routing: prefer historically-best provider per task
	Redact         bool    // privacy firewall: mask secrets/PII before forwarding
	Inspect        bool    // capture full prompts/responses for the inspector + replay
	AlertWebhook   string  // Slack/Discord/generic webhook for budget alerts
	AlertThreshold float64 // fraction of budget that triggers a warning (0 ⇒ 0.8)
	MaxRequestUSD  float64 // guardrail: downgrade a single request estimated above this to free/local

	QwenSession *qwen.Service
}

// Server is the main proxy server
type Server struct {
	config  *Config
	router  *mux.Router
	handler *Handler
	srv     *http.Server
}

// New creates a new proxy server with shared storage and an event broker.
func New(cfg *Config, db *storage.DB, broker EventPublisher) (*Server, error) {
	h, err := NewHandler(cfg, db, broker)
	if err != nil {
		return nil, fmt.Errorf("failed to create handler: %w", err)
	}

	r := mux.NewRouter()
	s := &Server{
		config:  cfg,
		router:  r,
		handler: h,
	}

	s.registerRoutes()
	return s, nil
}

// Routes returns the proxy's HTTP handler. Exposed for end-to-end route tests.
func (s *Server) Routes() http.Handler { return s.router }

// registerRoutes sets up all proxy routes
func (s *Server) registerRoutes() {
	// Main Anthropic API endpoint — Claude Code uses this
	s.router.HandleFunc("/v1/messages", s.handler.HandleMessages).Methods("POST")

	// OpenAI-compatible gateway — Cursor, aider, Continue, Cline, any OpenAI SDK app
	s.router.HandleFunc("/v1/chat/completions", s.handler.HandleChatCompletions).Methods("POST")
	s.router.HandleFunc("/v1/models", s.handler.HandleModels).Methods("GET")

	// Health check
	s.router.HandleFunc("/health", s.handleHealth).Methods("GET")

	// Catch-all for debugging unknown routes
	s.router.PathPrefix("/").HandlerFunc(s.handleUnknown)
}

// Start starts the proxy server
func (s *Server) Start(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      s.router,
		ReadTimeout:  5 * time.Minute, // long for streaming
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	log.Info().Int("port", s.config.Port).Msg("Proxy server starting")

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("proxy server error: %w", err)
	case <-ctx.Done():
		return s.Shutdown()
	}
}

// Shutdown gracefully shuts down the server and stops handler background work.
// The shared DB is owned and closed by the caller (main), not here.
func (s *Server) Shutdown() error {
	var err error
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = s.srv.Shutdown(ctx)
	}
	if s.handler != nil {
		_ = s.handler.Close()
	}
	return err
}

// handleHealth responds to health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","service":"nexus","providers":%d,"cache":%t}`,
		s.handler.ProviderCount(), s.handler.CacheEnabled())
}

// handleUnknown logs and rejects unknown routes
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	log.Warn().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("Unknown route")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"error":{"type":"not_found","message":"Route not found in NEXUS proxy"}}`))
}
