package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/config"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
)

type fakeQwenSession struct {
	approved  chan struct{}
	connected atomic.Bool
}

func (f *fakeQwenSession) BeginLogin(context.Context) (qwen.Flow, error) {
	return qwen.Flow{ID: "mock-login", URL: "https://chat.qwen.ai/authorize?user_code=example", UserCode: "example"}, nil
}

func (f *fakeQwenSession) FinishLogin(ctx context.Context, _ qwen.Flow) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.approved:
		f.connected.Store(true)
		return nil
	}
}

func (f *fakeQwenSession) Status(context.Context) (qwen.Status, error) {
	if f.connected.Load() {
		return qwen.Status{Revision: "mock-login", Connected: true}, nil
	}
	return qwen.Status{}, nil
}

func (f *fakeQwenSession) CancelLogin(context.Context, string) (bool, error) {
	return !f.connected.Load(), nil
}

func qwenRequest(h http.Handler, method, path, origin, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://nexus.example/api/qwen/"+path, nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestQwenDashboardAuthorizesAndSavesProvider(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	session := &fakeQwenSession{approved: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(path, &config.Config{Providers: []config.Provider{{Name: "groq", APIKey: "mock-static-key"}}}); err != nil {
		t.Fatal(err)
	}
	dash := &Server{qwen: &qwenSetup{
		ctx: ctx, stop: stop, session: session, path: path, secret: "operator-test",
		ready: func(string) (bool, int) { return false, 0 },
	}}
	h := dash.Routes()
	const origin = "https://nexus.example"
	if rec := qwenRequest(h, http.MethodGet, "status", origin, ""); rec.Code != http.StatusUnauthorized {
		t.Fatal(rec.Code, rec.Body)
	}
	for _, path := range []string{"status", "flow"} {
		method := http.MethodGet
		if path == "flow" {
			method = http.MethodPost
		}
		rec := qwenRequest(h, method, path, "https://another.example", "operator-test")
		if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal(rec.Code, rec.Header())
		}
	}
	rec := qwenRequest(h, http.MethodPost, "flow", origin, "operator-test")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "user_code") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Code, rec.Body)
	}
	close(session.approved)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := qwenRequest(h, http.MethodGet, "status", origin, "operator-test")
		if strings.Contains(status.Body.String(), `"connected":true`) && !strings.Contains(status.Body.String(), `"flow"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(status.Code, status.Body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cfg, err := config.Load(path)
	if err != nil || len(cfg.Providers) != 2 || cfg.Providers[1].Type != qwen.ProviderType || cfg.Providers[0].APIKey != "mock-static-key" {
		t.Fatal("sign-in must add Qwen while preserving existing providers", err)
	}
}

func TestQwenDashboardWithoutStore(t *testing.T) {
	dash := &Server{}
	dash.ConfigureQwen(context.Background(), nil, "operator-test", "", nil)
	defer dash.Shutdown()
	rec := qwenRequest(dash.Routes(), http.MethodGet, "status", "https://nexus.example", "operator-test")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal(rec.Code, rec.Body)
	}
}
