package providers

import (
	"fmt"
	"net/http"
	"strings"
)

// Spec is a fully-described provider built from configuration.
type Spec struct {
	Name        string
	Type        string // "openai-compatible"/"custom"/"azure"/"vertex"/"bedrock" → config-driven; else built-in
	APIKey      string
	BaseURL     string
	Models      []string
	Tier        string
	ModelMap    map[string]string // Claude model → provider model ("default" = catch-all)
	VisionModel string            // optional operator-supplied model ID for image-bearing requests
	InputPer1M  float64
	OutputPer1M float64
	// Optional off-peak pricing window (UTC hours) + rates.
	OffPeakInputPer1M  float64
	OffPeakOutputPer1M float64
	OffPeakStartUTC    int
	OffPeakEndUTC      int
	Region      string // AWS Bedrock / Google Vertex
	Project     string // Google Vertex
	APIVersion  string // Azure OpenAI
}

// hasOffPeak reports whether the spec defines an off-peak window.
func (s Spec) hasOffPeak() bool { return s.OffPeakStartUTC != s.OffPeakEndUTC }

// Authorizer lets a provider set its own auth header(s) on an outgoing request
// (e.g. Azure's api-key header, a Vertex bearer token, or AWS SigV4 for Bedrock).
type Authorizer interface {
	Authorize(req *http.Request, body []byte, apiKey string)
}

// AnthropicNative is implemented by providers that speak the Anthropic Messages
// API at a non-standard endpoint (Bedrock, Vertex): they customize the request
// URL and may rewrite the request body.
type AnthropicNative interface {
	MessagesURL(claudeModel string) string
	PrepareBody(body []byte, claudeModel string) []byte
}

// VisionCapable is implemented by a provider that has an operator-configured
// vision-model override. NEXUS does not maintain its own catalog of which
// provider models support vision — that would require tracking every
// provider's model catalog as it changes, which is exactly what this project
// avoids. Absent this override, an image-bearing request is simply forwarded
// using MapModel's normal result, and the provider's own response (success or
// error) is relayed as-is.
type VisionCapable interface {
	VisionModel(claudeModel string) string
}

// IsGenericType reports whether a provider type denotes a generic,
// config-driven OpenAI-compatible endpoint (rather than a built-in).
func IsGenericType(t string) bool {
	switch strings.ToLower(t) {
	case "openai-compatible", "openai_compatible", "custom", "generic":
		return true
	}
	return false
}

// New builds a Provider from a Spec. A generic OpenAI-compatible endpoint is
// created when Type is custom; otherwise a built-in is created (optionally
// wrapped with config overrides for model mapping, pricing, and tier).
func New(spec Spec) (Provider, error) {
	switch strings.ToLower(spec.Type) {
	case "azure":
		return newAzure(spec)
	case "vertex":
		return newVertex(spec)
	case "bedrock":
		return newBedrock(spec)
	}
	if IsGenericType(spec.Type) {
		if spec.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required for a custom (openai-compatible) provider", spec.Name)
		}
		return newGeneric(spec), nil
	}

	name := spec.Type
	if name == "" {
		name = spec.Name
	}
	base, err := FromConfig(name, spec.APIKey, spec.BaseURL, spec.Models)
	if err != nil {
		return nil, err
	}
	if len(spec.ModelMap) > 0 || spec.InputPer1M > 0 || spec.OutputPer1M > 0 || spec.Tier != "" || spec.hasOffPeak() || spec.VisionModel != "" {
		return &overridden{Provider: base, spec: spec}, nil
	}
	return base, nil
}

// ─── overridden: a built-in provider with config overrides applied ──────────

type overridden struct {
	Provider
	spec Spec
}

func (o *overridden) MapModel(claudeModel string) string {
	if m := mapFrom(o.spec.ModelMap, claudeModel); m != "" {
		return m
	}
	return o.Provider.MapModel(claudeModel)
}

func (o *overridden) Pricing() PricingInfo {
	p := o.Provider.Pricing()
	if o.spec.InputPer1M > 0 {
		p.InputPer1M = o.spec.InputPer1M
	}
	if o.spec.OutputPer1M > 0 {
		p.OutputPer1M = o.spec.OutputPer1M
	}
	if o.spec.hasOffPeak() {
		p.OffPeakInputPer1M = o.spec.OffPeakInputPer1M
		p.OffPeakOutputPer1M = o.spec.OffPeakOutputPer1M
		p.OffPeakStartUTC = o.spec.OffPeakStartUTC
		p.OffPeakEndUTC = o.spec.OffPeakEndUTC
	}
	return p
}

func (o *overridden) Tier() string {
	if o.spec.Tier != "" {
		return o.spec.Tier
	}
	return o.Provider.Tier()
}

// VisionModel returns the operator-configured vision-model override, if any.
func (o *overridden) VisionModel(claudeModel string) string {
	return o.spec.VisionModel
}

// ─── Generic: a fully config-driven OpenAI-compatible provider ──────────────

type Generic struct {
	name        string
	baseURL     string
	tier        string
	apiKey      string
	models      []string
	modelMap    map[string]string
	visionModel string
	pricing     PricingInfo
}

func newGeneric(spec Spec) *Generic {
	tier := spec.Tier
	if tier == "" {
		tier = TierStandard
	}
	return &Generic{
		name:        spec.Name,
		baseURL:     strings.TrimRight(spec.BaseURL, "/"),
		tier:        tier,
		apiKey:      spec.APIKey,
		models:      spec.Models,
		modelMap:    spec.ModelMap,
		visionModel: spec.VisionModel,
		pricing: PricingInfo{
			InputPer1M: spec.InputPer1M, OutputPer1M: spec.OutputPer1M,
			OffPeakInputPer1M: spec.OffPeakInputPer1M, OffPeakOutputPer1M: spec.OffPeakOutputPer1M,
			OffPeakStartUTC: spec.OffPeakStartUTC, OffPeakEndUTC: spec.OffPeakEndUTC,
		},
	}
}

func (g *Generic) Name() string               { return g.name }
func (g *Generic) BaseURL() string            { return g.baseURL }
func (g *Generic) Tier() string               { return g.tier }
func (g *Generic) ChatCompletionsURL() string { return g.baseURL + "/chat/completions" }
func (g *Generic) Pricing() PricingInfo       { return g.pricing }
func (g *Generic) HealthCheck() error         { return bearerHealthCheck(g.baseURL+"/models", g.apiKey) }

func (g *Generic) MapModel(claudeModel string) string {
	if m := mapFrom(g.modelMap, claudeModel); m != "" {
		return m
	}
	if len(g.models) > 0 {
		return g.models[0]
	}
	return claudeModel
}

// VisionModel returns the operator-configured vision-model override, if any.
func (g *Generic) VisionModel(claudeModel string) string { return g.visionModel }

// orDefault returns v if non-empty, else def.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// mapFrom looks up a Claude model in a model map, falling back to "default".
func mapFrom(m map[string]string, claudeModel string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[claudeModel]; ok && v != "" {
		return v
	}
	if v, ok := m["default"]; ok && v != "" {
		return v
	}
	return ""
}
