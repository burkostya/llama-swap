//go:build linux

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
)

type apiResourceProcess struct {
	state  atomic.Value
	logger *logmon.Monitor
}

func (p *apiResourceProcess) EnsureReady(context.Context, time.Duration) error {
	p.state.Store(process.StateReady)
	return nil
}
func (p *apiResourceProcess) Run(time.Duration) error       { return p.EnsureReady(context.Background(), 0) }
func (*apiResourceProcess) WaitReady(context.Context) error { return nil }
func (p *apiResourceProcess) Stop(time.Duration) error {
	p.state.Store(process.StateStopped)
	return nil
}
func (p *apiResourceProcess) State() process.ProcessState {
	return p.state.Load().(process.ProcessState)
}
func (*apiResourceProcess) ServeHTTP(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (p *apiResourceProcess) Logger() *logmon.Monitor                        { return p.logger }
func (*apiResourceProcess) ResourceIdentity() *process.ResourceIdentity      { return nil }
func (*apiResourceProcess) ResourceStopError() string                        { return "" }
func (p *apiResourceProcess) ForceResourceStop() error                       { return p.Stop(0) }

type apiResourceFactory struct{ started atomic.Int32 }

func (f *apiResourceFactory) New(_ context.Context, _ string, _ config.ModelConfig, logger, _ *logmon.Monitor) (process.Process, error) {
	f.started.Add(1)
	p := &apiResourceProcess{logger: logger}
	p.state.Store(process.StateStopped)
	return p, nil
}

func resourceAPIServer(t *testing.T) (*Server, *router.Resources, *apiResourceFactory) {
	t.Helper()
	c, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`apiKeys: [secret]
routing:
  router:
    use: resources
    settings:
      resources:
        stateDir: %q
        pools: {gpu0: {parameters: {device: test-device}}}
models:
  worker:
    cmd: worker --port ${PORT}
    activation: demand
    resources: [gpu0]
`, t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	f := &apiResourceFactory{}
	logger := logmon.NewWriter(io.Discard)
	r, err := router.NewResources(c, logger, logger, router.WithProcessFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(c, r, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = r.Shutdown(0); s.store.Close(); s.shutdownFn() })
	return s, r, f
}
func resourceAPIRequest(s *Server, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestProxy_ResourcesDemandAuthenticationAndValidation(t *testing.T) {
	s, r, f := resourceAPIServer(t)
	for _, route := range []struct{ method, path, body string }{{"GET", "/api/resources", ""}, {"PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":true}`}, {"POST", "/api/resources/force-stop", `{"model":"worker","generation":1}`}} {
		if w := resourceAPIRequest(s, route.method, route.path, route.body, ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s got %d", route.path, w.Code)
		}
	}
	for _, body := range []string{`{}`, `null`, `{"model":"worker","source":"main"}`, `{"model":"worker","source":"","hasWork":true}`, `{"model":"worker","source":"main","hasWork":true,"extra":1}`, `{"model":"worker","source":"main","hasWork":true} {}`} {
		if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, "secret"); w.Code != 400 {
			t.Fatalf("bad payload accepted: %s got %d", body, w.Code)
		}
	}
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"missing","source":"main","hasWork":true}`, "secret"); w.Code != 404 {
		t.Fatal("unknown model not rejected", w.Code)
	}
	if w := resourceAPIRequest(s, "GET", "/upstream/worker/health", "", "secret"); w.Code != 503 || f.started.Load() != 0 {
		t.Fatal("HTTP polling activated a demand-only worker", w.Code)
	}
	w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":true}`, "secret")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var status router.ResourceBackendStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Generation == 0 {
		t.Fatal("missing demand status", w.Body.String(), err)
	}
	for i := 0; i < 3; i++ {
		if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":true}`, "secret"); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if f.started.Load() != 1 || len(r.Snapshot().Queue) > 0 {
		t.Fatal("duplicate demand started multiple generations")
	}
	if w := resourceAPIRequest(s, "POST", "/api/resources/force-stop", `{"model":"worker","generation":999}`, "secret"); w.Code != 409 {
		t.Fatal("wrong generation force-stop was accepted")
	}
	if w := resourceAPIRequest(s, "GET", "/api/resources", "", "secret"); w.Code != 200 || !strings.Contains(w.Body.String(), "gpu0") {
		t.Fatal("resource snapshot unavailable")
	}
}

func TestProxy_ResourceReloadFence(t *testing.T) {
	s, _, _ := resourceAPIServer(t)
	resume, err := s.PrepareResourceReload(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":true}`, "secret"); w.Code != 503 {
		t.Fatal("reload fence admitted demand")
	}
	resume()
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":true}`, "secret"); w.Code != 200 {
		t.Fatal("failed reload did not resume the original server")
	}
	if _, err := s.PrepareResourceReload(s.cfg); err == nil {
		t.Fatal("owned-resource reload was accepted")
	}
	other := s.cfg
	other.Routing.Router.Use = "group"
	if _, err := s.PrepareResourceReload(other); err == nil {
		t.Fatal("router mode transition accepted without restart")
	}
}
