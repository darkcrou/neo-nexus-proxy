package providers

import "testing"

// TestVisionModelBuiltInWithOverride confirms that a built-in provider whose
// Spec sets VisionModel is wrapped in overridden and satisfies VisionCapable,
// returning the operator-configured model ID.
func TestVisionModelBuiltInWithOverride(t *testing.T) {
	p, err := New(Spec{Name: "groq", Type: "groq", APIKey: "key", VisionModel: "llama-vision-90b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vc, ok := p.(VisionCapable)
	if !ok {
		t.Fatalf("expected provider to satisfy VisionCapable")
	}
	if got := vc.VisionModel("claude-sonnet-4-6"); got != "llama-vision-90b" {
		t.Errorf("VisionModel = %q, want %q", got, "llama-vision-90b")
	}
}

// TestVisionModelBuiltInNoOverride confirms that a built-in provider with no
// overrides at all (no VisionModel, no ModelMap/pricing/tier/off-peak) is
// returned unwrapped and therefore does not satisfy VisionCapable — New()'s
// existing wrap-or-not condition simply never wraps it.
func TestVisionModelBuiltInNoOverride(t *testing.T) {
	p, err := New(Spec{Name: "groq", Type: "groq", APIKey: "key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(VisionCapable); ok {
		t.Errorf("expected provider with no overrides to NOT satisfy VisionCapable")
	}
}

// TestVisionModelGenericCustom confirms that a fully config-driven
// OpenAI-compatible (Generic) provider always satisfies VisionCapable and
// returns the configured VisionModel.
func TestVisionModelGenericCustom(t *testing.T) {
	p, err := New(Spec{
		Name:        "my-custom",
		Type:        "openai-compatible",
		BaseURL:     "https://example.com/v1",
		APIKey:      "key",
		VisionModel: "vision-model-x",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vc, ok := p.(VisionCapable)
	if !ok {
		t.Fatalf("expected Generic provider to satisfy VisionCapable")
	}
	if got := vc.VisionModel("claude-sonnet-4-6"); got != "vision-model-x" {
		t.Errorf("VisionModel = %q, want %q", got, "vision-model-x")
	}
}

// TestVisionModelGenericCustomNoOverride confirms that a Generic provider
// with no VisionModel set still satisfies VisionCapable (it always wraps
// image support, unlike the built-in path) but returns "".
func TestVisionModelGenericCustomNoOverride(t *testing.T) {
	p, err := New(Spec{
		Name:    "my-custom",
		Type:    "openai-compatible",
		BaseURL: "https://example.com/v1",
		APIKey:  "key",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vc, ok := p.(VisionCapable)
	if !ok {
		t.Fatalf("expected Generic provider to satisfy VisionCapable even with no VisionModel set")
	}
	if got := vc.VisionModel("claude-sonnet-4-6"); got != "" {
		t.Errorf("VisionModel = %q, want empty string", got)
	}
}
