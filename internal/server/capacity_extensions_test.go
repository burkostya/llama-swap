package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"gopkg.in/yaml.v3"
)

type capacityLifecycleExtension struct {
	name                string
	ready, stop, closed atomic.Int32
	lastGeneration      atomic.Uint64
}

func (e *capacityLifecycleExtension) Name() string { return e.name }
func (e *capacityLifecycleExtension) BackendReady(_ context.Context, runtime extension.BackendRuntime) error {
	e.ready.Add(1)
	e.lastGeneration.Store(runtime.Generation)
	return nil
}
func (e *capacityLifecycleExtension) BackendStopping(context.Context, extension.BackendRuntime) error {
	e.stop.Add(1)
	return nil
}
func (e *capacityLifecycleExtension) BackendStopped(context.Context, extension.BackendRuntime, extension.StopResult) {
	e.closed.Add(1)
}

type capacityLifecycleFactory struct {
	instances map[string]*capacityLifecycleExtension
}

func (*capacityLifecycleFactory) Type() string { return "test.capacity.lifecycle" }
func (f *capacityLifecycleFactory) Build(ctx extension.BuildContext, _ yaml.Node) (extension.Extension, error) {
	e := &capacityLifecycleExtension{name: ctx.ExtensionName}
	f.instances[ctx.ModelID] = e
	return e, nil
}

func TestServer_CapacityExtensionLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires the sleep command")
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
routing:
  router:
    use: capacity
    settings:
      capacity:
        pools: {ram: 10}
models:
  a:
    cmd: sleep 60
    proxy: http://127.0.0.1:1
    checkEndpoint: none
    capacities: {ram: 10}
    extensions: [{name: lifecycle, type: test.capacity.lifecycle}]
  b:
    cmd: sleep 60
    proxy: http://127.0.0.1:1
    checkEndpoint: none
    capacities: {ram: 10}
    extensions: [{name: lifecycle, type: test.capacity.lifecycle}]
`))
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	factory := &capacityLifecycleFactory{instances: map[string]*capacityLifecycleExtension{}}
	registry := extension.NewRegistry()
	registry.MustRegister(factory)
	s, err := New(cfg, logmon.NewGroup(io.Discard, true, true, true), nil, store, BuildInfo{}, nil, nil, nil, nil, WithExtensionRegistry(registry))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(3 * time.Second)
	request := func(model string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, chatRequest(model))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("model %s: status=%d body=%s", model, w.Code, w.Body.String())
		}
	}
	request("a")
	a, b := factory.instances["a"], factory.instances["b"]
	if a.ready.Load() != 1 || a.lastGeneration.Load() != 1 {
		t.Fatalf("first backend did not initialize extension: %+v", a)
	}
	request("b")
	if a.stop.Load() != 1 || a.closed.Load() != 1 || b.ready.Load() != 1 {
		t.Fatalf("eviction did not run extension lifecycle: a stop=%d stopped=%d b ready=%d", a.stop.Load(), a.closed.Load(), b.ready.Load())
	}
	request("a")
	if a.ready.Load() != 2 || a.lastGeneration.Load() != 2 {
		t.Fatalf("restarted backend lost extension generation: ready=%d generation=%d", a.ready.Load(), a.lastGeneration.Load())
	}
}
