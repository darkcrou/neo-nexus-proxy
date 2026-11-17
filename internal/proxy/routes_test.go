package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// newServerWithMock builds a real proxy.Server whose only provider points at an
// in-process mock, so every route can be exercised end-to-end through the router.
func newServerWithMock(t *testing.T) *Server {
	t.Helper()
	scrubProviderEnv(t)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "mock",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"}}},
			"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	t.Cleanup(mock.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Warming is off so the id set /v1/models answers with is the config seed alone: a
	// background discovery pass against the mock could change it mid-assertion.
	cfg := "[catalog]\nenabled = false\n\n[routing]\nstrategy = \"auto\"\n\n[[providers]]\nname = \"mock\"\ntype = \"openai-compatible\"\nbase_url = \"" + mock.URL + "\"\ntier = \"free\"\nmodels = [\"mock-model\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	srv, err := New(&Config{ConfigPath: cfgPath, DisableCache: true}, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	return srv
}

func TestRoutes_AllProxyEndpoints(t *testing.T) {
	h := newServerWithMock(t).Routes()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	t.Run("health", func(t *testing.T) {
		rec := do("GET", "/health", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Errorf("got %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"providers":1`) {
			t.Errorf("health should report 1 provider: %s", rec.Body.String())
		}
	})

	t.Run("models", func(t *testing.T) {
		rec := do("GET", "/v1/models", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"object":"list"`) {
			t.Errorf("got %d %s", rec.Code, rec.Body.String())
		}
		// The configured provider's models are the catalog's seed, so they are what the
		// endpoint advertises when discovery has nothing newer.
		if !strings.Contains(rec.Body.String(), `"id":"mock-model"`) {
			t.Errorf("configured model missing from the list: %s", rec.Body.String())
		}
	})

	t.Run("models rejects POST with the catch-all 404", func(t *testing.T) {
		rec := do("POST", "/v1/models", "")
		if rec.Code != 404 {
			t.Errorf("POST /v1/models = %d, want the catch-all 404 — GET is the only accepted method", rec.Code)
		}
	})

	t.Run("messages (anthropic in, openai provider, converted back)", func(t *testing.T) {
		rec := do("POST", "/v1/messages", `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Nexus-Provider") != "mock" {
			t.Errorf("provider header = %q", rec.Header().Get("X-Nexus-Provider"))
		}
		var resp AnthropicResponse
		if json.Unmarshal(rec.Body.Bytes(), &resp); len(resp.Content) == 0 || resp.Content[0].Text != "ok" {
			t.Errorf("anthropic content = %+v", resp.Content)
		}
	})

	t.Run("chat completions (openai passthrough)", func(t *testing.T) {
		rec := do("POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		var resp map[string]interface{}
		if json.Unmarshal(rec.Body.Bytes(), &resp); resp["object"] != "chat.completion" {
			t.Errorf("not an OpenAI completion: %s", rec.Body.String())
		}
	})

	t.Run("unknown route → 404", func(t *testing.T) {
		if rec := do("GET", "/nope", ""); rec.Code != 404 {
			t.Errorf("got %d, want 404", rec.Code)
		}
	})

	t.Run("wrong method → 4xx", func(t *testing.T) {
		if rec := do("GET", "/v1/messages", ""); rec.Code < 400 {
			t.Errorf("GET /v1/messages = %d, want 4xx", rec.Code)
		}
	})
}
