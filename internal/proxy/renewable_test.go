package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/qwen"
)

type testBearer struct {
	token    string
	rejected []string
}

func (s *testBearer) Token(_ context.Context, rejected string) (string, error) {
	if rejected != "" {
		s.rejected = append(s.rejected, rejected)
		s.token = "renewed"
	}
	return s.token, nil
}

type renewableRoundTrip func(*http.Request) (*http.Response, error)

func (f renewableRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSubscriptionRefreshKeepsModelStickyAndQuotaStillFailsOver(t *testing.T) {
	const model = "qwen-reported-model"
	peer := openAIServer(http.StatusOK, "from peer")
	defer peer.Close()
	h := buildTestHandler(t, []testProv{
		{qwen.ProviderType, "free", qwen.GatewayURL},
		{"peer", "free", peer.URL},
	})
	source := &testBearer{token: "original"}
	p, err := providers.New(providers.Spec{Type: qwen.ProviderType, QwenTokens: source})
	if err != nil {
		t.Fatal(err)
	}
	ap := h.providers[qwen.ProviderType]
	ap.impl = p
	ap.keys = []string{""}
	ap.cool = make([]time.Time, 1)
	ap.backoff = make([]time.Duration, 1)
	h.router.SetProviderModels(qwen.ProviderType, []string{model})
	h.router.SetProviderModels("peer", []string{model})

	var attempts int
	var exhausted bool
	h.httpClient.Transport = renewableRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.qwen.ai" {
			return http.DefaultTransport.RoundTrip(r)
		}
		attempts++
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != model {
			t.Errorf("provider received model %q, decode error %v", body.Model, err)
		}
		r.Body.Close()
		code := http.StatusOK
		content := `{"id":"x","model":"qwen-reported-model","choices":[{"message":{"role":"assistant","content":"from qwen"},"finish_reason":"stop"}]}`
		switch {
		case exhausted:
			code, content = http.StatusPaymentRequired, `{"error":{"code":"insufficient_quota"}}`
		case r.Header.Get("Authorization") == "Bearer original":
			code, content = http.StatusUnauthorized, `{"error":{"code":"invalid_token"}}`
		case r.Header.Get("Authorization") != "Bearer renewed":
			t.Error("expected renewed bearer token")
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(content))}, nil
	})

	body := `{"model":"` + model + `","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	if rec := doMessages(h, body); rec.Code != 200 || rec.Header().Get("X-Nexus-Provider") != qwen.ProviderType || !strings.Contains(rec.Body.String(), "from qwen") {
		t.Fatal(rec.Code, rec.Body)
	}
	if attempts != 2 || len(source.rejected) != 1 || source.rejected[0] != "original" {
		t.Fatalf("attempts %d, refresh calls %d", attempts, len(source.rejected))
	}
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 || rec.Header().Get("X-Nexus-Provider") != qwen.ProviderType {
		t.Fatal(rec.Code, rec.Body)
	}
	exhausted = true
	if rec := doMessages(h, body); rec.Code != 200 || rec.Header().Get("X-Nexus-Provider") != "peer" {
		t.Fatal(rec.Code, rec.Body)
	}
	if len(source.rejected) != 1 || ap.hasAvailableKey() {
		t.Fatal("quota exhaustion should cool the account without refreshing")
	}
}

func TestSubscriptionRepeatedUnauthorizedReturnsSignInGuidance(t *testing.T) {
	h := buildTestHandler(t, []testProv{{qwen.ProviderType, "free", qwen.GatewayURL}})
	h.cascade = true
	source := &testBearer{token: "first"}
	p, err := providers.New(providers.Spec{Type: qwen.ProviderType, QwenTokens: source})
	if err != nil {
		t.Fatal(err)
	}
	h.providers[qwen.ProviderType].impl = p
	var requests int
	h.httpClient.Transport = renewableRoundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: http.NoBody}, nil
	})

	rec := doMessages(h, `{"model":"qwen-model","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Connect Qwen") {
		t.Fatal(rec.Code, rec.Body)
	}
	if requests != 2 || len(source.rejected) != 1 {
		t.Fatalf("requests %d, refresh calls %d", requests, len(source.rejected))
	}
}

func TestSubscriptionRetryBudgetSurvivesTransportRetryAndReportsRelogin(t *testing.T) {
	const model = "qwen-model"
	peer := openAIServer(http.StatusServiceUnavailable, "")
	defer peer.Close()
	h := buildTestHandler(t, []testProv{{qwen.ProviderType, "free", qwen.GatewayURL}, {"peer", "free", peer.URL}})
	h.router.SetProviderModels(qwen.ProviderType, []string{model})
	h.router.SetProviderModels("peer", []string{model})
	source := &testBearer{token: "first"}
	p, err := providers.New(providers.Spec{Type: qwen.ProviderType, QwenTokens: source})
	if err != nil {
		t.Fatal(err)
	}
	h.providers[qwen.ProviderType].impl = p

	var requests int
	h.httpClient.Transport = renewableRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.qwen.ai" {
			return http.DefaultTransport.RoundTrip(r)
		}
		requests++
		code := http.StatusUnauthorized
		if requests == 2 {
			code = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: http.NoBody}, nil
	})
	rec := doMessages(h, `{"model":"qwen-model","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Connect Qwen") {
		t.Fatal(rec.Code, rec.Body)
	}
	if requests != 3 || len(source.rejected) != 1 {
		t.Fatalf("requests %d, refresh calls %d", requests, len(source.rejected))
	}
}
