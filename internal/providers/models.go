// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNoModelList reports that a provider has no endpoint that lists its models
// at all, as opposed to a listing that was attempted and failed. Callers keep
// their configured fallback list for this error and back off only for a real
// failure.
var ErrNoModelList = errors.New("provider exposes no model list endpoint")

const (
	modelListTimeout = 5 * time.Second
	// maxListedModels caps how many ids a single listing may contribute.
	maxListedModels = 2000
	// maxModelListPages caps how many requests a paginated listing may make,
	// so a misbehaving endpoint cannot spin.
	maxModelListPages = 10
	// maxModelListBytes caps the response body read from one listing page.
	maxModelListBytes = 8 << 20
	// modelListPageSize asks a paginated endpoint for its largest page, so a
	// full catalog costs as few requests as possible.
	modelListPageSize = "1000"

	anthropicModelsURL  = "https://api.anthropic.com/v1/models"
	geminiModelsURL     = "https://generativelanguage.googleapis.com/v1beta/models"
	anthropicAPIVersion = "2023-06-01"
)

// ListModels returns the model ids the provider currently offers, using the
// endpoint and authentication its family needs. Ids are returned in the
// provider's own order, trimmed, without blanks, and capped at maxListedModels.
//
// A provider that cannot list its models returns ErrNoModelList; any other
// error means the listing failed. The caller owns caching and scheduling.
func ListModels(p Provider, apiKey string) ([]string, error) {
	return ListModelsContext(context.Background(), p, apiKey)
}

func ListModelsContext(ctx context.Context, p Provider, apiKey string) ([]string, error) {
	if p == nil {
		return nil, errors.New("list models: nil provider")
	}
	spec, err := modelListSpecFor(p, apiKey)
	if err != nil {
		return nil, err
	}
	spec.source = BearerSourceFor(p)
	if spec.source != nil {
		spec.origin = p.BaseURL()
	}
	return spec.fetch(ctx)
}

// modelListSpec is one provider family's listing recipe: where to ask, how to
// authenticate, how to read the answer, and what to call the endpoint in errors.
type modelListSpec struct {
	url    string
	label  string            // provider name used in errors; never a URL carrying a secret
	apiKey string            // sent as a bearer token when bearer is set
	bearer bool              // the family authenticates with Authorization: Bearer
	header map[string]string // family-specific auth headers
	paging *pagination       // nil for a family that answers in one page
	parse  func([]byte) (modelPage, error)
	source BearerSource
	origin string
}

// pagination describes how a paginated family continues past its first page.
// Both paginated families use the same shape — a page-size parameter plus an
// opaque continuation cursor — under different parameter names.
type pagination struct {
	sizeParam   string // page-size query parameter
	cursorParam string // continuation-cursor query parameter
}

// pageURL returns the URL of one page: the base URL, which may already carry a
// query (Gemini's key does), with the page size and, once known, the cursor
// merged into it rather than appended by hand.
func (p pagination) pageURL(base, cursor string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set(p.sizeParam, modelListPageSize)
	if cursor != "" {
		q.Set(p.cursorParam, cursor)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// modelPage is one listing response: the ids it carries and the cursor that
// continues the listing. The cursor is empty when the family answers in a
// single page or the listing is exhausted.
type modelPage struct {
	ids    []string
	cursor string
}

// modelListSpecFor picks the listing recipe for a provider family.
//
// It switches on the concrete provider type rather than on Provider.Name(),
// because Name() is not a family keyword for config-driven providers: the
// enterprise constructors set it from Spec.Name (enterprise.go), and Generic
// carries the operator's instance name. Dispatching on the name would give a
// Bedrock provider called "prod-use1" the generic recipe (a GET on a URL that
// can never list, so the caller backs off forever instead of keeping its
// fallback) and would send a custom provider that happens to be called
// "gemini" or "anthropic" to Google or Anthropic with its key.
//
// baseURL comes from Provider.BaseURL(), so no host is hardcoded except where
// the provider's own BaseURL() omits the API prefix (anthropic, gemini) — the
// same split the providers' custom HealthCheck implementations already encode.
func modelListSpecFor(p Provider, apiKey string) (modelListSpec, error) {
	base := strings.TrimRight(p.BaseURL(), "/")
	// labelled stamps the provider's name on a recipe. Setting the label here,
	// rather than in each family branch, is what keeps a URL out of every
	// error: Gemini's URL carries the API key, and an operator-configured base
	// URL may carry a credential of its own.
	labelled := func(s modelListSpec) (modelListSpec, error) {
		s.label = p.Name()
		return s, nil
	}
	switch o := unwrapOverridden(p).(type) {
	case *Anthropic:
		// Anthropic.BaseURL() stops at the host; the models endpoint lives under /v1.
		return labelled(modelListSpec{
			url: anthropicModelsURL,
			header: map[string]string{
				"x-api-key":         apiKey,
				"anthropic-version": anthropicAPIVersion,
			},
			paging: &pagination{sizeParam: "limit", cursorParam: "after_id"},
			parse:  parseAnthropicModelList,
		})
	case *Gemini:
		// Gemini.BaseURL() points at the OpenAI-compat shim; the native listing
		// endpoint takes the key as a query parameter.
		return labelled(modelListSpec{
			url:    geminiModelsURL + "?key=" + url.QueryEscape(apiKey),
			paging: &pagination{sizeParam: "pageSize", cursorParam: "pageToken"},
			parse:  parseGeminiModelList,
		})
	case *Ollama:
		// The native /api/tags endpoint is authoritative and does not depend on
		// the OpenAI compatibility shim under /v1, so it is derived from the
		// same field Ollama.HealthCheck() uses instead of trimming BaseURL().
		return labelled(modelListSpec{
			url:   o.baseURL + "/api/tags",
			parse: parseOllamaModelList,
		})
	case *Groq:
		// Groq's BaseURL() ends at /openai, one segment short of the /v1 root its
		// /models endpoint lives under.
		return labelled(modelListSpec{
			url:    base + "/v1/models",
			apiKey: apiKey,
			bearer: true,
			parse:  parseOpenAIModelList,
		})
	case *Perplexity, *Azure, *Vertex, *Bedrock:
		// Perplexity publishes no listing endpoint; the enterprise providers are
		// Anthropic-native and have no OpenAI-style /models.
		return modelListSpec{}, fmt.Errorf("%w: %s", ErrNoModelList, p.Name())
	default:
		// Everything else, *Generic included, lists through its own BaseURL().
		return labelled(modelListSpec{
			url:    base + "/models",
			apiKey: apiKey,
			bearer: true,
			parse:  parseOpenAIModelList,
		})
	}
}

// unwrapOverridden sees through the config-override wrapper. New applies it
// conditionally (generic.go) and only ever wraps a built-in built by
// FromConfig, so one level is the whole nesting it can produce.
func unwrapOverridden(p Provider) Provider {
	if o, ok := p.(*overridden); ok {
		return o.Provider
	}
	return p
}

// fetch performs the listing and returns its ids. A single-page family makes
// exactly one request; a paginated family walks its cursor under the bounds
// documented on fetchPages.
func (s modelListSpec) fetch(ctx context.Context) ([]string, error) {
	client := &http.Client{Timeout: modelListTimeout}
	if s.paging == nil {
		body, err := s.get(ctx, client, s.url)
		if err != nil {
			return nil, err
		}
		page, err := s.parse(body)
		if err != nil {
			return nil, fmt.Errorf("list models: %s: %w", s.label, err)
		}
		return cleanModelIDs(page.ids), nil
	}
	return s.fetchPages(ctx, client)
}

// fetchPages walks a paginated listing. The walk is bounded three ways so a
// misbehaving endpoint cannot loop forever: at most maxModelListPages requests,
// at most maxListedModels ids, and a stop as soon as a page contributes no id
// it has not already returned or its cursor does not advance.
func (s modelListSpec) fetchPages(ctx context.Context, client *http.Client) ([]string, error) {
	var (
		out  []string
		seen = make(map[string]struct{})
		cur  string
	)
	for page := 0; page < maxModelListPages; page++ {
		pageURL, err := s.paging.pageURL(s.url, cur)
		if err != nil {
			return nil, fmt.Errorf("list models: %s: %w", s.label, withoutURL(err))
		}
		body, err := s.get(ctx, client, pageURL)
		if err != nil {
			return nil, err
		}
		parsed, err := s.parse(body)
		if err != nil {
			return nil, fmt.Errorf("list models: %s: %w", s.label, err)
		}
		var added int
		out, added = appendModels(out, seen, parsed.ids)
		if len(out) == maxListedModels {
			return out, nil
		}
		if added == 0 || parsed.cursor == "" || parsed.cursor == cur {
			break
		}
		cur = parsed.cursor
	}
	return out, nil
}

// get performs one listing request and returns its body, read under
// maxModelListBytes per response.
func (s modelListSpec) get(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, withoutURL(err))
	}
	if s.bearer && s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	for k, v := range s.header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	var resp *http.Response
	if s.source != nil {
		retried := false
		resp, err = CallWithBearer(ctx, s.source, &retried, func(token string) (*http.Response, error) {
			authorized := req.Clone(ctx)
			authorized.Header.Set("Authorization", "Bearer "+token)
			return DoRenewableRequest(client, s.origin, authorized)
		}, nil)
	} else {
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, withoutURL(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("list models: %s returned %d", s.label, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, err)
	}
	return body, nil
}

// withoutURL strips the *url.Error wrapper that http.NewRequest and
// http.Client.Do return, keeping only its cause. That wrapper's message embeds
// the full request URL, and the Gemini URL carries the API key, so wrapping it
// would echo the key from a transport failure (dial error, timeout) into the
// caller's logs — the same leak the non-2xx path already avoids by reporting
// only the status. The cause still carries the diagnostic.
//
// A *url.Error with no cause has nothing safe to pass on — its own message is
// the URL — so it becomes a fixed opaque error rather than the original.
func withoutURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if uerr.Err != nil {
			return uerr.Err
		}
		return errors.New("request failed")
	}
	return err
}

// parseOpenAIModelList reads the OpenAI shape: {"data":[{"id":"..."}]}.
func parseOpenAIModelList(body []byte) (modelPage, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	return modelPage{ids: ids}, nil
}

// parseAnthropicModelList reads the Anthropic shape, which extends the OpenAI
// one with its pagination fields:
// {"data":[{"id":"..."}],"has_more":true,"last_id":"..."}.
func parseAnthropicModelList(body []byte) (modelPage, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	page := modelPage{ids: make([]string, 0, len(payload.Data))}
	for _, m := range payload.Data {
		page.ids = append(page.ids, m.ID)
	}
	if payload.HasMore {
		page.cursor = payload.LastID
	}
	return page, nil
}

// parseGeminiModelList reads the Gemini shape, which paginates by token:
// {"models":[{"name":"models/<id>"}],"nextPageToken":"..."}.
func parseGeminiModelList(body []byte) (modelPage, error) {
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	page := modelPage{
		ids:    make([]string, 0, len(payload.Models)),
		cursor: payload.NextPageToken,
	}
	for _, m := range payload.Models {
		page.ids = append(page.ids, strings.TrimPrefix(m.Name, "models/"))
	}
	return page, nil
}

// parseOllamaModelList reads the Ollama shape: {"models":[{"name":"..."}]}.
func parseOllamaModelList(body []byte) (modelPage, error) {
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		ids = append(ids, m.Name)
	}
	return modelPage{ids: ids}, nil
}

// cleanModelIDs trims each id, drops blanks, and caps the result, preserving the
// provider's order.
func cleanModelIDs(ids []string) []string {
	out, _ := appendModels(nil, nil, ids)
	return out
}

// appendModels trims each id, drops blanks, appends the rest, and caps the
// result at maxListedModels. A non-nil seen set additionally drops ids already
// collected; it reports how many ids were appended, which the pagination loop
// uses to detect a page that repeats what it already returned.
func appendModels(out []string, seen map[string]struct{}, ids []string) ([]string, int) {
	added := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if seen != nil {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
		}
		out = append(out, id)
		added++
		if len(out) == maxListedModels {
			break
		}
	}
	return out, added
}
