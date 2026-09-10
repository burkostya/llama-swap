package llamacppsession

import (
	"context"
	"errors"
	"fmt"
	"sort"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

// BackendReady discovers the slots belonging to a healthy backend generation
// and enables allocation only after every slot is validated as idle.
func (e *Extension) BackendReady(ctx context.Context, runtime extension.BackendRuntime) error {
	slots, err := e.client.slots(ctx, runtime)
	if err != nil {
		return fmt.Errorf("discover llama.cpp slots: %w", err)
	}
	if len(slots) == 0 {
		return fmt.Errorf("llama.cpp returned no slots")
	}
	resources := make(map[int]*resourceState, len(slots))
	for _, slot := range slots {
		if slot.ID < 0 {
			return fmt.Errorf("llama.cpp returned invalid slot id %d", slot.ID)
		}
		if slot.IsProcessing {
			return fmt.Errorf("llama.cpp slot %d is still processing after health-check", slot.ID)
		}
		if _, duplicate := resources[slot.ID]; duplicate {
			return fmt.Errorf("llama.cpp returned duplicate slot id %d", slot.ID)
		}
		resources[slot.ID] = &resourceState{ID: slot.ID}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("session manager is closed")
	}
	if e.active != 0 {
		return fmt.Errorf("cannot start generation with %d active leases", e.active)
	}
	// Slot IDs have meaning only inside this generation. Retain logical session
	// and snapshot state, but never carry a hot binding across process lifetimes.
	for _, session := range e.sessions {
		session.Slot = -1
		session.Busy = false
	}
	e.runtime = runtime
	e.resources = resources
	e.accepting = true
	e.broadcastLocked()
	e.logger.Infof("<%s> extension %s discovered %d llama.cpp slots for generation %d", e.modelID, e.name, len(resources), runtime.Generation)
	return nil
}

// BackendStopping first prevents new allocation, then drains active HTTP and
// control leases before saving every dirty resident persistent session. The
// supplied context is part of the process-wide shutdown deadline.
func (e *Extension) BackendStopping(ctx context.Context, runtime extension.BackendRuntime) error {
	e.mu.Lock()
	if e.runtime.Generation != runtime.Generation {
		e.mu.Unlock()
		return nil
	}
	e.accepting = false
	e.broadcastLocked()
	// An active lease may represent an SSE stream, so saving before it returns
	// could capture KV while llama.cpp is still mutating that slot.
	for e.active > 0 {
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain %d active session leases: %w", e.activeCount(), ctx.Err())
		case <-changed:
		}
		e.mu.Lock()
	}

	// Reserve every dirty resident session before releasing mu for backend I/O;
	// although allocation is closed, this also excludes concurrent control calls.
	keys := make([]string, 0)
	for key, session := range e.sessions {
		if session.Slot >= 0 && session.Mode == sessionv1.ModePersistent && session.Dirty {
			keys = append(keys, key)
			session.Busy = true
			if resource := e.resources[session.Slot]; resource != nil {
				resource.Busy = true
			}
		}
	}
	e.mu.Unlock()
	// Stable order makes partial progress under a deadline predictable and keeps
	// shutdown diagnostics reproducible.
	sort.Strings(keys)

	var errs []error
	for _, key := range keys {
		e.mu.Lock()
		session := e.sessions[key]
		slot := -1
		if session != nil {
			slot = session.Slot
		}
		e.mu.Unlock()
		if slot < 0 {
			continue
		}
		e.logger.Infof("<%s> extension %s saving session %s from slot %d for generation %d", e.modelID, e.name, key, slot, runtime.Generation)
		err := e.client.action(ctx, runtime, slot, "save", e.snapshotFilename(key))
		if err == nil {
			err = e.verifySnapshot(key)
		}
		if err != nil {
			e.logger.Warnf("<%s> extension %s failed to save session %s from slot %d for generation %d: %v", e.modelID, e.name, key, slot, runtime.Generation, err)
		} else {
			e.logger.Infof("<%s> extension %s saved session %s from slot %d for generation %d", e.modelID, e.name, key, slot, runtime.Generation)
		}
		e.mu.Lock()
		if session := e.sessions[key]; session != nil {
			session.Busy = false
			if err == nil {
				session.Snapshot = true
				session.Dirty = false
			}
		}
		if resource := e.resources[slot]; resource != nil {
			resource.Busy = false
		}
		e.broadcastLocked()
		e.mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("save session %s: %w", key, err))
		}
	}
	if err := e.persistMetadata(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// activeCount reads the drain counter after BackendStopping temporarily releases
// mu while waiting for a lease transition.
func (e *Extension) activeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

// BackendStopped invalidates only the generation that actually stopped; a late
// callback cannot erase a replacement runtime.
func (e *Extension) BackendStopped(_ context.Context, runtime extension.BackendRuntime, _ extension.StopResult) {
	e.mu.Lock()
	if e.runtime.Generation == runtime.Generation {
		e.invalidateRuntimeLocked()
	}
	e.mu.Unlock()
}

// BackendExited drops unsafe hot bindings after an unexpected process death
// while leaving the last verified disk snapshots available for lazy restore.
func (e *Extension) BackendExited(_ context.Context, runtime extension.BackendRuntime, _ extension.BackendExit) {
	e.mu.Lock()
	if e.runtime.Generation == runtime.Generation {
		e.invalidateRuntimeLocked()
	}
	e.mu.Unlock()
}

// Close permanently rejects requests and persists model-lifetime metadata after
// all backend generations have stopped.
func (e *Extension) Close(_ context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.accepting = false
	e.broadcastLocked()
	e.mu.Unlock()
	return e.persistMetadata()
}
