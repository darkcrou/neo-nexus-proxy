// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package providers

import (
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
	// maxModelListBytes caps the response body read from a listing endpoint.
	maxModelListBytes = 8 << 20

	anthropicModelsURL  = "https://api.anthropic.com/v1/models"
	geminiModelsURL     = "https://generativelanguage.googleapis.com/v1beta/models"
	anthropicAPIVersion = "2023-06-01"
)

// ListModels returns the model ids the provider currently offers, using the
// endpoint and authentication its family needs. Ids are returned in the
// provider's own order, trimmed, without blanks, and capped at maxListedModels.
//
// A provider that cannot list its models returns ErrNoModelList; any other
// error means the listing was attempted and failed. ListModels does not retry
// or cache — the caller owns both.
func ListModels(p Provider, apiKey string) ([]string, error) {
	if p == nil {
		return nil, errors.New("list models: nil provider")
	}
	spec, err := modelListSpecFor(p.Name(), p.BaseURL(), apiKey)
	if err != nil {
		return nil, err
	}
	return spec.fetch()
}

// modelListSpec is one provider family's listing recipe: where to ask, how to
// authenticate, how to read the answer, and what to call the endpoint in errors.
type modelListSpec struct {
	url    string
	label  string            // endpoint name used in errors; never a URL carrying a secret
	apiKey string            // sent as a bearer token when bearer is set
	bearer bool              // the family authenticates with Authorization: Bearer
	header map[string]string // family-specific auth headers
	parse  func([]byte) ([]string, error)
}

// modelListSpecFor picks the listing recipe for a provider family. baseURL comes
// from Provider.BaseURL(), so no host is hardcoded except where the provider's
// own BaseURL() omits the API prefix (anthropic, gemini) — the same split the
// providers' custom HealthCheck implementations already encode.
func modelListSpecFor(name, baseURL, apiKey string) (modelListSpec, error) {
	base := strings.TrimRight(baseURL, "/")
	switch strings.ToLower(name) {
	case "anthropic":
		// Anthropic.BaseURL() stops at the host; the models endpoint lives under /v1.
		return modelListSpec{
			url:   anthropicModelsURL,
			label: "anthropic",
			header: map[string]string{
				"x-api-key":         apiKey,
				"anthropic-version": anthropicAPIVersion,
			},
			parse: parseOpenAIModelList,
		}, nil
	case "gemini":
		// Gemini.BaseURL() points at the OpenAI-compat shim; the native listing
		// endpoint takes the key as a query parameter.
		return modelListSpec{
			url:   geminiModelsURL + "?key=" + url.QueryEscape(apiKey),
			label: "gemini", // the URL carries the key, so it is never echoed
			parse: parseGeminiModelList,
		}, nil
	case "ollama":
		// The native /api/tags endpoint is authoritative and does not depend on
		// the OpenAI compatibility shim under /v1.
		return modelListSpec{
			url:   strings.TrimSuffix(base, "/v1") + "/api/tags",
			label: "ollama",
			parse: parseOllamaModelList,
		}, nil
	case "groq":
		// Groq's BaseURL() ends at /openai, one segment short of the /v1 root its
		// /models endpoint lives under.
		u := base + "/v1/models"
		return modelListSpec{url: u, label: u, apiKey: apiKey, bearer: true, parse: parseOpenAIModelList}, nil
	case "perplexity", "azure", "vertex", "bedrock":
		// Perplexity publishes no listing endpoint; the enterprise providers are
		// Anthropic-native and have no OpenAI-style /models.
		return modelListSpec{}, fmt.Errorf("%w: %s", ErrNoModelList, name)
	default:
		u := base + "/models"
		return modelListSpec{url: u, label: u, apiKey: apiKey, bearer: true, parse: parseOpenAIModelList}, nil
	}
}

// fetch performs the listing request and parses the response.
func (s modelListSpec) fetch() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, err)
	}
	if s.bearer && s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	for k, v := range s.header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	client := &http.Client{Timeout: modelListTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("list models: %s returned %d", s.label, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, err)
	}
	ids, err := s.parse(body)
	if err != nil {
		return nil, fmt.Errorf("list models: %s: %w", s.label, err)
	}
	return cleanModelIDs(ids), nil
}

// parseOpenAIModelList reads the OpenAI shape: {"data":[{"id":"..."}]}.
func parseOpenAIModelList(body []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// parseGeminiModelList reads the Gemini shape: {"models":[{"name":"models/<id>"}]}.
func parseGeminiModelList(body []byte) ([]string, error) {
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
	}
	return ids, nil
}

// parseOllamaModelList reads the Ollama shape: {"models":[{"name":"..."}]}.
func parseOllamaModelList(body []byte) ([]string, error) {
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		ids = append(ids, m.Name)
	}
	return ids, nil
}

// cleanModelIDs trims each id, drops blanks, and caps the result, preserving the
// provider's order.
func cleanModelIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, id)
		if len(out) == maxListedModels {
			break
		}
	}
	return out
}
