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
	models, err := listModelDetails(ctx, p, apiKey)
	return ListedModelIDs(models), err
}

// ListedModel is a model from an upstream listing. Nil InputModalities means
// unknown. An empty, non-nil slice is an explicit empty list.
type ListedModel struct {
	ID              string
	InputModalities []string
}

// ListedModelIDs returns the ids in listing order for consumers that only need
// model identity.
func ListedModelIDs(models []ListedModel) []string {
	if len(models) == 0 {
		return nil
	}
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

// ListModelDetails fetches model ids and any reported input modalities.
func ListModelDetails(p Provider, apiKey string) ([]ListedModel, error) {
	return listModelDetails(context.Background(), p, apiKey)
}

func listModelDetails(ctx context.Context, p Provider, apiKey string) ([]ListedModel, error) {
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

// modelPage is one listing response: the models it carries and the cursor that
// continues the listing. The cursor is empty when the family answers in a
// single page or the listing is exhausted.
type modelPage struct {
	models []ListedModel
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

// fetch performs the listing and returns its models. A single-page family makes
// exactly one request; a paginated family walks its cursor under the bounds
// documented on fetchPages.
func (s modelListSpec) fetch(ctx context.Context) ([]ListedModel, error) {
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
		models, _ := appendModels(nil, nil, page.models)
		return models, nil
	}
	return s.fetchPages(ctx, client)
}

// fetchPages walks a paginated listing. The walk is bounded three ways so a
// misbehaving endpoint cannot loop forever: at most maxModelListPages requests,
// at most maxListedModels ids, and a stop as soon as a page contributes no id
// it has not already returned or its cursor does not advance.
func (s modelListSpec) fetchPages(ctx context.Context, client *http.Client) ([]ListedModel, error) {
	var (
		out  []ListedModel
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
		out, added = appendModels(out, seen, parsed.models)
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

// Optional metadata is decoded independently so an invalid capability field
// cannot make the provider's model IDs unavailable for routing.
type modelInputFields struct {
	InputModalities json.RawMessage `json:"input_modalities"`
	Architecture    json.RawMessage `json:"architecture"`
	SupportedInput  json.RawMessage `json:"supported_input"`
	Input           json.RawMessage `json:"input"`
}

func (f modelInputFields) modalities() []string {
	if values := decodeModalities(f.InputModalities); values != nil {
		return values
	}
	var architecture struct {
		InputModalities json.RawMessage `json:"input_modalities"`
	}
	if len(f.Architecture) > 0 && json.Unmarshal(f.Architecture, &architecture) == nil {
		if values := decodeModalities(architecture.InputModalities); values != nil {
			return values
		}
	}
	if values := decodeModalities(f.SupportedInput); values != nil {
		return values
	}
	return decodeModalities(f.Input)
}

func decodeModalities(raw json.RawMessage) []string {
	var values []string
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil || values == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return nil
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

type modelListItem struct {
	ID string `json:"id"`
	modelInputFields
}

type namedModelListItem struct {
	Name string `json:"name"`
	modelInputFields
}

// parseOpenAIModelList reads the OpenAI shape: {"data":[{"id":"..."}]}.
func parseOpenAIModelList(body []byte) (modelPage, error) {
	var payload struct {
		Data []modelListItem `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	models := make([]ListedModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		models = append(models, ListedModel{ID: m.ID, InputModalities: m.modalities()})
	}
	return modelPage{models: models}, nil
}

// parseAnthropicModelList reads the Anthropic shape, which extends the OpenAI
// one with its pagination fields:
// {"data":[{"id":"..."}],"has_more":true,"last_id":"..."}.
func parseAnthropicModelList(body []byte) (modelPage, error) {
	var payload struct {
		Data []struct {
			modelListItem
			Capabilities json.RawMessage `json:"capabilities"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	page := modelPage{models: make([]ListedModel, 0, len(payload.Data))}
	for _, m := range payload.Data {
		inputs := m.modalities()
		if inputs == nil {
			inputs = anthropicInputModalities(m.Capabilities)
		}
		page.models = append(page.models, ListedModel{ID: m.ID, InputModalities: inputs})
	}
	if payload.HasMore {
		page.cursor = payload.LastID
	}
	return page, nil
}

func anthropicInputModalities(raw json.RawMessage) []string {
	type capability struct {
		Supported *bool `json:"supported"`
	}
	var flags struct {
		Image capability `json:"image_input"`
		PDF   capability `json:"pdf_input"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &flags) != nil {
		return nil
	}
	if flags.Image.Supported == nil && flags.PDF.Supported == nil {
		return nil
	}
	// Text is the base input for the native Messages API.
	inputs := []string{"text"}
	if flags.Image.Supported != nil && *flags.Image.Supported {
		inputs = append(inputs, "image")
	}
	if flags.PDF.Supported != nil && *flags.PDF.Supported {
		inputs = append(inputs, "pdf")
	}
	return inputs
}

// parseGeminiModelList reads the Gemini shape, which paginates by token:
// {"models":[{"name":"models/<id>"}],"nextPageToken":"..."}.
func parseGeminiModelList(body []byte) (modelPage, error) {
	var payload struct {
		Models        []namedModelListItem `json:"models"`
		NextPageToken string               `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	page := modelPage{
		models: make([]ListedModel, 0, len(payload.Models)),
		cursor: payload.NextPageToken,
	}
	for _, m := range payload.Models {
		page.models = append(page.models, ListedModel{
			ID: strings.TrimPrefix(m.Name, "models/"), InputModalities: m.modalities(),
		})
	}
	return page, nil
}

// parseOllamaModelList reads the Ollama shape: {"models":[{"name":"..."}]}.
func parseOllamaModelList(body []byte) (modelPage, error) {
	var payload struct {
		Models []namedModelListItem `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return modelPage{}, fmt.Errorf("decode model list: %w", err)
	}
	models := make([]ListedModel, 0, len(payload.Models))
	for _, m := range payload.Models {
		models = append(models, ListedModel{ID: m.Name, InputModalities: m.modalities()})
	}
	return modelPage{models: models}, nil
}

// appendModels trims each id, drops blanks, appends the rest, and caps the
// result at maxListedModels. A non-nil seen set additionally drops ids already
// collected; it reports how many ids were appended, which the pagination loop
// uses to detect a page that repeats what it already returned.
func appendModels(out []ListedModel, seen map[string]struct{}, models []ListedModel) ([]ListedModel, int) {
	added := 0
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if seen != nil {
			if _, dup := seen[model.ID]; dup {
				continue
			}
			seen[model.ID] = struct{}{}
		}
		out = append(out, model)
		added++
		if len(out) == maxListedModels {
			break
		}
	}
	return out, added
}
