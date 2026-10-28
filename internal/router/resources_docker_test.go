//go:build linux

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type resourceDockerFake struct {
	mu                                            sync.Mutex
	identity                                      resourceRuntimeIdentity
	absent, stopped, wrongLabel, wrongID, restart bool
	daemonErr, stopErr                            bool
	unknownState                                  string
	forceCalls                                    int
	queryGate                                     <-chan struct{}
}

func (d *resourceDockerFake) command(_ context.Context, _ string, args ...string) ([]byte, error) {
	d.mu.Lock()
	gate := d.queryGate
	d.mu.Unlock()
	if gate != nil {
		<-gate
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.daemonErr {
		return nil, errors.New("daemon unavailable")
	}
	if args[0] == "info" {
		return []byte("daemon-A\n"), nil
	}
	if len(args) < 2 || args[0] != "container" {
		return nil, fmt.Errorf("unexpected command %v", args)
	}
	switch args[1] {
	case "inspect":
		if d.absent {
			return nil, errors.New("not found")
		}
		id := d.identity.ID
		if id == "" {
			id = strings.Repeat("a", 64)
		}
		if d.wrongID {
			id = strings.Repeat("b", 64)
		}
		model := d.identity.Model
		if d.wrongLabel {
			model = "wrong"
		}
		status := "running"
		if d.stopped {
			status = "exited"
		}
		if d.unknownState != "" {
			status = d.unknownState
		}
		policy := "no"
		if d.restart {
			policy = "always"
		}
		return json.Marshal([]any{map[string]any{"Id": id, "Name": "/" + d.identity.Name,
			"Config":     map[string]any{"Labels": map[string]string{resourceModelLabel: model, resourceGenerationLabel: fmt.Sprint(d.identity.Generation)}},
			"State":      map[string]any{"Status": status, "Running": !d.stopped},
			"HostConfig": map[string]any{"RestartPolicy": map[string]string{"Name": policy}}}})
	case "ls":
		if d.absent {
			return nil, nil
		}
		return []byte(strings.Repeat("a", 64) + "\n"), nil
	case "kill":
		d.forceCalls++
		if len(args) != 4 || args[2] != "--signal=KILL" || args[3] != strings.Repeat("a", 64) {
			return nil, fmt.Errorf("unsafe kill arguments %v", args)
		}
		if d.stopErr {
			return nil, errors.New("stop denied")
		}
		d.stopped = true
		return []byte(args[3]), nil
	}
	return nil, fmt.Errorf("unexpected command %v", args)
}

func dockerTestIdentity() resourceRuntimeIdentity {
	return resourceRuntimeIdentity{Type: "docker", Command: "/test/docker", Name: "llama-swap-7-test", Model: "a", Generation: 7, ID: strings.Repeat("a", 64), DaemonID: "daemon-A"}
}

func TestResources_DockerInspectRequiresRuntimeProof(t *testing.T) {
	cases := []struct {
		name                string
		change              func(*resourceDockerFake, *resourceRuntimeIdentity)
		released, wantError bool
	}{
		{"running", func(*resourceDockerFake, *resourceRuntimeIdentity) {}, false, false},
		{"stopped", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.stopped = true }, true, false},
		{"confirmed absence", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.absent = true }, true, false},
		{"unknown creation", func(d *resourceDockerFake, i *resourceRuntimeIdentity) { d.absent = true; i.ID = "" }, false, true},
		{"daemon error", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.daemonErr = true }, false, true},
		{"wrong label", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.wrongLabel = true }, false, true},
		{"wrong ID", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.wrongID = true }, false, true},
		{"changed daemon", func(_ *resourceDockerFake, i *resourceRuntimeIdentity) { i.DaemonID = "different" }, false, true},
		{"restart policy", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.restart = true }, false, true},
		{"nonterminal state", func(d *resourceDockerFake, _ *resourceRuntimeIdentity) { d.stopped = true; d.unknownState = "created" }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := dockerTestIdentity()
			d := &resourceDockerFake{identity: identity}
			tc.change(d, &identity)
			result := inspectResourceDocker(identity, d.command)
			if result.released != tc.released || (result.err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

type resourceDockerFactory struct {
	resourceFactory
	docker         *resourceDockerFake
	ledger         string
	reservationErr error
	seenEnv        []string
}

func (f *resourceDockerFactory) New(ctx context.Context, id string, c config.ModelConfig, logger, upstream *logmon.Monitor) (process.Process, error) {
	if c.Runtime != nil {
		f.seenEnv = append([]string{}, c.ResourceEnvironment...)
		values := map[string]string{}
		for _, entry := range c.ResourceEnvironment {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		data, err := os.ReadFile(filepath.Join(f.ledger, "owners.json"))
		var ledger resourceLedger
		if err == nil {
			err = json.Unmarshal(data, &ledger)
		}
		if err == nil {
			owner := ledger.Owners[id]
			if owner.Runtime == nil || owner.Runtime.Name != values["LLAMA_SWAP_CONTAINER_NAME"] || fmt.Sprint(owner.Generation) != values["LLAMA_SWAP_RESOURCE_GENERATION"] {
				err = errors.New("runner started without durable generation reservation")
			}
		}
		f.reservationErr = err
		f.docker.mu.Lock()
		if ledger.Owners[id].Runtime != nil {
			f.docker.identity = *ledger.Owners[id].Runtime
		}
		f.docker.mu.Unlock()
	}
	return f.resourceFactory.New(ctx, id, c, logger, upstream)
}

func dockerTestRouter(t *testing.T, startGate chan struct{}) (*Resources, *resourceDockerFactory, *resourceDockerFake) {
	t.Helper()
	c := resourceTestConfig(t, strings.Replace(resourceModels, "a: {cmd: a,", "a: {runtime: {type: docker, command: /test/docker}, cmd: a,", 1))
	d := &resourceDockerFake{}
	f := &resourceDockerFactory{resourceFactory: resourceFactory{models: map[string][]*resourceFake{}}, docker: d, ledger: c.Routing.Router.Settings.Resources.StateDir}
	if startGate != nil {
		f.newProcess = func(id string) *resourceFake {
			p := newResourceFake()
			if id == "a" {
				p.startGate = startGate
			}
			return p
		}
	}
	logger := logmon.NewWriter(io.Discard)
	r, err := NewResources(c, logger, logger, WithProcessFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	r.submit(func() { r.runtimeCommand = d.command })
	t.Cleanup(func() {
		d.mu.Lock()
		d.stopped = true
		d.daemonErr = false
		d.stopErr = false
		d.wrongLabel = false
		d.absent = false
		d.mu.Unlock()
		f.finishAll()
		_ = r.Shutdown(0)
	})
	return r, f, d
}

func awaitDockerID(t *testing.T, r *Resources) ResourceBackendStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := r.Snapshot().Backends["a"]
		if s.Runtime != nil && s.Runtime.ID != "" {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("container ID was not persisted")
	return ResourceBackendStatus{}
}

func TestResources_DockerOutlivesCLIAndBlocksCompetitor(t *testing.T) {
	r, f, d := dockerTestRouter(t, nil)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	awaitDockerID(t, r)
	if f.reservationErr != nil {
		t.Fatal(f.reservationErr)
	}
	if len(f.seenEnv) != 5 || !strings.Contains(strings.Join(f.seenEnv, "\n"), "LLAMA_SWAP_CONTAINER_LABEL_MODEL="+resourceModelLabel+"=a") {
		t.Fatal("missing reserved runtime environment", f.seenEnv)
	}
	_ = f.latest("a").ForceResourceStop() // foreground CLI exits, daemon workload lives
	resourceDemand(t, r, "b", "main", true)
	resourceAwait(t, r, "a", "blocked")
	time.Sleep(350 * time.Millisecond)
	if f.latest("b") != nil || r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("CLI exit released live container")
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	resourceAwait(t, r, "b", "ready")
}

func TestResources_DockerPauseWaitsForContainer(t *testing.T) {
	r, f, d := dockerTestRouter(t, nil)
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "ready")
	awaitDockerID(t, r)
	r.SetStartGate(func() error { return swaputil.ErrInferencePaused })
	drained := make(chan struct{})
	go func() { r.DrainForPause(); close(drained) }()
	resourceAwait(t, r, "a", "draining")
	_ = f.latest("a").ForceResourceStop()
	resourceAwait(t, r, "a", "blocked")
	select {
	case <-drained:
		t.Fatal("pause completed while container was running")
	default:
	}
	if r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("pause released resource before container exit")
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not complete after container exit")
	}
}

func TestResources_DockerDaemonAndStopErrorsStayOwned(t *testing.T) {
	r, f, d := dockerTestRouter(t, nil)
	resourceDemand(t, r, "a", "main", true)
	s := awaitDockerID(t, r)
	d.mu.Lock()
	d.daemonErr = true
	d.mu.Unlock()
	resourceAwait(t, r, "a", "blocked")
	resourceDemand(t, r, "b", "main", true)
	if f.latest("b") != nil {
		t.Fatal("daemon error freed resource")
	}
	d.mu.Lock()
	d.daemonErr = false
	d.stopErr = true
	d.mu.Unlock()
	if err := r.ForceStop("a", s.Generation+1); err == nil {
		t.Fatal("stale generation accepted")
	}
	if err := r.ForceStop("a", s.Generation); err == nil {
		t.Fatal("stop error hidden")
	}
	time.Sleep(350 * time.Millisecond)
	if f.latest("b") != nil {
		t.Fatal("failed Docker stop freed resource")
	}
	d.mu.Lock()
	d.stopErr = false
	d.mu.Unlock()
	if err := r.ForceStop("a", s.Generation); err != nil {
		t.Fatal(err)
	}
	resourceAwait(t, r, "b", "ready")
}

func TestResources_DockerEarlyDrainAndAsyncQuery(t *testing.T) {
	start := make(chan struct{})
	r, f, d := dockerTestRouter(t, start)
	resourceDemand(t, r, "a", "main", true)
	resourceDemand(t, r, "b", "main", true)
	resourceAwait(t, r, "a", "draining")
	resourceDemand(t, r, "a", "main", true)
	close(start)
	deadline := time.Now().Add(5 * time.Second)
	for f.latest("a").stopCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.latest("a").stopCalls.Load() != 1 {
		t.Fatal("early drain repeated or missed stop")
	}
	gate := make(chan struct{})
	d.mu.Lock()
	d.queryGate = gate
	d.mu.Unlock()
	_ = f.latest("a").ForceResourceStop()
	// A daemon query may wait; actor APIs and unrelated resource operations remain responsive.
	read := make(chan struct{})
	go func() { _ = r.Snapshot(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("runtime query blocked actor")
	}
	d.mu.Lock()
	d.queryGate = nil
	d.stopped = true
	d.mu.Unlock()
	close(gate)
	resourceAwait(t, r, "b", "ready")
}

func TestResources_DockerRecoveryRetainsOwnerAfterCLIExit(t *testing.T) {
	c := resourceTestConfig(t, resourceModels)
	dir := c.Routing.Router.Settings.Resources.StateDir
	identity := dockerTestIdentity()
	identity.ID = "" // crash before async container-ID observation
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	owner := resourceOwner{Model: "a", Generation: 7, Resources: []string{"gpu0"}, Runtime: &identity, Identity: &process.ResourceIdentity{PID: 2147483640, PGID: 2147483640, StartTime: "1", BootID: strings.TrimSpace(string(boot))}}
	data, _ := json.Marshal(map[string]any{"nextGeneration": 7, "owners": map[string]resourceOwner{"a": owner}})
	if err := os.WriteFile(filepath.Join(dir, "owners.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f := &resourceFactory{models: map[string][]*resourceFake{}}
	r := resourceTestRouter(t, c, f)
	d := &resourceDockerFake{identity: identity}
	r.submit(func() { r.runtimeCommand = d.command })
	resourceDemand(t, r, "b", "main", true)
	awaitDockerID(t, r)
	if f.latest("a") != nil || f.latest("b") != nil || r.Snapshot().Pools["gpu0"].Owner != "a" {
		t.Fatal("recovered live container was discarded/adopted")
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	resourceAwait(t, r, "b", "ready")
}

func TestResources_DockerUnknownCreateAndStaleObservation(t *testing.T) {
	r, f, d := dockerTestRouter(t, nil)
	d.mu.Lock()
	d.absent = true
	d.mu.Unlock()
	resourceDemand(t, r, "a", "main", true)
	resourceAwait(t, r, "a", "blocked")
	_ = f.latest("a").ForceResourceStop()
	resourceDemand(t, r, "b", "main", true)
	time.Sleep(350 * time.Millisecond)
	if f.latest("b") != nil {
		t.Fatal("unknown create was treated as confirmed absence")
	}
	// A missing old observation cannot erase an ID learned by another effect.
	done := make(chan struct{})
	r.submit(func() {
		b := r.backends["a"]
		b.runtime.ID = strings.Repeat("a", 64)
		if err := r.recordRuntimeIdentity(b, resourceRuntimeIdentity{DaemonID: "daemon-A"}); err != nil {
			t.Error(err)
		}
		if b.runtime.ID == "" {
			t.Error("observation erased exact ID")
		}
		close(done)
	})
	<-done
	d.mu.Lock()
	d.absent = false
	d.stopped = true
	d.mu.Unlock()
	resourceAwait(t, r, "b", "ready")
}

func TestResources_DockerQueriesHaveBoundedContext(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("shell unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = runResourceRuntimeCommand(ctx, shell, "-c", "exec sleep 20")
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("query did not honor context: %v", err)
	}
}

func TestResources_DockerStaleGenerationObservationIsIgnored(t *testing.T) {
	r, _, _ := dockerTestRouter(t, nil)
	resourceDemand(t, r, "a", "main", true)
	awaitDockerID(t, r)
	done := make(chan struct{})
	r.submit(func() {
		b := r.backends["a"]
		b.runtimePending = true
		state, reason := b.state, b.reason
		stale := *b.runtime
		stale.ID = strings.Repeat("b", 64)
		r.finishRuntimeCheck("a", b.generation-1, resourceDockerResult{identity: stale, released: true}, true)
		if !b.runtimePending || b.runtimeReleased || b.runtime.ID != strings.Repeat("a", 64) || b.state != state || b.reason != reason {
			t.Error("stale runtime event changed current generation")
		}
		b.runtimePending = false
		close(done)
	})
	<-done
}
