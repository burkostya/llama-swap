//go:build linux

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
)

type apiResourceProcess struct {
	state       atomic.Value
	logger      *logmon.Monitor
	stopGate    <-chan struct{}
	stopStarted chan struct{}
	stopOnce    sync.Once
}

func (p *apiResourceProcess) EnsureReady(context.Context, time.Duration) error {
	p.state.Store(process.StateReady)
	return nil
}
func (p *apiResourceProcess) Run(time.Duration) error       { return p.EnsureReady(context.Background(), 0) }
func (*apiResourceProcess) WaitReady(context.Context) error { return nil }
func (p *apiResourceProcess) Stop(time.Duration) error {
	if p.stopStarted != nil {
		p.stopOnce.Do(func() { close(p.stopStarted) })
	}
	if p.stopGate != nil {
		<-p.stopGate
	}
	p.state.Store(process.StateStopped)
	return nil
}
func (p *apiResourceProcess) State() process.ProcessState {
	return p.state.Load().(process.ProcessState)
}
func (p *apiResourceProcess) Status() process.Status                         { return process.Status{State: p.State()} }
func (*apiResourceProcess) ServeHTTP(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (p *apiResourceProcess) Logger() *logmon.Monitor                        { return p.logger }
func (*apiResourceProcess) ResourceIdentity() *process.ResourceIdentity      { return nil }
func (*apiResourceProcess) ResourceStopError() string                        { return "" }
func (p *apiResourceProcess) ForceResourceStop() error                       { return p.Stop(0) }

type apiResourceFactory struct {
	started     atomic.Int32
	stopGate    <-chan struct{}
	stopStarted chan struct{}
}

func (f *apiResourceFactory) New(_ context.Context, _ string, _ config.ModelConfig, logger, _ *logmon.Monitor) (process.Process, error) {
	f.started.Add(1)
	p := &apiResourceProcess{logger: logger, stopGate: f.stopGate, stopStarted: f.stopStarted}
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
	r.SetStartGate(s.pause.startGate)
	t.Cleanup(func() { _ = r.Shutdown(0); s.store.Close(); s.shutdownFn() })
	return s, r, f
}

func TestProxy_ResourcesPauseWaitsAndRejectsDemand(t *testing.T) {
	s, r, f := resourceAPIServer(t)
	stopGate := make(chan struct{})
	stopStarted := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(stopGate) }) })
	f.stopGate, f.stopStarted = stopGate, stopStarted
	body := `{"model":"worker","source":"main","hasWork":true}`
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, "secret"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	pauseDone := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	request := httptest.NewRequest("POST", "/api/models/pause", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer secret")
	go func() {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, request)
		pauseDone <- w
	}()
	select {
	case <-stopStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not begin worker stop")
	}
	cancel() // A disconnected client must not cancel drain or clear pause.
	if !s.pause.Paused() {
		t.Fatal("client cancellation cleared pause")
	}
	for _, work := range []string{"true", "false"} {
		w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{"model":"worker","source":"main","hasWork":`+work+`}`, "secret")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "paused") {
			t.Fatalf("demand while paused: status=%d body=%q", w.Code, w.Body.String())
		}
	}
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", `{}`, "secret"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid demand status=%d", w.Code)
	}
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized demand status=%d", w.Code)
	}
	if w := resourceAPIRequest(s, "GET", "/api/resources", "", "secret"); w.Code != http.StatusOK || r.Snapshot().Pools["gpu0"].Owner != "worker" {
		t.Fatal("status unavailable or live owner released during pause")
	}
	resumeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { resumeDone <- resourceAPIRequest(s, "POST", "/api/models/resume", "", "secret") }()
	select {
	case <-pauseDone:
		t.Fatal("pause returned before worker exit")
	case <-resumeDone:
		t.Fatal("resume returned before pause completed")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(stopGate) })
	select {
	case w := <-pauseDone:
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"paused":true}` {
			t.Fatalf("pause: status=%d body=%q", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not finish")
	}
	select {
	case w := <-resumeDone:
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"paused":false}` {
			t.Fatalf("resume: status=%d body=%q", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not finish")
	}
	if f.started.Load() != 1 {
		t.Fatal("old demand restarted worker")
	}
	f.stopGate, f.stopStarted = nil, nil
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, "secret"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.started.Load() != 2 {
		t.Fatal("fresh heartbeat did not restart worker")
	}
}

func TestProxy_ResourcesPauseSurvivesReload(t *testing.T) {
	s, _, _ := resourceAPIServer(t)
	if w := resourceAPIRequest(s, "POST", "/api/models/pause", "", "secret"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	restore, err := s.PrepareResourceReload(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := New(s.cfg, s.logs, nil, s.store, BuildInfo{}, nil, nil, nil, s.pause)
	if err != nil {
		restore()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Shutdown(time.Second) })
	body := `{"model":"worker","source":"main","hasWork":true}`
	if w := resourceAPIRequest(replacement, "PUT", "/api/resources/demand", body, "secret"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "paused") {
		t.Fatalf("reload lost pause: status=%d body=%q", w.Code, w.Body.String())
	}
	if w := resourceAPIRequest(replacement, "POST", "/api/models/resume", "", "secret"); w.Code != http.StatusOK || s.pause.Paused() {
		t.Fatalf("resume after reload: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestProxy_ResourcesPausedReloadFailureRestoresLedger(t *testing.T) {
	s, _, f := resourceAPIServer(t)
	if w := resourceAPIRequest(s, "POST", "/api/models/pause", "", "secret"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	restore, err := s.PrepareResourceReload(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	bad := s.cfg
	bad.Models = maps.Clone(s.cfg.Models)
	model := bad.Models["worker"]
	model.Extensions = []config.ExtensionConfig{{Name: "unknown", Type: "unknown"}}
	bad.Models["worker"] = model
	_, err = New(bad, s.logs, nil, s.store, BuildInfo{}, nil, nil, nil, s.pause, WithExtensionRegistry(extension.NewRegistry()))
	if err == nil {
		t.Fatal("invalid extension accepted during reload")
	}
	restore()
	body := `{"model":"worker","source":"main","hasWork":true}`
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, "secret"); w.Code != http.StatusServiceUnavailable || !s.pause.Paused() {
		t.Fatalf("failed reload lost pause: status=%d body=%q", w.Code, w.Body.String())
	}
	if w := resourceAPIRequest(s, "POST", "/api/models/resume", "", "secret"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := resourceAPIRequest(s, "PUT", "/api/resources/demand", body, "secret"); w.Code != http.StatusOK || f.started.Load() != 1 {
		t.Fatalf("failed reload did not restore ledger: status=%d body=%q", w.Code, w.Body.String())
	}
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
