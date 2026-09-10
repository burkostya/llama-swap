package extension

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// FailurePolicy controls whether a backend-ready hook failure prevents the
// generation from serving requests.
type FailurePolicy string

const (
	// FailureFail aborts backend startup, preserving correctness when an
	// extension cannot safely be bypassed.
	FailureFail FailurePolicy = "fail"
	// FailureDisable skips the failed extension for only the current generation.
	FailureDisable FailurePolicy = "disable"
	// FailureWarn logs the error but leaves the extension active.
	FailureWarn FailurePolicy = "warn"
)

const (
	defaultReadyTimeout = 30 * time.Second
	defaultStopTimeout  = 30 * time.Second
)

// Entry binds one configured instance to its lifecycle policy and deadlines.
type Entry struct {
	Name          string        // Name is the configured instance identity.
	Type          string        // Type is the compile-time factory identity.
	Extension     Extension     // Extension owns the instance implementation.
	FailurePolicy FailurePolicy // FailurePolicy applies to backend-ready errors.
	ReadyTimeout  time.Duration // ReadyTimeout bounds generation initialization.
	StopTimeout   time.Duration // StopTimeout bounds each unwind hook.

	active bool
}

// Chain composes the model-scoped extensions that accompany a process across
// repeated backend generations. Its mutable fields track generation activity,
// not the backend-specific state owned by individual extensions.
type Chain struct {
	modelID string
	entries []Entry
	logger  *logmon.Monitor

	mu         sync.RWMutex
	runtime    BackendRuntime
	hasRuntime bool
	generation atomic.Uint64
}

func newChain(modelID string, entries []Entry, logger *logmon.Monitor) *Chain {
	return &Chain{modelID: modelID, entries: entries, logger: logger}
}

// NextGeneration returns a monotonically increasing identity for the next
// backend lifetime. Extensions use it to reject stale cleanup from older
// processes without discarding model-lifetime logical state.
func (c *Chain) NextGeneration() uint64 { return c.generation.Add(1) }

// Empty reports whether the model has any configured extension instances.
func (c *Chain) Empty() bool { return len(c.entries) == 0 }

// Entries returns a snapshot so control-plane readers cannot mutate the chain.
func (c *Chain) Entries() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := make([]Entry, len(c.entries))
	copy(entries, c.entries)
	return entries
}

func (c *Chain) setActive(index int, active bool) {
	c.mu.Lock()
	c.entries[index].active = active
	c.mu.Unlock()
}

func (c *Chain) isActive(index int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[index].active
}

// BackendReady initializes extensions in configuration order before requests
// can reach this backend generation.
func (c *Chain) BackendReady(ctx context.Context, runtime BackendRuntime) error {
	c.mu.Lock()
	c.runtime = runtime
	c.hasRuntime = true
	// Disable is generation-local: a clean backend restart must retry every
	// configured extension instead of carrying a transient failure forever.
	for i := range c.entries {
		c.entries[i].active = true
	}
	c.mu.Unlock()

	for i := range c.entries {
		entry := &c.entries[i]
		hook, ok := entry.Extension.(BackendReadyHook)
		if !ok {
			continue
		}
		err := c.call(ctx, *entry, entry.ReadyTimeout, "backend ready", func(hookCtx context.Context) error {
			return hook.BackendReady(hookCtx, runtime)
		})
		if err == nil {
			continue
		}
		switch entry.FailurePolicy {
		case FailureDisable:
			// A partially initialized extension gets the same transient cleanup
			// notification as a failed backend start before it is bypassed.
			if stopped, ok := entry.Extension.(BackendStoppedHook); ok {
				_ = c.call(context.Background(), *entry, entry.StopTimeout, "disable cleanup", func(cleanupCtx context.Context) error {
					stopped.BackendStopped(cleanupCtx, runtime, StopResult{Err: err})
					return nil
				})
			}
			c.setActive(i, false)
			c.logger.Warnf("<%s> extension %s disabled for generation %d: %v", c.modelID, entry.Name, runtime.Generation, err)
		case FailureWarn:
			c.logger.Warnf("<%s> extension %s backend-ready warning: %v", c.modelID, entry.Name, err)
		default:
			return fmt.Errorf("extension %s: %w", entry.Name, err)
		}
	}
	return nil
}

// WrapUpstream applies wrappers in reverse construction order, making the
// first configured extension the outermost middleware.
func (c *Chain) WrapUpstream(runtime BackendRuntime, next http.Handler) http.Handler {
	for i := len(c.entries) - 1; i >= 0; i-- {
		entry := c.entries[i]
		if !c.isActive(i) {
			continue
		}
		wrapper, ok := entry.Extension.(UpstreamHandlerWrapper)
		if !ok {
			continue
		}
		next = wrapper.WrapUpstream(runtime, next)
		next = c.recoverHandler(entry, next)
	}
	return next
}

// BackendStopping unwinds hooks in reverse order so consumers release or save
// state before the providers they depend on. All errors are collected because
// no extension is allowed to prevent process termination.
func (c *Chain) BackendStopping(ctx context.Context, runtime BackendRuntime) error {
	var errs []error
	for i := len(c.entries) - 1; i >= 0; i-- {
		entry := c.entries[i]
		if !c.isActive(i) {
			continue
		}
		hook, ok := entry.Extension.(BackendStoppingHook)
		if !ok {
			continue
		}
		if err := c.call(ctx, entry, entry.StopTimeout, "backend stopping", func(hookCtx context.Context) error {
			return hook.BackendStopping(hookCtx, runtime)
		}); err != nil {
			c.logger.Warnf("<%s> extension %s stop hook failed: %v", c.modelID, entry.Name, err)
			errs = append(errs, fmt.Errorf("extension %s: %w", entry.Name, err))
		}
	}
	return errors.Join(errs...)
}

// BackendStopped notifies active extensions that an intentional generation is
// gone and then clears only the matching runtime marker.
func (c *Chain) BackendStopped(ctx context.Context, runtime BackendRuntime, result StopResult) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		entry := c.entries[i]
		if !c.isActive(i) {
			continue
		}
		hook, ok := entry.Extension.(BackendStoppedHook)
		if !ok {
			continue
		}
		if err := c.call(ctx, entry, entry.StopTimeout, "backend stopped", func(hookCtx context.Context) error {
			hook.BackendStopped(hookCtx, runtime, result)
			return nil
		}); err != nil {
			c.logger.Warnf("<%s> extension %s stopped hook failed: %v", c.modelID, entry.Name, err)
		}
	}
	c.clearRuntime(runtime.Generation)
}

// BackendExited performs the same reverse-order transient cleanup for a
// generation that ended unexpectedly.
func (c *Chain) BackendExited(ctx context.Context, runtime BackendRuntime, exit BackendExit) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		entry := c.entries[i]
		if !c.isActive(i) {
			continue
		}
		hook, ok := entry.Extension.(BackendExitedHook)
		if !ok {
			continue
		}
		if err := c.call(ctx, entry, entry.StopTimeout, "backend exited", func(hookCtx context.Context) error {
			hook.BackendExited(hookCtx, runtime, exit)
			return nil
		}); err != nil {
			c.logger.Warnf("<%s> extension %s exit hook failed: %v", c.modelID, entry.Name, err)
		}
	}
	c.clearRuntime(runtime.Generation)
}

func (c *Chain) clearRuntime(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A late cleanup callback from an older process must not erase the runtime
	// of a replacement generation that has already become ready.
	if c.hasRuntime && c.runtime.Generation == generation {
		c.hasRuntime = false
	}
}

// Close unwinds model-lifetime resources after all process generations have
// stopped. It attempts every closer and returns their joined errors.
func (c *Chain) Close(ctx context.Context) error {
	var errs []error
	for i := len(c.entries) - 1; i >= 0; i-- {
		entry := c.entries[i]
		closer, ok := entry.Extension.(Closer)
		if !ok {
			continue
		}
		if err := c.call(ctx, entry, entry.StopTimeout, "close", closer.Close); err != nil {
			errs = append(errs, fmt.Errorf("extension %s: %w", entry.Name, err))
		}
	}
	return errors.Join(errs...)
}

// call isolates one lifecycle hook behind its own deadline and panic boundary.
// Hooks are expected to honor ctx; a timed-out hook may keep running if it
// ignores cancellation, but the lifecycle caller is never blocked by it.
func (c *Chain) call(parent context.Context, entry Entry, timeout time.Duration, operation string, fn func(context.Context) error) error {
	if timeout <= 0 {
		if operation == "backend ready" {
			timeout = defaultReadyTimeout
		} else {
			timeout = defaultStopTimeout
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Buffering lets a well-behaved hook report completion after its caller has
	// already selected the timeout path, without leaking the hook goroutine.
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
				c.logger.Errorf("<%s> extension %s %s panic: %v\n%s", c.modelID, entry.Name, operation, recovered, debug.Stack())
			}
			done <- err
		}()
		err = fn(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", operation, ctx.Err())
	}
}
