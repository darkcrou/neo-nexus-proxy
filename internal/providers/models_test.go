// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package providers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedRequest captures what an upstream test server actually received.
type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
}

// modelServer starts a test upstream that records the request and replies with
// the given status and body.
func modelServer(t *testing.T, status int, body string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// openAICompatibleTestProvider builds a config-driven OpenAI-compatible
// provider pointing at baseURL, which is how the majority of providers reach
// ListModels.
func openAICompatibleTestProvider(t *testing.T, baseURL string) Provider {
	t.Helper()
	p, err := New(Spec{Name: "acme", Type: "custom", BaseURL: baseURL, APIKey: "secret"})
	if err != nil {
		t.Fatalf("New(custom): %v", err)
	}
	return p
}

// TestListModelsOpenAICompatibleDefault covers the default family: a
// /models GET with a bearer token, ids returned in the provider's own order.
func TestListModelsOpenAICompatibleDefault(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"data":[{"id":"beta"},{"id":"alpha"}]}`)
	p := openAICompatibleTestProvider(t, srv.URL)

	ids, err := ListModels(p, "secret")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"beta", "alpha"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v (provider order must be preserved)", ids, want)
	}
	if rec.method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.method)
	}
	if rec.path != "/models" {
		t.Errorf("path = %q, want /models", rec.path)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret")
	}
}

// TestModelListURLPerFamily pins the listing URL each family derives, including
// the two providers whose BaseURL is one segment short of its OpenAI /v1 root.
func TestModelListURLPerFamily(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"openai", "https://api.openai.com/v1", "https://api.openai.com/v1/models"},
		{"deepseek", "https://api.deepseek.com", "https://api.deepseek.com/models"},
		{"groq", "https://api.groq.com/openai", "https://api.groq.com/openai/v1/models"},
		{"mistral", "https://api.mistral.ai/v1", "https://api.mistral.ai/v1/models"},
		{"ollama", "http://localhost:11434/v1", "http://localhost:11434/api/tags"},
		{"anthropic", "https://api.anthropic.com", anthropicModelsURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := modelListSpecFor(tc.name, tc.baseURL, "key")
			if err != nil {
				t.Fatalf("modelListSpecFor(%q): %v", tc.name, err)
			}
			if spec.url != tc.want {
				t.Errorf("url = %q, want %q", spec.url, tc.want)
			}
		})
	}
}

// TestListModelsAnthropic covers the native Anthropic family. Its BaseURL()
// hardcodes the host without the /v1 prefix, so the test drives the extracted
// request/parse helper against a test server rather than ListModels itself.
func TestListModelsAnthropic(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"data":[{"id":"claude-opus-4-6"},{"id":"claude-haiku-4-5"}]}`)

	spec, err := modelListSpecFor("anthropic", NewAnthropic("sk-ant-test").BaseURL(), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	if spec.url != anthropicModelsURL {
		t.Errorf("url = %q, want %q", spec.url, anthropicModelsURL)
	}
	spec.url = srv.URL

	ids, err := spec.fetch()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"claude-opus-4-6", "claude-haiku-4-5"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if got := rec.header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want %q", got, "sk-ant-test")
	}
	if got := rec.header.Get("anthropic-version"); got != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicAPIVersion)
	}
	if got := rec.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want no bearer header for anthropic", got)
	}
}

// TestListModelsGemini covers the Gemini family: the key travels in the query
// string and the "models/" prefix is stripped from every id.
func TestListModelsGemini(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-2.0-flash-lite"}]}`)

	spec, err := modelListSpecFor("gemini", NewGemini("AIza-test key").BaseURL(), "AIza-test key")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	if want := geminiModelsURL + "?key=AIza-test+key"; spec.url != want {
		t.Errorf("url = %q, want %q", spec.url, want)
	}
	spec.url = srv.URL

	ids, err := spec.fetch()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"gemini-2.0-flash", "gemini-2.0-flash-lite"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if rec.method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.method)
	}
}

// TestListModelsGeminiErrorHidesKey asserts a failing listing never echoes the
// query-string key back into the error.
func TestListModelsGeminiErrorHidesKey(t *testing.T) {
	srv, _ := modelServer(t, http.StatusInternalServerError, `{}`)

	spec, err := modelListSpecFor("gemini", "", "AIzaVerySecretKey")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	spec.url = srv.URL + "?key=AIzaVerySecretKey"

	_, err = spec.fetch()
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the status code", err)
	}
	if strings.Contains(err.Error(), "AIzaVerySecretKey") {
		t.Errorf("error %q leaks the api key", err)
	}
}

// TestListModelsOllamaTags covers the Ollama family end to end: the native
// /api/tags endpoint, not the OpenAI shim under /v1.
func TestListModelsOllamaTags(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"models":[{"name":"llama3:8b"},{"name":"codellama:13b"}]}`)
	p := NewOllama(srv.URL, "")

	ids, err := ListModels(p, "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"llama3:8b", "codellama:13b"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if rec.path != "/api/tags" {
		t.Errorf("path = %q, want /api/tags", rec.path)
	}
}

// TestListModelsTrimsAndDropsEmptyIDs asserts whitespace is trimmed and blank
// entries are dropped without reordering the rest.
func TestListModelsTrimsAndDropsEmptyIDs(t *testing.T) {
	srv, _ := modelServer(t, http.StatusOK, `{"data":[{"id":"  alpha  "},{"id":""},{"id":"   "},{"id":"beta\n"}]}`)
	p := openAICompatibleTestProvider(t, srv.URL)

	ids, err := ListModels(p, "secret")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"alpha", "beta"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

// TestListModelsStatusError asserts a non-2xx reply is reported with its status.
func TestListModelsStatusError(t *testing.T) {
	srv, _ := modelServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	p := openAICompatibleTestProvider(t, srv.URL)

	_, err := ListModels(p, "secret")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the status code", err)
	}
}

// TestListModelsMalformedBody asserts an unparsable listing is an error rather
// than an empty success, so the caller backs off instead of trusting it.
func TestListModelsMalformedBody(t *testing.T) {
	srv, _ := modelServer(t, http.StatusOK, `{"data":[{"id":`)
	p := openAICompatibleTestProvider(t, srv.URL)

	if _, err := ListModels(p, "secret"); err == nil {
		t.Fatal("expected an error for a malformed body")
	}
}

// TestListModelsUnsupportedFamilies asserts families without a listing endpoint
// report ErrNoModelList, which the caller treats as "keep the fallback".
func TestListModelsUnsupportedFamilies(t *testing.T) {
	azure, err := New(Spec{Name: "azure", Type: "azure", BaseURL: "https://acme.openai.azure.com/openai/deployments/dep", APIKey: "key"})
	if err != nil {
		t.Fatalf("New(azure): %v", err)
	}
	vertex, err := New(Spec{Name: "vertex", Type: "vertex", Region: "us-central1", Project: "proj"})
	if err != nil {
		t.Fatalf("New(vertex): %v", err)
	}
	bedrock, err := New(Spec{Name: "bedrock", Type: "bedrock", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("New(bedrock): %v", err)
	}

	cases := map[string]Provider{
		"perplexity": NewPerplexity("key"),
		"azure":      azure,
		"vertex":     vertex,
		"bedrock":    bedrock,
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			ids, err := ListModels(p, "key")
			if !errors.Is(err, ErrNoModelList) {
				t.Fatalf("err = %v, want ErrNoModelList", err)
			}
			if ids != nil {
				t.Errorf("ids = %v, want nil", ids)
			}
		})
	}
}

// TestListModelsCapsResultSize asserts the parsed list is capped so a runaway
// upstream cannot bloat the catalog.
func TestListModelsCapsResultSize(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"data":[`)
	for i := 0; i < maxListedModels+100; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"model-%04d"}`, i)
	}
	body.WriteString(`]}`)

	srv, _ := modelServer(t, http.StatusOK, body.String())
	p := openAICompatibleTestProvider(t, srv.URL)

	ids, err := ListModels(p, "secret")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != maxListedModels {
		t.Fatalf("len(ids) = %d, want %d", len(ids), maxListedModels)
	}
	if ids[0] != "model-0000" {
		t.Errorf("ids[0] = %q, want model-0000", ids[0])
	}
	if want := fmt.Sprintf("model-%04d", maxListedModels-1); ids[len(ids)-1] != want {
		t.Errorf("ids[last] = %q, want %q", ids[len(ids)-1], want)
	}
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
