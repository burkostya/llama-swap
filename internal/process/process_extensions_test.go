package process

import (
	"context"
	"net/http"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"gopkg.in/yaml.v3"
)

type processHookFactory struct {
	calls *[]string
	mu    *sync.Mutex
}

func (processHookFactory) Type() string { return "test.process-hooks" }
func (f processHookFactory) Build(ctx extension.BuildContext, _ yaml.Node) (extension.Extension, error) {
	return &processHookExtension{name: ctx.ExtensionName, calls: f.calls, mu: f.mu}, nil
}

type processHookExtension struct {
	name  string
	calls *[]string
	mu    *sync.Mutex
}

func (e *processHookExtension) Name() string { return e.name }
func (e *processHookExtension) add(value string) {
	e.mu.Lock()
	*e.calls = append(*e.calls, value)
	e.mu.Unlock()
}
func (e *processHookExtension) BackendReady(context.Context, extension.BackendRuntime) error {
	e.add("ready")
	return nil
}
func (e *processHookExtension) WrapUpstream(_ extension.BackendRuntime, next http.Handler) http.Handler {
	e.add("wrap")
	return next
}
func (e *processHookExtension) BackendStopping(context.Context, extension.BackendRuntime) error {
	e.add("stopping")
	return nil
}
func (e *processHookExtension) BackendStopped(context.Context, extension.BackendRuntime, extension.StopResult) {
	e.add("stopped")
}

func TestProcessCommand_ExtensionLifecycleSeams(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep executable not available")
	}
	var calls []string
	var mu sync.Mutex
	registry := extension.NewRegistry()
	registry.MustRegister(processHookFactory{calls: &calls, mu: &mu})
	manager, err := extension.NewManager(config.Config{Models: map[string]config.ModelConfig{
		"test": {Extensions: []config.ExtensionConfig{{Name: "hooks", Type: "test.process-hooks"}}},
	}}, registry, logmon.New())
	if err != nil {
		t.Fatal(err)
	}
	chain, err := manager.ModelChain("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, err := New(ctx, "test", config.ModelConfig{
		Cmd: sleep + " 30", Proxy: "http://127.0.0.1:1", CheckEndpoint: "none",
	}, logmon.New(), logmon.New(), WithExtensions(chain))
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- p.Run(2 * time.Second) }()
	readyCtx, readyCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer readyCancel()
	if err := p.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := p.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{"ready", "wrap", "stopping", "stopped"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}
