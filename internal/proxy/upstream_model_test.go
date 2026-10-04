package proxy

// I4: the model id sent upstream (and recorded as model_used) must be the
// same on /v1/messages and /v1/chat/completions, including the operator's
// vision_model override for image-bearing requests.

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynuxis2026-pixel/nexus-proxy/internal/providers"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/router"
	"github.com/lynuxis2026-pixel/nexus-proxy/internal/storage"
)

// vmGatewayImageBody is an OpenAI-format request with one image_url part.
const vmGatewayImageBody = `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"what is in this image?"},` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`

const vmGatewayTextBody = `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`

// modelHandler builds a single-provider handler. visionModel may be empty.
// direct selects StrategyDirect (and sets directModel exactly like NewHandler).
func modelHandler(t *testing.T, baseURL, visionModel string, direct bool) *Handler {
	t.Helper()
	impl, err := providers.New(providers.Spec{
		Name: "visionprov", Type: "openai-compatible", BaseURL: baseURL,
		Models: []string{"llama-x"}, VisionModel: visionModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	strategy := router.StrategyAuto
	if direct {
		strategy = router.StrategyDirect
	}
	rt := router.New(strategy)
	rt.AddProvider(&router.Provider{Name: "visionprov", Tier: "free", Healthy: true})
	return &Handler{
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		router:      rt,
		providers:   map[string]*activeProvider{"visionprov": {impl: impl, apiKey: "k"}},
		directModel: direct,
	}
}

func TestUpstreamModel(t *testing.T) {
	newActive := func(spec providers.Spec) *activeProvider {
		impl, err := providers.New(spec)
		if err != nil {
			t.Fatal(err)
		}
		return &activeProvider{impl: impl, apiKey: "k"}
	}
	withVision := newActive(providers.Spec{Name: "vp", Type: "openai-compatible", BaseURL: "http://x", Models: []string{"llama-x"}, VisionModel: "llama-vision-90b"})
	noVision := newActive(providers.Spec{Name: "np", Type: "openai-compatible", BaseURL: "http://x", Models: []string{"llama-x"}})
	native := newActive(providers.Spec{Name: "anthropic", Type: "anthropic", APIKey: "k", VisionModel: "should-be-ignored"})

	const asked = "claude-sonnet-4-6"
	cases := []struct {
		name   string
		h      *Handler
		active *activeProvider
		images bool
		want   string
	}{
		{"direct, image, vision_model set -> requested verbatim", &Handler{directModel: true}, withVision, true, asked},
		{"direct, text -> requested verbatim", &Handler{directModel: true}, noVision, false, asked},
		{"auto, image, vision_model set -> vision model", &Handler{}, withVision, true, "llama-vision-90b"},
		{"auto, text, vision_model set -> mapped model", &Handler{}, withVision, false, "llama-x"},
		{"auto, image, no vision_model -> mapped model", &Handler{}, noVision, true, "llama-x"},
		{"auto, image, anthropic-native -> vision_model never applied", &Handler{}, native, true, native.impl.MapModel(asked)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.upstreamModel(tc.active, asked, tc.images); got != tc.want {
				t.Errorf("upstreamModel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGateway_ImageRequest_UsesVisionModelOverride: the OpenAI gateway applies
// the provider's vision_model for image requests, like /v1/messages does, and
// leaves text-only requests on the mapped model.
func TestGateway_ImageRequest_UsesVisionModelOverride(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", false)

	if rec := chatCompletions(h, vmGatewayImageBody); rec.Code != 200 {
		t.Fatalf("image: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "llama-vision-90b" {
		t.Errorf("image request: upstream model = %q, want vision_model %q", gotModel, "llama-vision-90b")
	}

	gotModel = ""
	if rec := chatCompletions(h, vmGatewayTextBody); rec.Code != 200 {
		t.Fatalf("text: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "llama-x" {
		t.Errorf("text-only request: upstream model = %q, want mapped model %q", gotModel, "llama-x")
	}
}

func TestGateway_ImageRequest_NoVisionModelUsesMapped(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "", false)

	if rec := chatCompletions(h, vmGatewayImageBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "llama-x" {
		t.Errorf("upstream model = %q, want mapped model %q", gotModel, "llama-x")
	}
}

// TestGateway_DirectStrategy_ImageIgnoresVisionModel: StrategyDirect forwards
// the requested model id verbatim, vision_model or not.
func TestGateway_DirectStrategy_ImageIgnoresVisionModel(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", true)

	if rec := chatCompletions(h, vmGatewayImageBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "claude-sonnet-4-6" {
		t.Errorf("upstream model = %q, want requested id verbatim %q", gotModel, "claude-sonnet-4-6")
	}
}

// withModelDB attaches a real SQLite DB so logResult / recordUsageEvent persist.
func withModelDB(t *testing.T, h *Handler) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "model.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h.db = db
	return db
}

// assertModelUsed checks the single requests row and single usage event both
// name want as the model actually used.
func assertModelUsed(t *testing.T, db *storage.DB, want string) {
	t.Helper()
	rows, err := db.GetRecentRequests(5)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("requests rows = %d, want 1", len(rows))
	}
	if rows[0].ModelUsed != want {
		t.Errorf("requests.model_used = %q, want %q", rows[0].ModelUsed, want)
	}
	evs, err := db.GetUsageEvents(storage.UsageFilter{Ascending: true})
	if err != nil {
		t.Fatalf("GetUsageEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("usage events = %d, want 1", len(evs))
	}
	if evs[0].ModelUsed != want {
		t.Errorf("usage_events.model_used = %q, want %q", evs[0].ModelUsed, want)
	}
}

// TestGateway_ImageRequest_LogsVisionModelAsModelUsed: dashboard/usage rows
// name the model that was really sent for image requests.
func TestGateway_ImageRequest_LogsVisionModelAsModelUsed(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", false)
	db := withModelDB(t, h)

	if rec := chatCompletions(h, vmGatewayImageBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertModelUsed(t, db, "llama-vision-90b")
}

func TestGateway_TextRequest_LogsMappedModelAsModelUsed(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", false)
	db := withModelDB(t, h)

	if rec := chatCompletions(h, vmGatewayTextBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertModelUsed(t, db, "llama-x")
}

// TestGateway_DirectStrategy_ImageLogsRequestedModel: under direct the
// recorded model is the requested id, matching what was sent.
func TestGateway_DirectStrategy_ImageLogsRequestedModel(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", true)
	db := withModelDB(t, h)

	if rec := chatCompletions(h, vmGatewayImageBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertModelUsed(t, db, "claude-sonnet-4-6")
}

// TestMessages_ImageRequest_LogsVisionModelAsModelUsed: same attribution on
// the /v1/messages path.
func TestMessages_ImageRequest_LogsVisionModelAsModelUsed(t *testing.T) {
	var gotModel string
	srv := captureModelServer(&gotModel)
	defer srv.Close()
	h := modelHandler(t, srv.URL, "llama-vision-90b", false)
	db := withModelDB(t, h)

	if rec := doMessages(h, imageRequestBody); rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotModel != "llama-vision-90b" {
		t.Fatalf("upstream model = %q, want %q", gotModel, "llama-vision-90b")
	}
	assertModelUsed(t, db, "llama-vision-90b")
}
