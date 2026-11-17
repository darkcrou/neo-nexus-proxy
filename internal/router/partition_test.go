package router

import (
	"reflect"
	"sync"
	"testing"
)

const requestedModelID = "glm-5.2"

func providerNames(chain []*Provider) []string {
	out := make([]string, len(chain))
	for i, p := range chain {
		out[i] = p.Name
	}
	return out
}

// serves builds a provider Models map in the shape discovery produces: each
// provider model id mapped to itself.
func serves(ids ...string) map[string]string {
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[id] = id
	}
	return m
}

func TestRouteChain_PrefersProvidersThatServeTheRequestedModel(t *testing.T) {
	r := New(StrategyAuto)
	// "primary" wins the tier ordering for a standard task, so only the model
	// partition can move the serving provider ahead of it.
	r.AddProvider(&Provider{Name: "primary", Tier: "standard", Healthy: true})
	r.AddProvider(&Provider{Name: "serving", Tier: "premium", Healthy: true, Models: serves(requestedModelID)})

	got := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))
	want := []string{"serving", "primary"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestRouteChain_PrefersServingProvidersUnderEveryStrategy(t *testing.T) {
	// "preferred" wins the natural ordering of every strategy below: local tier,
	// lowest price, highest priority. Only the model partition can reorder them.
	setup := func(r *Router) {
		r.AddProvider(&Provider{
			Name: "preferred", Tier: "local", Healthy: true, Priority: 0,
			Pricing: Pricing{InputPer1M: 0.1, OutputPer1M: 0.1},
		})
		r.AddProvider(&Provider{
			Name: "serving", Tier: "premium", Healthy: true, Priority: 9,
			Pricing: Pricing{InputPer1M: 30, OutputPer1M: 60},
			Models:  serves(requestedModelID),
		})
	}

	for _, strategy := range []RoutingStrategy{
		StrategyAuto, StrategyDirect, StrategyCheapest, StrategyFastest,
	} {
		t.Run(string(strategy), func(t *testing.T) {
			r := New(strategy)
			setup(r)

			got := providerNames(r.RouteChain(requestedModelID, ComplexitySimple))
			want := []string{"serving", "preferred"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("chain = %v, want %v", got, want)
			}
		})
	}
}

func TestRouteChain_KeepsStrategyOrderWhenNoProviderServesTheModel(t *testing.T) {
	r := New(StrategyAuto)
	// Registered out of tier order so the expected chain cannot be confused
	// with registration order.
	r.AddProvider(&Provider{Name: "premium", Tier: "premium", Healthy: true})
	r.AddProvider(&Provider{Name: "free", Tier: "free", Healthy: true})
	r.AddProvider(&Provider{Name: "standard", Tier: "standard", Healthy: true})

	got := providerNames(r.RouteChain("model-nobody-serves", ComplexityStandard))
	want := []string{"standard", "free", "premium"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestRouteChain_KeepsStrategyOrderWhenEveryProviderServesTheModel(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "premium", Tier: "premium", Healthy: true, Models: serves(requestedModelID)})
	r.AddProvider(&Provider{Name: "free", Tier: "free", Healthy: true, Models: serves(requestedModelID)})
	r.AddProvider(&Provider{Name: "standard", Tier: "standard", Healthy: true, Models: serves(requestedModelID)})

	got := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))
	want := []string{"standard", "free", "premium"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestRouteChain_PreservesRelativeOrderWithinEachGroup(t *testing.T) {
	r := New(StrategyAuto)
	// One tier for every provider, so the base order is Priority order:
	// p0, p1, p2, p3. Serving and non-serving providers are interleaved.
	layout := []struct {
		name    string
		serving bool
	}{
		{"p0", false},
		{"p1", true},
		{"p2", false},
		{"p3", true},
	}
	for i, l := range layout {
		p := &Provider{Name: l.name, Tier: "standard", Healthy: true, Priority: i}
		if l.serving {
			p.Models = serves(requestedModelID)
		}
		r.AddProvider(p)
	}

	got := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))
	want := []string{"p1", "p3", "p0", "p2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestManualStrategy_PutsServingProvidersFirst(t *testing.T) {
	r := New(StrategyManual)
	r.AddProvider(&Provider{Name: "first", Healthy: true})
	r.AddProvider(&Provider{Name: "serving", Healthy: true, Models: serves(requestedModelID)})
	r.AddProvider(&Provider{Name: "third", Healthy: true})

	got := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))
	want := []string{"serving", "first", "third"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestManualStrategy_KeepsRegistrationOrderWhenNothingServesTheModel(t *testing.T) {
	r := New(StrategyManual)
	r.AddProvider(&Provider{Name: "premium", Tier: "premium", Healthy: true})
	r.AddProvider(&Provider{Name: "free", Tier: "free", Healthy: true})

	got := providerNames(r.RouteChain("model-nobody-serves", ComplexityStandard))
	want := []string{"premium", "free"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

func TestSetProviderModels_IgnoresUnregisteredProvider(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "known", Tier: "standard", Healthy: true})
	r.AddProvider(&Provider{Name: "other", Tier: "premium", Healthy: true})

	before := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))
	r.SetProviderModels("never-registered", []string{requestedModelID}) // must not panic
	after := providerNames(r.RouteChain(requestedModelID, ComplexityStandard))

	if !reflect.DeepEqual(after, before) {
		t.Errorf("chain changed from %v to %v after setting models for an unknown provider", before, after)
	}
	for _, p := range r.Providers() {
		if _, ok := p.Models[requestedModelID]; ok {
			t.Errorf("provider %q unexpectedly gained model %q", p.Name, requestedModelID)
		}
	}
}

func TestSetProviderModels_ReplacesRatherThanMerges(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "p", Tier: "standard", Healthy: true})

	r.SetProviderModels("p", []string{"old-model"})
	r.SetProviderModels("p", []string{"new-model"})

	models := r.Providers()[0].Models
	if _, ok := models["old-model"]; ok {
		t.Errorf("old-model should be gone after a replacement, got %v", models)
	}
	if got := models["new-model"]; got != "new-model" {
		t.Errorf("new-model should map to itself, got %q", got)
	}
	if len(models) != 1 {
		t.Errorf("models = %v, want exactly one entry", models)
	}
}

// The router lock must cover the Models maps: a background discovery loop
// rewrites them while request goroutines read them inside RouteChain.
func TestRouteChain_ConcurrentSetProviderModelsIsRaceFree(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "local", Tier: "local", Healthy: true})
	r.AddProvider(&Provider{Name: "standard", Tier: "standard", Healthy: true})
	r.AddProvider(&Provider{Name: "premium", Tier: "premium", Healthy: true})

	lists := [][]string{{requestedModelID}, {"another-model"}, nil}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			chain := r.RouteChain(requestedModelID, ComplexityStandard)
			if len(chain) == 0 {
				t.Error("RouteChain returned an empty chain")
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			r.SetProviderModels("local", lists[i%len(lists)])
			r.SetProviderModels("standard", lists[(i+1)%len(lists)])
			r.SetProviderModels("premium", lists[(i+2)%len(lists)])
		}
	}()
	wg.Wait()
}

func TestRouteChain_HonoursModelsRegisteredThroughSetProviderModels(t *testing.T) {
	r := New(StrategyAuto)
	r.AddProvider(&Provider{Name: "cheap", Tier: "local", Healthy: true})
	r.AddProvider(&Provider{Name: "serving", Tier: "premium", Healthy: true})
	r.SetProviderModels("serving", []string{requestedModelID})

	got := providerNames(r.RouteChain(requestedModelID, ComplexitySimple))
	want := []string{"serving", "cheap"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}
