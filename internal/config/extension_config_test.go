package config

import (
	"strings"
	"testing"
	"time"
)

func TestExtensionConfig_PreservesOpaqueConfig(t *testing.T) {
	t.Setenv("EXTENSION_TEST_SECRET", "secret-value")
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  test:
    cmd: server --port ${PORT}
    extensions:
      - name: ${MODEL_ID}-session
        type: session.identity.v1
        readyTimeout: 5s
        stopTimeout: 2
        config:
          hmacSecret: ${env.EXTENSION_TEST_SECRET}
          untouched: ${MODEL_ID}
`))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	extensions := cfg.Models["test"].Extensions
	if len(extensions) != 1 {
		t.Fatalf("extensions = %d, want 1", len(extensions))
	}
	if extensions[0].Name != "test-session" {
		t.Fatalf("extension name = %q, want test-session", extensions[0].Name)
	}
	if extensions[0].ReadyTimeout.Value() != 5*time.Second || extensions[0].StopTimeout.Value() != 2*time.Second {
		t.Fatalf("durations = %v/%v", extensions[0].ReadyTimeout.Value(), extensions[0].StopTimeout.Value())
	}
	var opaque map[string]string
	if err := extensions[0].Config.Decode(&opaque); err != nil {
		t.Fatalf("Decode opaque config: %v", err)
	}
	if opaque["hmacSecret"] != "secret-value" {
		t.Errorf("hmacSecret = %q", opaque["hmacSecret"])
	}
	if opaque["untouched"] != "${MODEL_ID}" {
		t.Errorf("core macro was unexpectedly expanded: %q", opaque["untouched"])
	}
}
