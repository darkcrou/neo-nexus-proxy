package router

import "testing"

func TestAutoRouteByComplexity(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "groq", Tier: "free", Healthy: true})
	r.AddProvider(&Provider{Name: "deepseek", Tier: "standard", Healthy: true})
	r.AddProvider(&Provider{Name: "anthropic", Tier: "premium", Healthy: true})

	cases := []struct {
		name       string
		model      string
		complexity Complexity
		want       string
	}{
		{"simple → free", "claude-haiku-4-5", ComplexitySimple, "groq"},
		{"standard → standard", "claude-sonnet-4-6", ComplexityStandard, "deepseek"},
		{"complex → premium", "claude-opus-4-5", ComplexityComplex, "anthropic"},
		{"critical → premium", "claude-opus-4-5", ComplexityCritical, "anthropic"},
	}
	for _, tc := range cases {
		p := r.Route(tc.model, tc.complexity)
		if p == nil {
			t.Errorf("%s: got nil", tc.name)
			continue
		}
		if p.Name != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, p.Name, tc.want)
		}
	}
}

func TestRouteChainFallbackOrder(t *testing.T) {
	// Only premium available; a simple request should still find it via fallback.
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "anthropic", Tier: "premium", Healthy: true})

	chain := r.RouteChain("claude-haiku-4-5", ComplexitySimple)
	if len(chain) != 1 || chain[0].Name != "anthropic" {
		t.Errorf("fallback chain = %+v, want [anthropic]", chain)
	}
}

func TestDirectStrategyMatchesAutoChainOrder(t *testing.T) {
	// direct must reuse autoChain's ordering exactly (tier fallback, priority
	// within tier) — the only difference from auto is model-id passthrough,
	// which lives in internal/proxy, not here.
	setup := func(r *Router) {
		r.AddProvider(&Provider{Name: "groq", Tier: "free", Healthy: true})
		r.AddProvider(&Provider{Name: "deepseek", Tier: "standard", Healthy: true})
		r.AddProvider(&Provider{Name: "anthropic", Tier: "premium", Healthy: true})
	}

	direct := New(StrategyDirect)
	setup(direct)
	auto := New(StrategyAuto)
	setup(auto)

	complexities := []Complexity{ComplexitySimple, ComplexityStandard, ComplexityComplex, ComplexityCritical}
	for _, c := range complexities {
		wantChain := auto.RouteChain("glm-5.2", c)
		gotChain := direct.RouteChain("glm-5.2", c)
		if len(gotChain) != len(wantChain) {
			t.Fatalf("%s: chain length = %d, want %d", c, len(gotChain), len(wantChain))
		}
		for i := range wantChain {
			if gotChain[i].Name != wantChain[i].Name {
				t.Errorf("%s: chain[%d] = %q, want %q (direct chain: %+v)", c, i, gotChain[i].Name, wantChain[i].Name, gotChain)
			}
		}
	}
}

func TestUnhealthyExcluded(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "groq", Tier: "free", Healthy: false})
	r.AddProvider(&Provider{Name: "anthropic", Tier: "premium", Healthy: true})

	if p := r.Route("claude-haiku-4-5", ComplexitySimple); p == nil || p.Name != "anthropic" {
		t.Errorf("unhealthy groq should be skipped, got %v", p)
	}
}

func TestCheapestStrategy(t *testing.T) {
	r := New(StrategyCheapest)
	r.AddProvider(&Provider{Name: "anthropic", Tier: "premium", Healthy: true, Pricing: Pricing{InputPer1M: 3, OutputPer1M: 15}})
	r.AddProvider(&Provider{Name: "groq", Tier: "free", Healthy: true, Pricing: Pricing{InputPer1M: 0, OutputPer1M: 0}})

	if p := r.Route("claude-sonnet-4-6", ComplexityComplex); p == nil || p.Name != "groq" {
		t.Errorf("cheapest should pick groq, got %v", p)
	}
}

func TestClassifyCriticalKeyword(t *testing.T) {
	msgs := []map[string]interface{}{
		{"role": "user", "content": "we have a production down emergency, help!"},
	}
	if c := ClassifyRequest("claude-haiku-4-5", msgs, false); c != ComplexityCritical {
		t.Errorf("expected critical, got %s", c)
	}
}

func TestClassifySimpleShortPrompt(t *testing.T) {
	msgs := []map[string]interface{}{
		{"role": "user", "content": "hi"},
	}
	if c := ClassifyRequest("claude-haiku-4-5", msgs, false); c != ComplexitySimple {
		t.Errorf("expected simple, got %s", c)
	}
}
