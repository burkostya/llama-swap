package sessionidentity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"gopkg.in/yaml.v3"
)

func buildTestExtension(t *testing.T, configYAML string) *Extension {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(configYAML), &node); err != nil {
		t.Fatal(err)
	}
	built, err := (Factory{}).Build(extension.BuildContext{ModelID: "model", ExtensionName: "session"}, node)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built.(*Extension)
}

func TestExtension_HeaderIdentityIsHMACedAndRecorded(t *testing.T) {
	ext := buildTestExtension(t, `
sources:
  - header: X-Session-ID
tenantSources:
  - header: X-Tenant-ID
hmacSecret: test-secret
`)
	var got sessionv1.Identity
	handler := ext.WrapUpstream(extension.BackendRuntime{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		got, ok = sessionv1.FromRequest(r)
		if !ok {
			t.Error("identity missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	metadata := make(map[string]string)
	ctx := swaputil.SetContext(t.Context(), swaputil.ReqContextData{ModelID: "model", Metadata: metadata})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Session-ID", "external-id")
	r.Header.Set("X-Tenant-ID", "tenant-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	want := hmacDigest("test-secret", "model\x00tenant-a\x00external-id")
	if got.Key != want {
		t.Fatalf("key = %q, want %q", got.Key, want)
	}
	if got.Mode != sessionv1.ModePersistent {
		t.Fatalf("mode = %q", got.Mode)
	}
	if metadata["session_key"] != want {
		t.Fatalf("metadata key = %q", metadata["session_key"])
	}
}

func TestExtension_JSONSourcePreservesBody(t *testing.T) {
	ext := buildTestExtension(t, `
sources:
  - jsonPath: metadata.conversation_id
hmacSecret: test-secret
`)
	var downstream string
	handler := ext.WrapUpstream(extension.BackendRuntime{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := extension.RequestBody(r)
		if err != nil {
			t.Fatal(err)
		}
		downstream = string(body.Bytes())
	}))
	r := httptest.NewRequest(http.MethodPost, "/completion", strings.NewReader(`{"metadata":{"conversation_id":"abc"},"prompt":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(downstream, `"prompt":"hi"`) {
		t.Fatalf("downstream body = %s", downstream)
	}
}

func TestExtension_MissingIdentityRejectedOnlyOnManagedPath(t *testing.T) {
	ext := buildTestExtension(t, `
sources:
  - header: X-Session-ID
hmacSecret: test-secret
`)
	called := false
	handler := ext.WrapUpstream(extension.BackendRuntime{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/completion", strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest || called {
		t.Fatalf("managed request status=%d called=%v", w.Code, called)
	}
	called = false
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/props", nil))
	if !called {
		t.Fatal("unmanaged path did not pass through")
	}
}

func hmacDigest(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
