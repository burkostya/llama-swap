package process

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

const extensionCleanupTimeout = time.Second
const minimumKillTimeout = 100 * time.Millisecond

// extensionsBackendReady creates the generation boundary only after the
// backend is usable, then installs wrappers before the handler is published by
// the process state machine. The checkEndpoint=none path uses the same seam.
func (p *ProcessCommand) extensionsBackendReady(ctx context.Context, proxyURL *url.URL, handler http.Handler) (extension.BackendRuntime, http.Handler, error) {
	runtime := extension.BackendRuntime{ModelID: p.id, BaseURL: proxyURL}
	if p.extensions == nil || p.extensions.Empty() {
		return runtime, handler, nil
	}
	runtime.Generation = p.extensions.NextGeneration()
	if err := p.extensions.BackendReady(ctx, runtime); err != nil {
		return runtime, nil, err
	}
	return runtime, p.extensions.WrapUpstream(runtime, handler), nil
}

// stopProcessWithExtensions shares one deadline between the blocking stop
// hooks and process termination. Hook failures are cleanup results, never a
// reason to leave the backend running.
func (p *ProcessCommand) stopProcessWithExtensions(runtime extension.BackendRuntime, cmd *exec.Cmd, cancel context.CancelFunc, done <-chan struct{}, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var hookErr error
	if p.extensions != nil && runtime.Generation != 0 {
		ctx, stop := context.WithDeadline(context.Background(), deadline)
		hookErr = p.extensions.BackendStopping(ctx, runtime)
		stop()
	}

	remaining := time.Until(deadline)
	// Even an extension that consumes the deadline cannot prevent a final kill
	// attempt. This small floor bounds the safety action instead of hanging
	// shutdown on an already-expired graceful budget.
	if remaining < minimumKillTimeout {
		remaining = minimumKillTimeout
	}
	forced := p.killProcess(cmd, cancel, done, remaining)

	if p.extensions != nil && runtime.Generation != 0 {
		// BackendStopped cannot delay process termination because the process is
		// already gone; it gets a separate short bound for transient cleanup.
		ctx, stop := context.WithTimeout(context.Background(), extensionCleanupTimeout)
		p.extensions.BackendStopped(ctx, runtime, extension.StopResult{Forced: forced, Err: hookErr})
		stop()
	}
}

// extensionsBackendExited reports an unexpected death for the exact generation
// whose process state was observed, allowing extensions to invalidate hot
// bindings while preserving durable logical state.
func (p *ProcessCommand) extensionsBackendExited(runtime extension.BackendRuntime, cmd *exec.Cmd) {
	if p.extensions == nil || runtime.Generation == 0 {
		return
	}
	exit := extension.BackendExit{ExitCode: -1, Err: fmt.Errorf("upstream exited unexpectedly")}
	if cmd != nil && cmd.ProcessState != nil {
		exit.ExitCode = cmd.ProcessState.ExitCode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), extensionCleanupTimeout)
	p.extensions.BackendExited(ctx, runtime, exit)
	cancel()
}

// extensionsStartFailed balances a partially completed BackendReady sequence
// when startup is aborted before a wrapped handler becomes active.
func (p *ProcessCommand) extensionsStartFailed(runtime extension.BackendRuntime, err error) {
	if p.extensions == nil || runtime.Generation == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), extensionCleanupTimeout)
	p.extensions.BackendStopped(ctx, runtime, extension.StopResult{Err: err})
	cancel()
}
