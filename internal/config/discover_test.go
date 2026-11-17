package config

import "testing"

func TestDiscoverFromEnv(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "gsk_testkey")
	t.Setenv("ANTHROPIC_API_KEY", "nexus-local") // placeholder → must be skipped

	disc := DiscoverFromEnv(nil)
	found := map[string]string{}
	for _, p := range disc {
		found[p.Name] = p.APIKey
	}
	if found["groq"] != "env:GROQ_API_KEY" {
		t.Errorf("groq should be discovered as env:GROQ_API_KEY, got %q", found["groq"])
	}
	if _, ok := found["anthropic"]; ok {
		t.Error("ANTHROPIC_API_KEY=nexus-local must not be discovered")
	}
}

// TestEnvKeyVarsCoversDiscoveryAndIsACopy pins the accessor to the single list
// discovery scans: a caller that blanks everything it reports must see no
// provider discovered, and the returned slice must not be a window into the
// internal list.
func TestEnvKeyVarsCoversDiscoveryAndIsACopy(t *testing.T) {
	vars := EnvKeyVars()
	if len(vars) == 0 {
		t.Fatal("EnvKeyVars returned no variables")
	}
	for _, v := range vars {
		t.Setenv(v, "")
	}
	if got := DiscoverFromEnv(nil); len(got) != 0 {
		t.Errorf("discovered %v after blanking every reported variable — the accessor must cover discovery", got)
	}

	original := knownEnvKeys[0].vars[0]
	vars[0] = "MUTATED_BY_TEST"
	if again := EnvKeyVars(); again[0] != original {
		t.Errorf("EnvKeyVars()[0] = %q after mutating a previous result, want %q — the accessor must return a copy", again[0], original)
	}
}

func TestDiscoverSkipsExisting(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "gsk_testkey")
	disc := DiscoverFromEnv([]Provider{{Name: "groq"}})
	for _, p := range disc {
		if p.Name == "groq" {
			t.Error("already-configured groq must not be re-discovered")
		}
	}
}
