// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NEXUS contributors

package proxy

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
)

// The dashboard's Providers tab reads per-provider catalog state through
// Server.ProviderCatalog. These tests specify that read surface: it reports
// what each provider serves and how the list was established, it is a pure
// cache read (never a fetch), and it hands out copies the caller may keep.

func TestProviderCatalogReportsSeedBeforeDiscovery(t *testing.T) {
	f := newFakeUpstream(t, "live-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.warms = true
	cat.seed(map[string][]string{"acme": {"seed-model"}})

	got := cat.providerSnapshot()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	e := got[0]
	if e.Name != "acme" {
		t.Errorf("name = %q, want acme", e.Name)
	}
	if !reflect.DeepEqual(e.Models, []string{"seed-model"}) {
		t.Errorf("models = %v, want the seed before discovery", e.Models)
	}
	if e.Discovered {
		t.Error("discovered = true before any upstream listing")
	}
	if !e.Warming {
		t.Error("warming = false, want true when the warm loop runs")
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0 — reading the catalog must not fetch", f.requests())
	}
}

func TestProviderCatalogReportsDiscoveredList(t *testing.T) {
	f := newFakeUpstream(t, "live-a", "live-b")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.warms = true
	cat.seed(map[string][]string{"acme": {"seed-model"}})

	cat.refreshAll(context.Background())

	got := cat.providerSnapshot()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if !got[0].Discovered {
		t.Error("discovered = false after a successful listing")
	}
	if want := []string{"live-a", "live-b"}; !reflect.DeepEqual(got[0].Models, want) {
		t.Errorf("models = %v, want the discovered list %v", got[0].Models, want)
	}
}

func TestProviderCatalogPreservesRegistrationOrder(t *testing.T) {
	fa := newFakeUpstream(t, "a-model")
	fb := newFakeUpstream(t, "b-model")
	cat, _ := buildCatalog(t, 300*time.Second,
		newUpstreamProvider(t, "beta", fb), newUpstreamProvider(t, "alpha", fa))
	cat.seed(map[string][]string{"alpha": {"a-seed"}, "beta": {"b-seed"}})

	got := cat.providerSnapshot()
	if len(got) != 2 || got[0].Name != "beta" || got[1].Name != "alpha" {
		t.Fatalf("order = %v, want registration order [beta alpha]", got)
	}
}

func TestProviderCatalogNeverFetches(t *testing.T) {
	f := newFakeUpstream(t, "live-model")
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	calls := 0
	cat.listModels = func(providers.Provider, string) ([]providers.ListedModel, error) {
		calls++
		return nil, providers.ErrNoModelList
	}

	for i := 0; i < 3; i++ {
		_ = cat.providerSnapshot()
	}
	if calls != 0 {
		t.Errorf("listing attempts = %d, want 0 — a catalog read must never list upstream", calls)
	}
	if f.requests() != 0 {
		t.Errorf("upstream requests = %d, want 0", f.requests())
	}
}

func TestProviderCatalogReportsUnsupportedListing(t *testing.T) {
	f := newFakeUpstream(t)
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.listModels = func(providers.Provider, string) ([]providers.ListedModel, error) {
		return nil, providers.ErrNoModelList
	}

	cat.refreshAll(context.Background())

	got := cat.providerSnapshot()
	if len(got) != 1 || !got[0].Unsupported {
		t.Fatalf("unsupported = %v, want true after ErrNoModelList", got)
	}
	if got[0].Discovered || got[0].Err != "" {
		t.Errorf("unsupported entry carries discovered/err state: %+v", got[0])
	}
}

func TestProviderCatalogReportsRetryableFailureAndKeepsLastList(t *testing.T) {
	f := newFakeUpstream(t)
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.seed(map[string][]string{"acme": {"seed-model"}})
	cat.listModels = func(providers.Provider, string) ([]providers.ListedModel, error) {
		return nil, errors.New("listing exploded")
	}

	cat.refreshAll(context.Background())

	got := cat.providerSnapshot()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Err != "listing exploded" {
		t.Errorf("err = %q, want the retryable failure text", got[0].Err)
	}
	if got[0].Discovered {
		t.Error("discovered = true after a failed listing")
	}
	if !reflect.DeepEqual(got[0].Models, []string{"seed-model"}) {
		t.Errorf("models = %v, want the last known list kept on failure", got[0].Models)
	}
}

func TestProviderCatalogHandsOutCopies(t *testing.T) {
	f := newFakeUpstream(t)
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.seed(map[string][]string{"acme": {"seed-model"}})

	got := cat.providerSnapshot()
	got[0].Models[0] = "mutated-by-caller"

	again := cat.providerSnapshot()
	if again[0].Models[0] != "seed-model" {
		t.Errorf("caller mutation leaked into the catalog: %v", again[0].Models)
	}
}

func TestProviderCatalogReportsWarmingDisabled(t *testing.T) {
	f := newFakeUpstream(t)
	cat, _ := buildCatalog(t, 300*time.Second, newUpstreamProvider(t, "acme", f))
	cat.seed(map[string][]string{"acme": {"seed-model"}})

	got := cat.providerSnapshot()
	if len(got) != 1 || got[0].Warming {
		t.Fatalf("warming = true, want false when the warm loop never runs: %+v", got)
	}
}

// Server-level: the export the dashboard adapter calls, on a real Server built
// from config with warming off (so no background discovery races the assertion).
func TestServerProviderCatalogSeededWithWarmingOff(t *testing.T) {
	srv := newServerWithMock(t)

	got := srv.ProviderCatalog()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	e := got[0]
	if e.Name != "mock" || e.Warming || e.Discovered {
		t.Errorf("entry = %+v, want the seeded mock provider with warming off", e)
	}
	if !reflect.DeepEqual(e.Models, []string{"mock-model"}) {
		t.Errorf("models = %v, want the config seed [mock-model]", e.Models)
	}
}

func TestServerProviderCatalogNilSafe(t *testing.T) {
	s := &Server{}
	if got := s.ProviderCatalog(); got != nil {
		t.Errorf("ProviderCatalog on a handler-less server = %v, want nil", got)
	}
}
