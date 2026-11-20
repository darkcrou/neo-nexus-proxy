// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// pagingServer starts a test upstream that answers the n-th request with
// body(n) and records every request it received, so a test can assert both the
// sequence of pages and the parameters each page carried.
func pagingServer(t *testing.T, body func(page int) string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
		})
		page := len(reqs) - 1
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body(page))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
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

// TestListModelsURLPerFamily pins the listing URL each family derives, including
// the two providers whose BaseURL is one segment short of its OpenAI /v1 root.
func TestListModelsURLPerFamily(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want string
	}{
		{"openai", NewOpenAI("key"), "https://api.openai.com/v1/models"},
		{"deepseek", NewDeepSeek("key"), "https://api.deepseek.com/models"},
		{"groq", NewGroq("key"), "https://api.groq.com/openai/v1/models"},
		{"mistral", NewMistral("key"), "https://api.mistral.ai/v1/models"},
		{"ollama", NewOllama("http://localhost:11434", ""), "http://localhost:11434/api/tags"},
		{"anthropic", NewAnthropic("key"), anthropicModelsURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := modelListSpecFor(tc.p, "key")
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

	spec, err := modelListSpecFor(NewAnthropic("sk-ant-test"), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	if spec.url != anthropicModelsURL {
		t.Errorf("url = %q, want %q", spec.url, anthropicModelsURL)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
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

	spec, err := modelListSpecFor(NewGemini("AIza-test key"), "AIza-test key")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	if want := geminiModelsURL + "?key=AIza-test+key"; spec.url != want {
		t.Errorf("url = %q, want %q", spec.url, want)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
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

// TestListModelsWrappedUnsupportedFamily pins the other half of unwrapping: a
// wrapped family with no listing endpoint still reports ErrNoModelList, so the
// caller keeps its configured fallback instead of probing a URL that cannot
// list.
func TestListModelsWrappedUnsupportedFamily(t *testing.T) {
	p, err := New(Spec{Type: "perplexity", APIKey: "k", Tier: "premium"})
	if err != nil {
		t.Fatalf("New(perplexity): %v", err)
	}
	if _, ok := p.(*overridden); !ok {
		t.Fatalf("New returned %T, want *overridden: the test only pins unwrapping while New keeps wrapping", p)
	}

	ids, err := ListModels(p, "k")
	if !errors.Is(err, ErrNoModelList) {
		t.Fatalf("err = %v, want ErrNoModelList", err)
	}
	if ids != nil {
		t.Errorf("ids = %v, want nil", ids)
	}
}

// TestListModelsAnthropicPaginates pins the Anthropic cursor walk: the first
// page asks for a full page with limit, and has_more/last_id continue with
// after_id until the listing is exhausted.
func TestListModelsAnthropicPaginates(t *testing.T) {
	srv, requests := pagingServer(t, func(page int) string {
		if page == 0 {
			return `{"data":[{"id":"claude-opus-4-6"}],"has_more":true,"last_id":"claude-opus-4-6"}`
		}
		return `{"data":[{"id":"claude-haiku-4-5"}],"has_more":false}`
	})
	spec, err := modelListSpecFor(NewAnthropic("sk-ant-test"), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"claude-opus-4-6", "claude-haiku-4-5"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].query != "limit=1000" {
		t.Errorf("first query = %q, want limit=1000", reqs[0].query)
	}
	if want := "after_id=claude-opus-4-6&limit=1000"; reqs[1].query != want {
		t.Errorf("second query = %q, want %q", reqs[1].query, want)
	}
	if got := reqs[1].header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key on page 2 = %q, want every page authenticated", got)
	}
}

// TestListModelsGeminiPaginates pins the Gemini cursor walk: pageSize on every
// page, nextPageToken continuing as pageToken, and the key carried on each page
// because the cursor never replaces the base query.
func TestListModelsGeminiPaginates(t *testing.T) {
	srv, requests := pagingServer(t, func(page int) string {
		if page == 0 {
			return `{"models":[{"name":"models/gemini-2.0-flash"}],"nextPageToken":"tok-2"}`
		}
		return `{"models":[{"name":"models/gemini-2.0-flash-lite"}]}`
	})
	spec, err := modelListSpecFor(NewGemini("AIza-test key"), "AIza-test key")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	spec.url = srv.URL + "?key=AIza-test+key"

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"gemini-2.0-flash", "gemini-2.0-flash-lite"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if want := "key=AIza-test+key&pageSize=1000"; reqs[0].query != want {
		t.Errorf("first query = %q, want %q", reqs[0].query, want)
	}
	if want := "key=AIza-test+key&pageSize=1000&pageToken=tok-2"; reqs[1].query != want {
		t.Errorf("second query = %q, want %q", reqs[1].query, want)
	}
}

// TestListModelsPaginationStopsOnRepeatedCursor pins the loop guard: an
// endpoint that keeps answering has_more with an unchanged cursor is followed
// exactly once, then abandoned.
func TestListModelsPaginationStopsOnRepeatedCursor(t *testing.T) {
	srv, requests := pagingServer(t, func(page int) string {
		return fmt.Sprintf(`{"data":[{"id":"model-%d"}],"has_more":true,"last_id":"stuck"}`, page)
	})
	spec, err := modelListSpecFor(NewAnthropic("sk-ant-test"), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"model-0", "model-1"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if got := len(requests()); got != 2 {
		t.Errorf("requests = %d, want 2 (the cursor did not advance)", got)
	}
}

// TestListModelsPaginationStopsOnRepeatedIDs pins the other loop guard: a page
// that contributes nothing new ends the walk even while the cursor advances.
func TestListModelsPaginationStopsOnRepeatedIDs(t *testing.T) {
	srv, requests := pagingServer(t, func(page int) string {
		return fmt.Sprintf(`{"models":[{"name":"models/gemini-2.0-flash"}],"nextPageToken":"tok-%d"}`, page+1)
	})
	spec, err := modelListSpecFor(NewGemini("AIza-test"), "AIza-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"gemini-2.0-flash"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if got := len(requests()); got != 2 {
		t.Errorf("requests = %d, want 2 (the second page added nothing new)", got)
	}
}

// TestListModelsPaginationCapsPages pins the request cap, so an endpoint that
// always has another page cannot spin.
func TestListModelsPaginationCapsPages(t *testing.T) {
	srv, requests := pagingServer(t, func(page int) string {
		return fmt.Sprintf(`{"data":[{"id":"model-%02d"}],"has_more":true,"last_id":"cursor-%d"}`, page, page)
	})
	spec, err := modelListSpecFor(NewAnthropic("sk-ant-test"), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := len(requests()); got != maxModelListPages {
		t.Errorf("requests = %d, want the %d-page cap", got, maxModelListPages)
	}
	if len(ids) != maxModelListPages {
		t.Errorf("len(ids) = %d, want %d", len(ids), maxModelListPages)
	}
}

// TestListModelsPaginationCapsTotal pins the id cap across pages, not per page.
func TestListModelsPaginationCapsTotal(t *testing.T) {
	const perPage = 300
	srv, requests := pagingServer(t, func(page int) string {
		var body strings.Builder
		body.WriteString(`{"data":[`)
		for i := 0; i < perPage; i++ {
			if i > 0 {
				body.WriteString(",")
			}
			fmt.Fprintf(&body, `{"id":"model-%02d-%03d"}`, page, i)
		}
		fmt.Fprintf(&body, `],"has_more":true,"last_id":"cursor-%d"}`, page)
		return body.String()
	})
	spec, err := modelListSpecFor(NewAnthropic("sk-ant-test"), "sk-ant-test")
	if err != nil {
		t.Fatalf("modelListSpecFor(anthropic): %v", err)
	}
	spec.url = srv.URL

	ids, err := spec.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(ids) != maxListedModels {
		t.Fatalf("len(ids) = %d, want %d", len(ids), maxListedModels)
	}
	if got := len(requests()); got > maxModelListPages {
		t.Errorf("requests = %d, want at most %d", got, maxModelListPages)
	}
}

// TestListModelsSinglePageFamiliesSendNoPagingParams pins that the families
// with one response per listing stay one request with no pagination parameters.
func TestListModelsSinglePageFamiliesSendNoPagingParams(t *testing.T) {
	srv, requests := pagingServer(t, func(int) string {
		return `{"data":[{"id":"m"}],"models":[{"name":"m"}]}`
	})

	groqSpec, err := modelListSpecFor(NewGroq("key"), "key")
	if err != nil {
		t.Fatalf("modelListSpecFor(groq): %v", err)
	}
	groqSpec.url = srv.URL + "/openai/v1/models"

	cases := map[string]Provider{
		"openai-compatible": openAICompatibleTestProvider(t, srv.URL),
		"ollama":            NewOllama(srv.URL, ""),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(requests())
			if _, err := ListModels(p, "key"); err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			reqs := requests()[before:]
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(reqs))
			}
			if reqs[0].query != "" {
				t.Errorf("query = %q, want no pagination parameters", reqs[0].query)
			}
		})
	}
	t.Run("groq", func(t *testing.T) {
		before := len(requests())
		if _, err := groqSpec.fetch(context.Background()); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		reqs := requests()[before:]
		if len(reqs) != 1 {
			t.Fatalf("requests = %d, want 1", len(reqs))
		}
		if reqs[0].query != "" {
			t.Errorf("query = %q, want no pagination parameters", reqs[0].query)
		}
		if reqs[0].path != "/openai/v1/models" {
			t.Errorf("path = %q, want /openai/v1/models", reqs[0].path)
		}
	})
}

// TestListModelsGeminiErrorHidesKey asserts a failing listing never echoes the
// query-string key back into the error.
func TestListModelsGeminiErrorHidesKey(t *testing.T) {
	srv, _ := modelServer(t, http.StatusInternalServerError, `{}`)

	spec, err := modelListSpecFor(NewGemini("AIzaVerySecretKey"), "AIzaVerySecretKey")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	spec.url = srv.URL + "?key=AIzaVerySecretKey"

	_, err = spec.fetch(context.Background())
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

// TestListModelsWrappedOllamaUsesTagsEndpoint pins that the config-override
// wrapper does not hide the Ollama family. New wraps a built-in in *overridden
// whenever the spec carries overrides, so ListModels must unwrap before it
// dispatches or the provider silently falls to the generic /models recipe.
func TestListModelsWrappedOllamaUsesTagsEndpoint(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"models":[{"name":"llama3:8b"},{"name":"codellama:13b"}]}`)
	p, err := New(Spec{Type: "ollama", BaseURL: srv.URL, ModelMap: map[string]string{"default": "llama3:8b"}})
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}
	if _, ok := p.(*overridden); !ok {
		t.Fatalf("New returned %T, want *overridden: the test only pins unwrapping while New keeps wrapping", p)
	}

	ids, err := ListModels(p, "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"llama3:8b", "codellama:13b"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if rec.path != "/api/tags" {
		t.Errorf("path = %q, want /api/tags, not the OpenAI /models shim", rec.path)
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

// TestListModelsUnsupportedFamilyIgnoresInstanceName pins that the listing
// recipe follows the provider type, not its instance name: an enterprise
// provider is unsupported however the operator named it, so the caller keeps
// its configured fallback instead of probing an endpoint that cannot work.
func TestListModelsUnsupportedFamilyIgnoresInstanceName(t *testing.T) {
	azure, err := New(Spec{Name: "prod-use1", Type: "azure", BaseURL: "https://acme.openai.azure.com/openai/deployments/dep", APIKey: "key"})
	if err != nil {
		t.Fatalf("New(azure): %v", err)
	}
	vertex, err := New(Spec{Name: "prod-use1", Type: "vertex", Region: "us-central1", Project: "proj"})
	if err != nil {
		t.Fatalf("New(vertex): %v", err)
	}
	bedrock, err := New(Spec{Name: "prod-use1", Type: "bedrock", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("New(bedrock): %v", err)
	}

	cases := map[string]Provider{"azure": azure, "vertex": vertex, "bedrock": bedrock}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ListModels(p, "key"); !errors.Is(err, ErrNoModelList) {
				t.Fatalf("err = %v, want ErrNoModelList for a %s provider named prod-use1", err, name)
			}
		})
	}
}

// TestListModelsInstanceNameCollisionKeepsOwnEndpoint pins the opposite
// direction: a custom provider whose instance name collides with a family
// keyword is still listed through its own BaseURL() with its own bearer token,
// never through that family's fixed endpoint.
func TestListModelsInstanceNameCollisionKeepsOwnEndpoint(t *testing.T) {
	srv, rec := modelServer(t, http.StatusOK, `{"data":[{"id":"acme-large"}]}`)
	p, err := New(Spec{Name: "gemini", Type: "custom", BaseURL: srv.URL, APIKey: "acme-key"})
	if err != nil {
		t.Fatalf("New(custom): %v", err)
	}

	ids, err := ListModels(p, "acme-key")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"acme-large"}; !equalIDs(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if rec.method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.method)
	}
	if rec.path != "/models" {
		t.Errorf("path = %q, want /models on the provider's own base URL", rec.path)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer acme-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer acme-key")
	}
}

// TestListModelsTransportErrorHidesKey pins that a transport failure hides the
// query-string key too, not only a non-2xx reply: http.Client.Do returns a
// *url.Error whose message embeds the request URL, key included.
func TestListModelsTransportErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close() // a closed listener makes every request fail at the transport layer

	spec, err := modelListSpecFor(NewGemini("AIzaVerySecretKey"), "AIzaVerySecretKey")
	if err != nil {
		t.Fatalf("modelListSpecFor(gemini): %v", err)
	}
	spec.url = closedURL + "/v1beta/models?key=AIzaVerySecretKey"

	_, err = spec.fetch(context.Background())
	if err == nil {
		t.Fatal("expected an error when the listing endpoint is unreachable")
	}
	if strings.Contains(err.Error(), "AIzaVerySecretKey") {
		t.Errorf("error %q leaks the api key", err)
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("error %q does not name the endpoint", err)
	}
}

// TestListModelsNilProvider pins the nil guard: a caller that has not resolved
// a provider gets an error rather than a panic.
func TestListModelsNilProvider(t *testing.T) {
	ids, err := ListModels(nil, "key")
	if err == nil {
		t.Fatal("expected an error for a nil provider")
	}
	if ids != nil {
		t.Errorf("ids = %v, want nil", ids)
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
