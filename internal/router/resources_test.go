//go:build linux

package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type resourceFake struct {
	state         atomic.Value
	stopBegan     chan struct{}
	stopBeganOnce sync.Once
	stopCalls     atomic.Int32
	stopGate      chan struct{}
	startGate     chan struct{}
	startErr      error
	stopOnce      sync.Once
	serveGate     chan struct{}
	serveStarted  chan struct{}
	serveOnce     sync.Once
	logger        *logmon.Monitor
	reported      atomic.Pointer[process.Status]
}

func newResourceFake() *resourceFake {
	p := &resourceFake{stopGate: make(chan struct{}), stopBegan: make(chan struct{}), startGate: make(chan struct{}), serveStarted: make(chan struct{}), logger: logmon.NewWriter(io.Discard)}
	p.state.Store(process.StateStopped)
	close(p.startGate)
	return p
}
func (p *resourceFake) EnsureReady(context.Context, time.Duration) error {
	p.state.Store(process.StateStarting)
	<-p.startGate
	if p.startErr != nil {
		p.state.Store(process.StateStopped)
		return p.startErr
	}
	p.state.Store(process.StateReady)
	return nil
}
func (p *resourceFake) Run(t time.Duration) error       { return p.EnsureReady(context.Background(), t) }
func (p *resourceFake) WaitReady(context.Context) error { return nil }
func (p *resourceFake) Stop(time.Duration) error {
	if p.State() == process.StateStopped {
		return nil
	}
	p.stopCalls.Add(1)
	p.stopBeganOnce.Do(func() { close(p.stopBegan) })
	p.state.Store(process.StateStopping)
	<-p.stopGate
	p.state.Store(process.StateStopped)
	return nil
}
func (p *resourceFake) State() process.ProcessState { return p.state.Load().(process.ProcessState) }
func (p *resourceFake) Status() process.Status {
	if st := p.reported.Load(); st != nil {
		return *st
	}
	return process.Status{State: p.State()}
}
func (p *resourceFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.serveOnce.Do(func() { close(p.serveStarted) })
	if p.serveGate != nil {
		<-p.serveGate
	}
	w.WriteHeader(200)
}
func (p *resourceFake) Logger() *logmon.Monitor                     { return p.logger }
func (p *resourceFake) ResourceIdentity() *process.ResourceIdentity { return nil }
func (p *resourceFake) ResourceStopError() string                   { return "" }
func (p *resourceFake) ForceResourceStop() error {
	p.stopOnce.Do(func() { close(p.stopGate) })
	p.state.Store(process.StateStopped)
	return nil
}

type resourceFactory struct {
	mu         sync.Mutex
	models     map[string][]*resourceFake
	newProcess func(string) *resourceFake
}

func (f *resourceFactory) New(_ context.Context, id string, _ config.ModelConfig, _, _ *logmon.Monitor) (process.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := newResourceFake()
	if f.newProcess != nil {
		p = f.newProcess(id)
	}
	f.models[id] = append(f.models[id], p)
	return p, nil
}
func (f *resourceFactory) latest(id string) *resourceFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	ps := f.models[id]
	if len(ps) == 0 {
		return nil
	}
	return ps[len(ps)-1]
}
func (f *resourceFactory) finishAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ps := range f.models {
		for _, p := range ps {
			_ = p.ForceResourceStop()
		}
	}
}

func resourceTestConfig(t *testing.T, models string) config.Config {
	t.Helper()
	body := fmt.Sprintf(`routing:
  router:
    use: resources
    settings:
      resources:
        stateDir: %q
        demandTimeout: 30s
        pools:
          gpu0: {parameters: {device: GPU-0}}
          gpu1: {parameters: {device: GPU-1}}
models:
%s`, t.TempDir(), models)
	c, err := config.LoadConfigFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const resourceModels = `  a: {cmd: a, proxy: http://127.0.0.1:5800, activation: demand, resources: [gpu0], checkEndpoint: none, drain: {timeout: 1s}}
  b: {cmd: b, proxy: http://127.0.0.1:5801, activation: demand, resources: [gpu0, gpu1], checkEndpoint: none}
  c: {cmd: c, proxy: http://127.0.0.1:5802, activation: demand, resources: [gpu1], checkEndpoint: none}
`

func resourceTestRouter(t *testing.T, c config.Config, f *resourceFactory) *Resources {
	t.Helper()
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger, WithProcessFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.finishAll()
		done := make(chan struct{})
		go func() { _ = r.Shutdown(0); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("resource cleanup did not stop")
		}
	})
	return r
}
func resourceAwait(t *testing.T, r *Resources, id, state string) ResourceBackendStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := r.Snapshot().Backends[id]
		if s.State == state {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("model %s did not reach %s: %#v", id, state, r.Snapshot())
	return ResourceBackendStatus{}
}
func resourceDemand(t *testing.T, r *Resources, id, source string, work bool) ResourceBackendStatus {
	t.Helper()
	s, _, err := r.Demand(ResourceDemand{Model: id, Source: source, HasWork: work})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func resourceAdvance(r *Resources, delta time.Duration) {
	done := make(chan struct{})
	r.submit(func() { now := r.now().Add(delta); r.now = func() time.Time { return now }; r.tick(now); close(done) })
	<-done
}

func TestResources_RunningStatusPreservesUpstreamDetails(t *testing.T) {
	p := newResourceFake()
	p.startGate = make(chan struct{})
	progress := 0.5
	p.reported.Store(&process.Status{
		State:   process.StateStarting,
		Loading: &process.LoadingProgress{Progress: &progress, Message: "loading weights"},
	})
	f := &resourceFactory{models: map[string][]*resourceFake{}, newProcess: func(string) *resourceFake { return p }}
	r := resourceTestRouter(t, resourceTestConfig(t, resourceModels), f)
	// Release startup even when an assertion below fails, before router cleanup.
	t.Cleanup(func() {
		select {
		case <-p.startGate:
		default:
			close(p.startGate)
		}
	})
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "starting")
	st := r.RunningStatus()["a"]
	if st.State != process.StateStarting || st.Loading == nil || st.Loading.Progress == nil || *st.Loading.Progress != progress || st.Loading.Message != "loading weights" || !st.ReadySince.IsZero() {
		t.Fatalf("loading snapshot = %+v", st)
	}
	readySince := time.Now()
	p.reported.Store(&process.Status{State: process.StateReady, ReadySince: readySince})
	close(p.startGate)
	resourceAwait(t, r, "a", "ready")
	st = r.RunningStatus()["a"]
	if st.State != process.StateReady || !st.ReadySince.Equal(readySince) || st.Loading != nil {
		t.Fatalf("ready snapshot = %+v", st)
	}
	if r.RunningModels()["a"] != st.State {
		t.Fatal("RunningModels disagrees with RunningStatus")
	}
	resourceDemand(t, r, "a", "main", false)
	resourceAwait(t, r, "a", "draining")
	st = r.RunningStatus()["a"]
	if st.State != process.StateStopping || !st.ReadySince.IsZero() || st.Loading != nil {
		t.Fatalf("draining snapshot = %+v", st)
	}
}

func TestResources_DeduplicateAndWithdrawSources(t *testing.T) {
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, resourceTestConfig(t, resourceModels), f)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	resourceDemand(t, r, "b", "one", true)
	resourceDemand(t, r, "b", "one", true)
	resourceDemand(t, r, "b", "two", true)
	s := r.Snapshot()
	if len(s.Queue) != 1 || len(s.Backends["b"].Sources) != 2 || s.Pools["gpu1"].Owner != "" {
		t.Fatal("duplicate queue or partial multi-resource reservation", s)
	}
	resourceDemand(t, r, "b", "one", false)
	if len(r.Snapshot().Queue) != 1 {
		t.Fatal("one source cancelled another source")
	}
	resourceDemand(t, r, "b", "two", false)
	_ = f.latest("a").ForceResourceStop()
	resourceAwait(t, r, "a", "stopped")
	if f.latest("b") != nil {
		t.Fatal("withdrawn waiter started")
	}
	resourceDemand(t, r, "c", "main", true)
	resourceAwait(t, r, "c", "ready")
	resourceAdvance(r, 31*time.Second)
	resourceAwait(t, r, "c", "draining")
	if r.Snapshot().Pools["gpu1"].Owner != "c" {
		t.Fatal("heartbeat expiry released a live owner")
	}
}

func TestResources_AtomicResourcesAndConcurrentBackends(t *testing.T) {
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, resourceTestConfig(t, resourceModels), f)
	resourceDemand(t, r, "a", "main", true)
	resourceDemand(t, r, "c", "main", true)
	resourceAwait(t, r, "a", "ready")
	resourceAwait(t, r, "c", "ready")
	resourceDemand(t, r, "b", "main", true)
	resourceAwait(t, r, "a", "draining")
	resourceAwait(t, r, "c", "draining")
	_ = f.latest("a").ForceResourceStop()
	resourceAwait(t, r, "a", "stopped")
	if f.latest("b") != nil {
		t.Fatal("two-resource worker started before both resources were free")
	}
	_ = f.latest("c").ForceResourceStop()
	resourceAwait(t, r, "b", "ready")
	s := r.Snapshot()
	if s.Pools["gpu0"].Owner != "b" || s.Pools["gpu1"].Owner != "b" {
		t.Fatal("incomplete resource allocation", s)
	}
}

func TestResources_TimeoutAndFreshDemand(t *testing.T) {
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, resourceTestConfig(t, resourceModels), f)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	resourceDemand(t, r, "b", "main", true)
	resourceAdvance(r, 2*time.Second)
	resourceAwait(t, r, "a", "blocked")
	if s := r.Snapshot(); s.Pools["gpu0"].Owner != "a" || f.latest("b") != nil {
		t.Fatal("timeout permitted an overlapping owner", s)
	}
	select {
	case <-f.latest("a").stopBegan:
	case <-time.After(5 * time.Second):
		t.Fatal("drain signal was not delivered")
	}
	resourceDemand(t, r, "a", "main", true)
	if f.latest("a").stopCalls.Load() != 1 {
		t.Fatal("heartbeat sent another drain signal")
	}
	_ = f.latest("a").ForceResourceStop()
	resourceAwait(t, r, "b", "ready")
	if r.Snapshot().Backends["a"].QueuePosition != 0 {
		t.Fatal("stale demand automatically requeued the victim")
	}
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "b", "draining")
}

func TestResources_RequestsFinishBeforeDrain(t *testing.T) {
	c := resourceTestConfig(t, resourceModels)
	a := c.Models["a"]
	a.Activation = "both"
	c.Models["a"] = a
	f := &resourceFactory{models: map[string][]*resourceFake{}, newProcess: func(id string) *resourceFake {
		p := newResourceFake()
		if id == "a" {
			p.serveGate = make(chan struct{})
		}
		return p
	}}
	r := resourceTestRouter(t, c, f)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	// Existing /upstream handling supplies the same context; exercise it via
	// the public middleware instead in the server tests. Here admit an already
	// resolved waiter so the scheduler's completion fence is deterministic.
	w := &resourceWaiter{ctx: context.Background(), reply: make(chan resourceGrant)}
	r.submit(func() { b := r.backends["a"]; b.waiters = append(b.waiters, w); r.grantWaiters(b) })
	grant := <-w.reply
	done := make(chan struct{})
	go func() {
		grant.process.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		r.submit(func() { r.backends["a"].inflight-- })
		close(done)
	}()
	<-f.latest("a").serveStarted
	resourceDemand(t, r, "b", "main", true)
	resourceAwait(t, r, "a", "draining")
	if f.latest("a").stopCalls.Load() != 0 {
		t.Fatal("stop sent while an admitted HTTP request was active")
	}
	close(f.latest("a").serveGate)
	<-done
	deadline := time.Now().Add(time.Second)
	for f.latest("a").stopCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.latest("a").stopCalls.Load() != 1 {
		t.Fatal("drain did not begin after request completion")
	}
}

func TestResources_ReloadLockFenceAndRecoveredUnknownOwner(t *testing.T) {
	c := resourceTestConfig(t, resourceModels)
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, c, f)
	logger := logmon.NewWriter(io.Discard)
	if _, err := NewResources(c, logger, logger); err == nil {
		t.Fatal("second supervisor acquired the same stateDir")
	}
	if err := r.PauseReload(); err != nil {
		t.Fatal(err)
	}
	if _, code, err := r.Demand(ResourceDemand{Model: "a", Source: "main", HasWork: true}); err == nil || code != 503 {
		t.Fatal("reload fence accepted demand")
	}
	if err := r.ResumeReload(); err != nil {
		t.Fatal(err)
	}
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	if err := r.PauseReload(); err == nil {
		t.Fatal("reload accepted an owned resource")
	}
	// Recovery deliberately uses a different directory with an uncertain
	// pre-spawn reservation. It must not be silently discarded on startup.
	other := resourceTestConfig(t, resourceModels)
	dir := other.Routing.Router.Settings.Resources.StateDir
	data := `{"nextGeneration":9,"owners":{"a":{"model":"a","generation":9,"resources":["gpu0"]}}}`
	if err := os.WriteFile(filepath.Join(dir, "owners.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := openResourceLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	if len(l.Owners) != 1 {
		t.Fatal("unconfirmed crashed reservation disappeared")
	}
}

func TestResources_Helper(t *testing.T) {
	if os.Getenv("LLAMA_RESOURCE_HELPER") != "1" {
		return
	}
	args := os.Args
	index := 0
	for index < len(args) && args[index] != "--" {
		index++
	}
	dir, mode := args[index+1], args[index+2]
	marker := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ok"), 0600); err != nil {
			os.Exit(2)
		}
	}
	if mode == "child" {
		marker("child.ready")
		for {
			if _, err := os.Stat(filepath.Join(dir, "finish")); err == nil {
				marker("child.done")
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	if mode == "hung" {
		marker("ready")
		<-ch
		marker("drain")
		select {}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestResources_Helper$", "--", dir, "child")
	child.Env = os.Environ()
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "child.ready")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	marker("ready")
	if mode == "orphan" {
		os.Exit(0)
	}
	<-ch
	marker("drain")
	if err := child.Wait(); err != nil {
		marker("child.killed")
		os.Exit(4)
	}
	os.Exit(0)
}

func realResourceConfig(t *testing.T, mode string) (config.Config, string) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf(`  a:
    cmd: %q
    env: [LLAMA_RESOURCE_HELPER=1]
    proxy: http://127.0.0.1:5800
    activation: demand
    resources: [gpu0]
    checkEndpoint: none
    drain: {timeout: 80ms}
  b: {cmd: %q, proxy: http://127.0.0.1:5801, env: [LLAMA_RESOURCE_HELPER=1], activation: demand, resources: [gpu0], checkEndpoint: none}
`, fmt.Sprintf("%s -test.run=^TestResources_Helper$ -- %s %s", exe, dir, mode), fmt.Sprintf("%s -test.run=^TestResources_Helper$ -- %s hung", exe, t.TempDir()))
	return resourceTestConfig(t, models), dir
}
func markerAwait(t *testing.T, dir, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("missing %s", name)
}
func cleanupRealResources(t *testing.T, r *Resources, dir string) {
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "finish"), []byte("done"), 0600)
		for _, b := range r.Snapshot().Backends {
			if b.Generation != 0 && b.State != "stopped" && b.State != "queued" {
				_ = r.ForceStop(b.Model, b.Generation)
			}
		}
		done := make(chan struct{})
		go func() { _ = r.Shutdown(0); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("real resource supervisor did not stop")
		}
	})
}

func TestResources_BackgroundTaskOutlivesHTTPAndDrainTimeout(t *testing.T) {
	c, dir := realResourceConfig(t, "task")
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	markerAwait(t, dir, "ready")
	resourceDemand(t, r, "b", "other", true)
	markerAwait(t, dir, "drain")
	resourceAwait(t, r, "a", "blocked")
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("drain timeout freed a background task's GPU")
	}
	if _, err := os.Stat(filepath.Join(dir, "child.killed")); err == nil {
		t.Fatal("graceful signal killed the child task")
	}
	if err := os.WriteFile(filepath.Join(dir, "finish"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	markerAwait(t, dir, "child.done")
	resourceAwait(t, r, "b", "ready")
}

func TestResources_ParentExitDoesNotReleaseDescendants(t *testing.T) {
	c, dir := realResourceConfig(t, "orphan")
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "a", "main", true)
	markerAwait(t, dir, "ready")
	resourceAwait(t, r, "a", "blocked")
	resourceDemand(t, r, "b", "other", true)
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("parent exit released a live descendant")
	}
	if err := os.WriteFile(filepath.Join(dir, "finish"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}

func TestResources_ForceStopChecksGeneration(t *testing.T) {
	c, dir := realResourceConfig(t, "hung")
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "a", "main", true)
	markerAwait(t, dir, "ready")
	owner := resourceAwait(t, r, "a", "ready")
	resourceDemand(t, r, "b", "other", true)
	resourceAwait(t, r, "a", "blocked")
	if err := r.ForceStop("a", owner.Generation+1); err == nil {
		t.Fatal("force-stop accepted wrong generation")
	}
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("bad generation changed ownership")
	}
	if err := r.ForceStop("a", owner.Generation); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}

func TestResources_StartupFailureAndCancelledWaiter(t *testing.T) {
	c := resourceTestConfig(t, resourceModels)
	f := &resourceFactory{models: map[string][]*resourceFake{}, newProcess: func(id string) *resourceFake {
		p := newResourceFake()
		if id == "a" {
			p.startErr = fmt.Errorf("fixture startup failed")
		}
		return p
	}}
	r := resourceTestRouter(t, c, f)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "stopped")
	if r.Snapshot().Pools["gpu0"].Owner != "" {
		t.Fatal("failed clean startup retained a resource")
	}
	resourceDemand(t, r, "c", "main", true)
	resourceAwait(t, r, "c", "ready")
	b := r.cfg.Models["b"]
	b.Activation = "both"
	// Config is immutable in production; set the fixture before admitting a
	// request, on the actor thread, to test the common HTTP path.
	done := make(chan struct{})
	r.submit(func() { r.backends["b"].cfg = b; close(done) })
	<-done
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("POST", "/", nil).WithContext(swaputil.SetContext(ctx, swaputil.ReqContextData{Model: "b", ModelID: "b"}))
	returned := make(chan struct{})
	go func() { r.ServeHTTP(httptest.NewRecorder(), request); close(returned) }()
	resourceAwait(t, r, "b", "queued")
	cancel()
	<-returned
	_ = f.latest("c").ForceResourceStop()
	resourceAwait(t, r, "c", "stopped")
	if f.latest("b") != nil {
		t.Fatal("cancelled queued HTTP request launched a backend")
	}
}

func TestResources_DrainWaitsForPendingStart(t *testing.T) {
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	f := &resourceFactory{models: map[string][]*resourceFake{}, newProcess: func(id string) *resourceFake {
		p := newResourceFake()
		if id == "a" {
			p.startGate = gate
		}
		return p
	}}
	r := resourceTestRouter(t, resourceTestConfig(t, resourceModels), f)
	resourceDemand(t, r, "a", "main", true)
	resourceDemand(t, r, "b", "other", true)
	resourceAwait(t, r, "a", "draining")
	if f.latest("a").stopCalls.Load() != 0 {
		t.Fatal("stop raced ahead of the pending start")
	}
	close(gate)
	select {
	case <-f.latest("a").stopBegan:
	case <-time.After(5 * time.Second):
		t.Fatal("pending start did not receive drain after completion")
	}
	_ = f.latest("a").ForceResourceStop()
	resourceAwait(t, r, "b", "ready")
}

func TestResources_TTLUnloadAndShutdownKeepOwnership(t *testing.T) {
	c := resourceTestConfig(t, resourceModels)
	a := c.Models["a"]
	a.UnloadAfter = 1
	c.Models["a"] = a
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, c, f)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	resourceAdvance(r, 2*time.Second)
	resourceAwait(t, r, "a", "draining")
	r.Unload(1*time.Millisecond, "a")
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("TTL/unload released a live owner")
	}
	finished := make(chan struct{})
	go func() { _ = r.Shutdown(time.Millisecond); close(finished) }()
	select {
	case <-finished:
		t.Fatal("shutdown returned before the live worker exited")
	case <-time.After(30 * time.Millisecond):
	}
	_ = f.latest("a").ForceResourceStop()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after worker exit")
	}
}

func TestResources_EarlyDrainWhileStarting(t *testing.T) {
	c, dir := realResourceConfig(t, "task")
	a := c.Models["a"]
	a.CheckEndpoint = "/not-listening"
	c.Models["a"] = a
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "a", "main", true)
	markerAwait(t, dir, "ready")
	resourceDemand(t, r, "b", "other", true)
	markerAwait(t, dir, "drain")
	resourceAwait(t, r, "a", "blocked")
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("early startup cancellation released active background work")
	}
	if err := os.WriteFile(filepath.Join(dir, "finish"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}

func TestResources_StopCommandFailureDoesNotFreeOwner(t *testing.T) {
	c, dir := realResourceConfig(t, "hung")
	a := c.Models["a"]
	a.CmdStop = "false"
	c.Models["a"] = a
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "a", "main", true)
	markerAwait(t, dir, "ready")
	owner := resourceAwait(t, r, "a", "ready")
	resourceDemand(t, r, "b", "other", true)
	blocked := resourceAwait(t, r, "a", "blocked")
	if !strings.Contains(blocked.Reason, "stop command failed") || r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("stop command error was treated as release", blocked)
	}
	if err := r.ForceStop("a", owner.Generation); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}

func TestResources_RecoverLiveOwnerAndIgnoreStaleGeneration(t *testing.T) {
	c, dir := realResourceConfig(t, "hung")
	logger := logmon.NewWriter(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := process.New(ctx, "a", c.Models["a"], logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureReady(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	markerAwait(t, dir, "ready")
	identity := p.ResourceIdentity()
	if identity == nil {
		t.Fatal("worker identity not recorded")
	}
	l, err := openResourceLedger(c.Routing.Router.Settings.Resources.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	l.NextGeneration = 10
	l.Owners["a"] = resourceOwner{Model: "a", Generation: 10, Resources: []string{"gpu0"}, Identity: identity}
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	l.close()
	r, err := NewResources(c, logger, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealResources(t, r, dir)
	resourceDemand(t, r, "b", "other", true)
	resourceAwait(t, r, "a", "blocked")
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("recovery admitted a competitor")
	}
	if err := r.ForceStop("a", 9); err == nil {
		t.Fatal("stale generation stopped recovered owner")
	}
	if err := r.ForceStop("a", 10); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}
