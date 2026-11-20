// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/mux"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
)

const qwenAPIPath = "/api/qwen/"

type qwenSession interface {
	BeginLogin(context.Context) (qwen.Flow, error)
	FinishLogin(context.Context, qwen.Flow) error
	Status(context.Context) (qwen.Status, error)
}

type qwenSetup struct {
	session qwenSession
	secret  string
	path    string
	ready   func(string) (bool, int)
	ctx     context.Context
	stop    context.CancelFunc

	mu      sync.Mutex
	flow    *qwen.Flow
	cancel  context.CancelFunc
	message string
}

// ConfigureQwen installs the operator-only Qwen controls before Start.
func (s *Server) ConfigureQwen(ctx context.Context, session *qwen.Service, secret, configPath string, ready func(string) (bool, int)) {
	ctx, stop := context.WithCancel(ctx)
	s.qwen = &qwenSetup{secret: secret, path: configPath, ready: ready, ctx: ctx, stop: stop}
	if session != nil {
		s.qwen.session = session
	}
}

func (q *qwenSetup) routes(r *mux.Router) {
	s := r.PathPrefix(qwenAPIPath).Subrouter()
	s.Use(q.authorize)
	s.HandleFunc("/status", q.status).Methods(http.MethodGet)
	s.HandleFunc("/flow", q.start).Methods(http.MethodPost)
	s.HandleFunc("/flow/{id}", q.cancelFlow).Methods(http.MethodDelete)
}

func sameDashboardOrigin(r *http.Request) bool {
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite == "cross-site" || fetchSite == "same-site" {
		return false
	}
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return err == nil && u.User == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

func (q *qwenSetup) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !sameDashboardOrigin(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "Open the NEXUS dashboard directly to connect Qwen."})
			return
		}
		if q == nil || q.secret == "" {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "Set NEXUS_DASHBOARD_ADMIN_TOKEN on the NEXUS deployment to enable dashboard sign-in."})
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(q.secret)) != 1 {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "Enter the NEXUS dashboard operator token."})
			return
		}
		if q.session == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "Qwen session storage is unavailable. Check the NEXUS server log."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (q *qwenSetup) status(w http.ResponseWriter, r *http.Request) {
	status, err := q.session.Status(r.Context())
	if err != nil {
		slog.Warn("read Qwen status", "error", err)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "Could not read the Qwen session."})
		return
	}
	loaded, models := q.ready(status.Revision)
	q.mu.Lock()
	defer q.mu.Unlock()
	writeJSON(w, struct {
		Connected bool       `json:"connected"`
		Loaded    bool       `json:"loaded"`
		Models    int        `json:"models"`
		Flow      *qwen.Flow `json:"flow,omitempty"`
		Message   string     `json:"message,omitempty"`
	}{status.Connected, loaded, models, q.flow, q.message})
}

func (q *qwenSetup) start(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	flow, err := q.session.BeginLogin(r.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "Could not start Qwen sign-in. Try again."})
		return
	}
	if q.cancel != nil {
		q.cancel()
	}
	ctx, cancel := context.WithCancel(q.ctx)
	q.flow, q.cancel, q.message = &flow, cancel, ""
	go q.finish(ctx, flow)
	writeJSONStatus(w, http.StatusAccepted, flow)
}

func (q *qwenSetup) finish(ctx context.Context, flow qwen.Flow) {
	err := q.session.FinishLogin(ctx, flow)
	message := ""
	switch {
	case err == nil:
		if err := qwen.EnableProvider(q.path); err != nil {
			slog.Warn("enable Qwen provider", "error", err)
			message = "Signed in, but NEXUS could not write the provider entry. Check config.toml and the server log."
		}
	case errors.Is(err, context.Canceled):
		message = "Qwen sign-in cancelled."
	case errors.Is(err, qwen.ErrDenied), errors.Is(err, qwen.ErrExpired), errors.Is(err, qwen.ErrMembership), errors.Is(err, qwen.ErrSuperseded):
		message = err.Error()
	default:
		slog.Warn("Qwen sign-in failed", "error", err)
		message = "Qwen sign-in failed. Try again."
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.flow != nil && q.flow.ID == flow.ID {
		q.cancel()
		q.flow, q.cancel, q.message = nil, nil, message
	}
}

func (q *qwenSetup) cancelFlow(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.flow == nil || q.flow.ID != mux.Vars(r)["id"] {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "This sign-in attempt has already ended."})
		return
	}
	q.cancel()
	w.WriteHeader(http.StatusNoContent)
}
