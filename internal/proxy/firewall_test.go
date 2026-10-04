package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactDetectsSecrets(t *testing.T) {
	r := &redactor{}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"key sk-ant-api03ABCDEFGHIJKLMNOPQRST mail bob@acme.io aws AKIAIOSFODNN7EXAMPLE"}]}`)
	out, m := r.redact(body)
	s := string(out)
	for _, secret := range []string{"sk-ant-api03ABCDEFGHIJKLMNOPQRST", "bob@acme.io", "AKIAIOSFODNN7EXAMPLE"} {
		if strings.Contains(s, secret) {
			t.Errorf("redacted body still contains %q", secret)
		}
	}
	if len(m) < 3 {
		t.Errorf("expected ≥3 redactions, got %d: %v", len(m), m)
	}
}

func TestRedactNoFalsePositiveOnPlainCode(t *testing.T) {
	r := &redactor{}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"func add(a, b int) int { return a + b }"}]}`)
	if _, m := r.redact(body); len(m) != 0 {
		t.Errorf("plain code should not be redacted, got %v", m)
	}
}

func TestRestoringWriterSplitPlaceholder(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newRestoringWriter(rec, map[string]string{"[NX_REDACTED_APIKEY_1]": "sk-ant-secret"})
	// placeholder split across two writes
	_, _ = rw.Write([]byte("before [NX_RED"))
	_, _ = rw.Write([]byte("ACTED_APIKEY_1] after"))
	rw.flush()
	if got := rec.Body.String(); got != "before sk-ant-secret after" {
		t.Errorf("split-placeholder restore failed: %q", got)
	}
}

// TestFirewallMaskOutboundAndRestore is the full round-trip: the provider must
// never see the secret, but the client must get it back.
func TestFirewallMaskOutboundAndRestore(t *testing.T) {
	var providerSaw string
	// mock echoes the (redacted) user content back as the assistant message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		providerSaw = string(raw)
		var oreq struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &oreq)
		content := ""
		for _, m := range oreq.Messages {
			if m.Role == "user" {
				content = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": content}}},
			"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer srv.Close()

	h := buildTestHandler(t, []testProv{{"p", "free", srv.URL}})
	h.firewall = &redactor{}

	secret := "sk-ant-api03ZZZZYYYYXXXXWWWWVVVV"
	body := `{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"deploy with ` + secret + `"}]}`
	rec := doMessages(h, body)

	if strings.Contains(providerSaw, secret) {
		t.Errorf("SECRET LEAKED to provider:\n%s", providerSaw)
	}
	if !strings.Contains(providerSaw, "[NX_REDACTED_APIKEY_1]") {
		t.Errorf("provider should have seen a placeholder, saw:\n%s", providerSaw)
	}
	var resp AnthropicResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) == 0 || !strings.Contains(resp.Content[0].Text, secret) {
		t.Errorf("client response should have the secret restored, got %+v", resp.Content)
	}
}

// imgNoise is base64-looking image data containing sequences that the apikey
// detector would match (Gemini "AIza"+20+ chars, AWS "AKIA"+16).
const imgNoise = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ" +
	"AIzaSyA1234567890abcdefghijklmnop" + "QUJD+/9z" +
	"AKIAIOSFODNN7EXAMPLE" + "AAAAAElFTkSuQmCC"

// parseBody decodes a JSON body so image payloads can be compared for
// byte-identity after redaction + re-marshal.
func parseBody(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return m
}

func TestRedactSkipsOpenAIImageDataURI(t *testing.T) {
	r := &redactor{}
	uri := "data:image/png;base64," + imgNoise
	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "mail bob@acme.io please"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": uri}},
			},
		}},
	})
	out, m := r.redact(body)
	if len(m) != 1 {
		t.Fatalf("expected exactly the text secret in restoreMap, got %v", m)
	}
	var found bool
	for _, orig := range m {
		found = orig == "bob@acme.io"
	}
	if !found {
		t.Errorf("restoreMap should hold the email, got %v", m)
	}
	if strings.Contains(string(out), "bob@acme.io") {
		t.Errorf("text secret not redacted")
	}
	parsed := parseBody(t, out)
	part := parsed["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[1].(map[string]interface{})
	if got := part["image_url"].(map[string]interface{})["url"]; got != uri {
		t.Errorf("data URI was modified:\n got %v\nwant %v", got, uri)
	}
}

func TestRedactSkipsAnthropicBase64Source(t *testing.T) {
	r := &redactor{}
	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "mail bob@acme.io please"},
				map[string]interface{}{"type": "image", "source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": imgNoise,
				}},
			},
		}},
	})
	out, m := r.redact(body)
	if len(m) != 1 {
		t.Fatalf("expected exactly the text secret in restoreMap, got %v", m)
	}
	for _, orig := range m {
		if orig != "bob@acme.io" {
			t.Errorf("restoreMap should hold the email, got %v", m)
		}
	}
	parsed := parseBody(t, out)
	part := parsed["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[1].(map[string]interface{})
	if got := part["source"].(map[string]interface{})["data"]; got != imgNoise {
		t.Errorf("source.data was modified:\n got %v\nwant %v", got, imgNoise)
	}
}

func TestRedactImageOnlyMatchesReturnBodyUntouched(t *testing.T) {
	r := &redactor{}
	uri := "data:image/jpeg;base64," + imgNoise
	for name, body := range map[string][]byte{
		"openai": mustJSON(map[string]interface{}{"messages": []interface{}{map[string]interface{}{
			"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": uri}},
			}}}}),
		"anthropic": mustJSON(map[string]interface{}{"messages": []interface{}{map[string]interface{}{
			"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": imgNoise}},
			}}}}),
	} {
		out, m := r.redact(body)
		if m != nil {
			t.Errorf("%s: expected nil restoreMap, got %v", name, m)
		}
		if string(out) != string(body) {
			t.Errorf("%s: body must be returned untouched", name)
		}
	}
}

func TestRedactStillScansHTTPImageURL(t *testing.T) {
	r := &redactor{}
	body := mustJSON(map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{
				"url": "https://img.example.com/a.png?token=abcdef1234567890XYZ",
			}},
		}}}})
	out, m := r.redact(body)
	if len(m) == 0 {
		t.Fatalf("http image URL with a credential must still be scanned")
	}
	if strings.Contains(string(out), "abcdef1234567890XYZ") {
		t.Errorf("credential in http image URL not redacted: %s", out)
	}
}

func TestIsBase64DataURI(t *testing.T) {
	cases := map[string]bool{
		"data:image/png;base64,AAAA":                        true,
		"data:image/svg+xml;base64,AAAA":                    true,
		"data:text/plain,hello":                             false,
		"https://x/y?u=data:image/png;base64,A":             false,
		"AIzaSyA1234567890abcdefghijklmnop":                 false,
		"data:" + strings.Repeat("x", 200) + ";base64,AAAA": false,
	}
	for in, want := range cases {
		if got := isBase64DataURI(in); got != want {
			t.Errorf("isBase64DataURI(%.40q) = %v, want %v", in, got, want)
		}
	}
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
