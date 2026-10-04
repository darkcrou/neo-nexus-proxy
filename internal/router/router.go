// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package router

import (
	"sort"
	"strings"
	"sync"
)

// Complexity levels for task classification
type Complexity int

const (
	ComplexitySimple   Complexity = iota // Quick answers, short prompts
	ComplexityStandard                   // Normal coding tasks
	ComplexityComplex                    // Architecture, debugging, planning
	ComplexityCritical                   // Security, production issues
)

func (c Complexity) String() string {
	switch c {
	case ComplexitySimple:
		return "simple"
	case ComplexityStandard:
		return "standard"
	case ComplexityComplex:
		return "complex"
	case ComplexityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// RoutingStrategy defines how the router picks providers
type RoutingStrategy string

const (
	StrategyAuto     RoutingStrategy = "auto"     // intelligent routing (default)
	StrategyCheapest RoutingStrategy = "cheapest" // always cheapest available
	StrategyFastest  RoutingStrategy = "fastest"  // always fastest provider
	StrategyManual   RoutingStrategy = "manual"   // explicit model → provider mapping
	StrategyDirect   RoutingStrategy = "direct"   // pass through the requested model id unchanged
)

// Provider holds provider configuration for routing
type Provider struct {
	Name     string
	BaseURL  string
	APIKey   string
	Models   map[string]string // Claude model → provider model
	Tier     string            // free | standard | premium | local
	Priority int               // lower = higher priority within same tier
	Pricing  Pricing
	Healthy  bool
}

// Pricing holds token costs for a provider
type Pricing struct {
	InputPer1M  float64 // USD per 1M input tokens
	OutputPer1M float64 // USD per 1M output tokens
}

// Router routes requests to the appropriate provider.
type Router struct {
	mu        sync.RWMutex
	strategy  RoutingStrategy
	providers []*Provider
	adaptive  bool
	stats     map[string]*provStat // key: provider|complexity
}

// provStat tracks how often a provider produced a usable answer for a complexity.
type provStat struct{ attempts, successes int }

func statKey(name string, c Complexity) string { return name + "|" + c.String() }

// SetAdaptive turns on learned routing: within a tier, providers that have
// historically produced usable answers for a complexity are tried first.
func (r *Router) SetAdaptive(b bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adaptive = b
	if r.stats == nil {
		r.stats = map[string]*provStat{}
	}
}

// RecordOutcome records whether a provider produced a usable answer.
func (r *Router) RecordOutcome(name string, c Complexity, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats == nil {
		r.stats = map[string]*provStat{}
	}
	k := statKey(name, c)
	s := r.stats[k]
	if s == nil {
		s = &provStat{}
		r.stats[k] = s
	}
	s.attempts++
	if ok {
		s.successes++
	}
}

// successRate returns successes/attempts, or a neutral 0.5 with no data. Caller
// holds at least an RLock.
func (r *Router) successRate(name string, c Complexity) float64 {
	if s := r.stats[statKey(name, c)]; s != nil && s.attempts > 0 {
		return float64(s.successes) / float64(s.attempts)
	}
	return 0.5
}

// adaptiveRates snapshots the success rate of each provider for a complexity.
func (r *Router) adaptiveRates(hp []*Provider, c Complexity) map[string]float64 {
	rates := map[string]float64{}
	if !r.adaptive {
		return rates
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range hp {
		rates[p.Name] = r.successRate(p.Name, c)
	}
	return rates
}

// New creates a new router
func New(strategy RoutingStrategy) *Router {
	if strategy == "" {
		strategy = StrategyAuto
	}
	return &Router{
		strategy:  strategy,
		providers: make([]*Provider, 0),
	}
}

// AddProvider adds a provider to the router
func (r *Router) AddProvider(p *Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

// Providers returns all registered providers.
func (r *Router) Providers() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// SetHealthy updates a provider's health (called by the background checker).
func (r *Router) SetHealthy(name string, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.providers {
		if p.Name == name {
			p.Healthy = healthy
			return
		}
	}
}

// Route returns the single best provider (head of the chain), or nil.
func (r *Router) Route(requestedModel string, complexity Complexity) *Provider {
	chain := r.RouteChain(requestedModel, complexity)
	if len(chain) == 0 {
		return nil
	}
	return chain[0]
}

// RouteChain returns an ordered list of candidate providers to try, best first.
// Only healthy providers are included. The caller walks the chain, failing over
// to the next candidate on transport errors or retryable HTTP statuses.
func (r *Router) RouteChain(requestedModel string, complexity Complexity) []*Provider {
	switch r.strategy {
	case StrategyCheapest:
		return r.byCost()
	case StrategyFastest:
		return r.byTierSpeed()
	case StrategyManual:
		return r.manualChain(requestedModel)
	case StrategyDirect:
		return r.autoChain(complexity)
	default: // StrategyAuto
		return r.autoChain(complexity)
	}
}

// CascadeChain orders healthy providers cheapest-first (local → free → standard
// → premium) for the cheap-first cascade: try the cheapest capable model, verify
// its output, and escalate only on failure. Critical requests stay premium-first
// so security/production work is never gambled on a weak model.
func (r *Router) CascadeChain(complexity Complexity) []*Provider {
	if complexity == ComplexityCritical {
		return r.autoChain(complexity)
	}
	pri := map[string]int{"local": 0, "free": 1, "standard": 2, "premium": 3}
	hp := r.healthy()
	rates := r.adaptiveRates(hp, complexity)
	sort.SliceStable(hp, func(i, j int) bool {
		if pri[hp[i].Tier] != pri[hp[j].Tier] {
			return pri[hp[i].Tier] < pri[hp[j].Tier]
		}
		if r.adaptive && rates[hp[i].Name] != rates[hp[j].Name] {
			return rates[hp[i].Name] > rates[hp[j].Name] // higher success first
		}
		return hp[i].Priority < hp[j].Priority
	})
	return hp
}

// ─── Strategy implementations ──────────────────────────────────────────────

func (r *Router) healthy() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Provider, 0, len(r.providers))
	for _, p := range r.providers {
		if p.Healthy {
			out = append(out, p)
		}
	}
	return out
}

// autoChain orders healthy providers by the tier-fallback preference for the
// target complexity, then by Priority within a tier.
func (r *Router) autoChain(complexity Complexity) []*Provider {
	order := fallbackTiers(complexityToTier(complexity))
	rank := make(map[string]int, len(order))
	for i, t := range order {
		rank[t] = i
	}
	hp := r.healthy()
	rates := r.adaptiveRates(hp, complexity)
	sort.SliceStable(hp, func(i, j int) bool {
		ri, ok := rank[hp[i].Tier]
		if !ok {
			ri = len(order)
		}
		rj, ok := rank[hp[j].Tier]
		if !ok {
			rj = len(order)
		}
		if ri != rj {
			return ri < rj
		}
		if r.adaptive && rates[hp[i].Name] != rates[hp[j].Name] {
			return rates[hp[i].Name] > rates[hp[j].Name]
		}
		return hp[i].Priority < hp[j].Priority
	})
	return hp
}

func (r *Router) byCost() []*Provider {
	hp := r.healthy()
	sort.SliceStable(hp, func(i, j int) bool {
		return hp[i].Pricing.InputPer1M+hp[i].Pricing.OutputPer1M <
			hp[j].Pricing.InputPer1M+hp[j].Pricing.OutputPer1M
	})
	return hp
}

func (r *Router) byTierSpeed() []*Provider {
	pri := map[string]int{"local": 0, "free": 1, "standard": 2, "premium": 3}
	hp := r.healthy()
	sort.SliceStable(hp, func(i, j int) bool {
		return pri[hp[i].Tier] < pri[hp[j].Tier]
	})
	return hp
}

func (r *Router) manualChain(requestedModel string) []*Provider {
	var match, rest []*Provider
	for _, p := range r.healthy() {
		if _, ok := p.Models[requestedModel]; ok {
			match = append(match, p)
		} else {
			rest = append(rest, p)
		}
	}
	return append(match, rest...)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// complexityToTier maps task complexity to a provider tier
func complexityToTier(c Complexity) string {
	switch c {
	case ComplexitySimple:
		return "free"
	case ComplexityStandard:
		return "standard"
	case ComplexityComplex:
		return "premium"
	case ComplexityCritical:
		return "premium"
	default:
		return "standard"
	}
}

// fallbackTiers returns the tier preference order for a given target tier
func fallbackTiers(target string) []string {
	switch target {
	case "free":
		return []string{"local", "free", "standard", "premium"}
	case "standard":
		return []string{"standard", "free", "premium"}
	case "premium":
		return []string{"premium", "standard"}
	case "local":
		return []string{"local", "free", "standard", "premium"}
	default:
		return []string{"standard", "free", "premium"}
	}
}

// IsAnthropicModel checks if a model string is a Claude model
func IsAnthropicModel(model string) bool {
	return strings.HasPrefix(model, "claude-")
}
