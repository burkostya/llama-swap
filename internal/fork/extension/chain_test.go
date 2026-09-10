package extension

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"gopkg.in/yaml.v3"
)

type recordingFactory struct {
	typ      string
	provides []string
	requires []string
	calls    *[]string
	mu       *sync.Mutex
}

type failingExtension struct {
	name       string
	stopped    bool
	wrapCalled bool
}

func (e *failingExtension) Name() string { return e.name }
func (e *failingExtension) BackendReady(context.Context, BackendRuntime) error {
	return errors.New("ready failed")
}
func (e *failingExtension) BackendStopped(context.Context, BackendRuntime, StopResult) {
	e.stopped = true
}
func (e *failingExtension) WrapUpstream(_ BackendRuntime, next http.Handler) http.Handler {
	e.wrapCalled = true
	return next
}

type panicWrapperExtension struct{ name string }

func (e panicWrapperExtension) Name() string { return e.name }
func (panicWrapperExtension) WrapUpstream(_ BackendRuntime, _ http.Handler) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
}

func (f recordingFactory) Type() string { return f.typ }
func (f recordingFactory) Specification() Specification {
	return Specification{Type: f.typ, Provides: f.provides, Requires: f.requires}
}
func (f recordingFactory) Build(ctx BuildContext, _ yaml.Node) (Extension, error) {
	return &recordingExtension{name: ctx.ExtensionName, calls: f.calls, mu: f.mu}, nil
}

type recordingExtension struct {
	name  string
	calls *[]string
	mu    *sync.Mutex
}

func (e *recordingExtension) Name() string { return e.name }
func (e *recordingExtension) record(value string) {
	e.mu.Lock()
	*e.calls = append(*e.calls, value)
	e.mu.Unlock()
}
func (e *recordingExtension) BackendReady(context.Context, BackendRuntime) error {
	e.record("ready:" + e.name)
	return nil
}
func (e *recordingExtension) BackendStopping(context.Context, BackendRuntime) error {
	e.record("stop:" + e.name)
	return nil
}
func (e *recordingExtension) Close(context.Context) error {
	e.record("close:" + e.name)
	return nil
}
func (e *recordingExtension) WrapUpstream(_ BackendRuntime, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.record("before:" + e.name)
		next.ServeHTTP(w, r)
		e.record("after:" + e.name)
	})
}

func TestChain_LifecycleAndWrapperOrder(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	registry := NewRegistry()
	registry.MustRegister(recordingFactory{typ: "first", provides: []string{"cap/first"}, calls: &calls, mu: &mu})
	registry.MustRegister(recordingFactory{typ: "second", requires: []string{"cap/first"}, calls: &calls, mu: &mu})
	manager, err := NewManager(config.Config{Models: map[string]config.ModelConfig{
		"m": {Extensions: []config.ExtensionConfig{{Name: "one", Type: "first"}, {Name: "two", Type: "second"}}},
	}}, registry, logmon.New())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	chain, err := manager.ModelChain("m")
	if err != nil {
		t.Fatal(err)
	}
	runtime := BackendRuntime{ModelID: "m", Generation: chain.NextGeneration(), BaseURL: &url.URL{Scheme: "http", Host: "example.test"}}
	if err := chain.BackendReady(context.Background(), runtime); err != nil {
		t.Fatalf("BackendReady: %v", err)
	}
	handler := chain.WrapUpstream(runtime, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		calls = append(calls, "upstream")
		mu.Unlock()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/completion", nil))
	if err := chain.BackendStopping(context.Background(), runtime); err != nil {
		t.Fatalf("BackendStopping: %v", err)
	}
	chain.BackendStopped(context.Background(), runtime, StopResult{})
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := []string{
		"ready:one", "ready:two",
		"before:one", "before:two", "upstream", "after:two", "after:one",
		"stop:two", "stop:one", "close:two", "close:one",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestManager_RejectsMissingEarlierCapability(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	registry := NewRegistry()
	registry.MustRegister(recordingFactory{typ: "consumer", requires: []string{"cap/missing"}, calls: &calls, mu: &mu})
	_, err := NewManager(config.Config{Models: map[string]config.ModelConfig{
		"m": {Extensions: []config.ExtensionConfig{{Name: "consumer", Type: "consumer"}}},
	}}, registry, logmon.New())
	if err == nil {
		t.Fatal("NewManager succeeded, want dependency error")
	}
}

func TestChain_DisablePolicyCleansUpAndSkipsWrapper(t *testing.T) {
	ext := &failingExtension{name: "failing"}
	chain := newChain("m", []Entry{{
		Name: "failing", Type: "test", Extension: ext, FailurePolicy: FailureDisable,
		ReadyTimeout: time.Second, StopTimeout: time.Second,
	}}, logmon.New())
	runtime := BackendRuntime{ModelID: "m", Generation: chain.NextGeneration()}
	if err := chain.BackendReady(t.Context(), runtime); err != nil {
		t.Fatalf("BackendReady: %v", err)
	}
	called := false
	handler := chain.WrapUpstream(runtime, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called || ext.wrapCalled {
		t.Fatalf("called=%v wrapCalled=%v", called, ext.wrapCalled)
	}
	if !ext.stopped {
		t.Fatal("disabled extension was not cleaned up")
	}
}

func TestChain_RequestPanicIsIsolated(t *testing.T) {
	chain := newChain("m", []Entry{{
		Name: "panic", Type: "panic", Extension: panicWrapperExtension{name: "panic"}, FailurePolicy: FailureFail, active: true,
	}}, logmon.New())
	runtime := BackendRuntime{ModelID: "m", Generation: 1}
	w := httptest.NewRecorder()
	chain.WrapUpstream(runtime, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
