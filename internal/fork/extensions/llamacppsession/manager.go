package llamacppsession

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

// lease keeps one logical session and one generation-local slot exclusive until
// the complete upstream handler, including any streaming response, returns.
type lease struct {
	extension *Extension
	key       string
	slot      int
	once      sync.Once
}

// release is idempotent because both normal completion and recovery paths may
// converge here. upstreamCalled distinguishes backend work from local failures
// that cannot have dirtied KV state.
func (l *lease) release(upstreamCalled bool) {
	l.once.Do(func() {
		e := l.extension
		e.mu.Lock()
		if session := e.sessions[l.key]; session != nil {
			session.Busy = false
			if upstreamCalled {
				session.Dirty = true
				session.LastUsed = time.Now().UTC()
			}
		}
		if resource := e.resources[l.slot]; resource != nil && resource.Session == l.key {
			resource.Busy = false
		}
		if e.active > 0 {
			e.active--
		}
		e.broadcastLocked()
		e.mu.Unlock()
	})
}

// allocation freezes a reservation made under mu before slow save, restore, or
// erase calls run without the lock.
type allocation struct {
	slot int

	victimKey      string
	victimMode     sessionv1.Mode
	victimDirty    bool
	victimSnapshot bool

	targetKey      string
	targetSnapshot bool
}

// prepareResult tells the commit phase whether backend state was modified and
// therefore whether the old in-memory slot binding is still trustworthy.
type prepareResult struct {
	modified    bool
	victimSaved bool
	err         error
}

// acquire serializes one logical session, reserves a slot, and prepares its KV
// state before returning a lease. All queueing shares the request deadline; no
// backend I/O occurs while the state mutex is held.
func (e *Extension) acquire(parent context.Context, identity sessionv1.Identity) (*lease, *extension.RequestError) {
	if !sessionKeyPattern.MatchString(identity.Key) {
		return nil, &extension.RequestError{Status: http.StatusBadRequest, Code: "invalid_session_key", Message: "session identity key must be 64 lowercase hexadecimal characters"}
	}
	if identity.Mode != sessionv1.ModePersistent && identity.Mode != sessionv1.ModeEphemeral {
		return nil, &extension.RequestError{Status: http.StatusBadRequest, Code: "invalid_session_mode", Message: "session identity mode is invalid"}
	}
	ctx, cancel := context.WithTimeout(parent, e.config.QueueTimeout.Value())
	defer cancel()

	// Queue admission is counted separately from active leases so callers can be
	// rejected before unbounded contention consumes memory.
	e.mu.Lock()
	if !e.accepting || e.closed {
		e.mu.Unlock()
		return nil, unavailableError("backend session manager is not accepting requests")
	}
	if e.config.MaxQueue > 0 && e.waiting >= e.config.MaxQueue {
		e.mu.Unlock()
		return nil, &extension.RequestError{Status: http.StatusTooManyRequests, Code: "session_queue_full", Message: "session request queue is full"}
	}
	e.waiting++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.waiting--
		e.mu.Unlock()
	}()

	for {
		e.mu.Lock()
		if !e.accepting || e.closed {
			e.mu.Unlock()
			return nil, unavailableError("backend session manager stopped while waiting")
		}
		// Logical sessions are created lazily. A snapshot discovered on disk is
		// not restored until this session actually wins a slot.
		session := e.sessions[identity.Key]
		if session == nil {
			snapshot := false
			if identity.Mode == sessionv1.ModePersistent {
				if _, err := os.Stat(e.snapshotPath(identity.Key)); err == nil {
					snapshot = true
				}
			}
			session = &sessionState{Key: identity.Key, Mode: identity.Mode, Slot: -1, Snapshot: snapshot, LastUsed: time.Now().UTC()}
			e.sessions[identity.Key] = session
		}
		if session.Mode != identity.Mode {
			e.mu.Unlock()
			return nil, &extension.RequestError{Status: http.StatusConflict, Code: "session_mode_conflict", Message: "session mode changed for an existing session"}
		}

		// The hot path reuses a consistent resident binding without backend
		// preparation, while still enforcing one in-flight request per session.
		if !session.Busy && session.Slot >= 0 {
			resource := e.resources[session.Slot]
			if resource != nil && resource.Session == session.Key && !resource.Busy {
				session.Busy = true
				resource.Busy = true
				e.active++
				lease := &lease{extension: e, key: session.Key, slot: session.Slot}
				e.mu.Unlock()
				return lease, nil
			}
			session.Slot = -1
		}

		if !session.Busy {
			resource, victim := e.selectResourceLocked()
			if resource != nil {
				allocation := allocation{
					slot:           resource.ID,
					targetKey:      session.Key,
					targetSnapshot: session.Snapshot,
				}
				if victim != nil {
					allocation.victimKey = victim.Key
					allocation.victimMode = victim.Mode
					allocation.victimDirty = victim.Dirty
					allocation.victimSnapshot = victim.Snapshot
					victim.Busy = true
				}
				// Reserve target, victim, and slot atomically before releasing mu.
				// Other requests then wait instead of observing a half-prepared slot.
				session.Busy = true
				session.Slot = resource.ID
				resource.Busy = true
				resource.Session = session.Key
				e.active++
				runtime := e.runtime
				e.mu.Unlock()

				// Save/restore can block on llama.cpp and disk. The reservation protects
				// ownership while I/O runs without blocking unrelated resident sessions.
				prepared := e.prepareSlot(ctx, runtime, allocation)
				e.finishPreparation(allocation, prepared)
				if prepared.err != nil {
					return nil, unavailableError(prepared.err.Error())
				}
				if prepared.victimSaved {
					if err := e.persistMetadata(); err != nil {
						e.logger.Warnf("<%s> persist session metadata after eviction: %v", e.modelID, err)
					}
				}
				return &lease{extension: e, key: session.Key, slot: resource.ID}, nil
			}
		}

		// Capture changed while holding mu. A state transition either closes this
		// exact channel afterwards or is already visible before the next loop.
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, &extension.RequestError{Status: http.StatusServiceUnavailable, Code: "session_queue_timeout", Message: "timed out waiting for a backend session resource"}
		case <-changed:
		}
	}
}

// selectResourceLocked prefers an unused slot, then deterministically evicts
// the least recently used idle session. Slot ID breaks equal-time ties so tests
// and operational behavior do not depend on map iteration order.
func (e *Extension) selectResourceLocked() (*resourceState, *sessionState) {
	ids := make([]int, 0, len(e.resources))
	for id := range e.resources {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		resource := e.resources[id]
		if resource.Session == "" && !resource.Busy {
			return resource, nil
		}
	}

	var selected *resourceState
	var victim *sessionState
	for _, id := range ids {
		resource := e.resources[id]
		if resource.Busy || resource.Session == "" {
			continue
		}
		session := e.sessions[resource.Session]
		if session == nil || session.Busy {
			continue
		}
		if victim == nil || session.LastUsed.Before(victim.LastUsed) || (session.LastUsed.Equal(victim.LastUsed) && id < selected.ID) {
			selected = resource
			victim = session
		}
	}
	return selected, victim
}

// prepareSlot preserves a dirty persistent victim before assigning its slot to
// the target. Once target preparation begins, failures conservatively invalidate
// the old hot binding rather than assuming the backend slot is unchanged.
func (e *Extension) prepareSlot(ctx context.Context, runtime extension.BackendRuntime, allocation allocation) prepareResult {
	result := prepareResult{}
	if allocation.victimKey != "" && allocation.victimMode == sessionv1.ModePersistent && allocation.victimDirty {
		filename := e.snapshotFilename(allocation.victimKey)
		e.logger.Infof("<%s> extension %s saving evicted session %s from slot %d for generation %d", e.modelID, e.name, allocation.victimKey, allocation.slot, runtime.Generation)
		if err := e.client.action(ctx, runtime, allocation.slot, "save", filename); err != nil {
			e.logger.Warnf("<%s> extension %s failed to save evicted session %s from slot %d for generation %d: %v", e.modelID, e.name, allocation.victimKey, allocation.slot, runtime.Generation, err)
			result.err = fmt.Errorf("save evicted session: %w", err)
			return result
		}
		if err := e.verifySnapshot(allocation.victimKey); err != nil {
			e.logger.Warnf("<%s> extension %s failed to verify evicted session %s from slot %d for generation %d: %v", e.modelID, e.name, allocation.victimKey, allocation.slot, runtime.Generation, err)
			result.err = err
			return result
		}
		e.logger.Infof("<%s> extension %s saved evicted session %s from slot %d for generation %d", e.modelID, e.name, allocation.victimKey, allocation.slot, runtime.Generation)
		result.victimSaved = true
	}

	// The commit phase may restore the victim only if preparation failed before
	// this boundary. Restore/erase errors do not describe the backend's exact KV
	// contents, so the slot must become unowned.
	result.modified = true
	if allocation.targetSnapshot {
		if err := e.verifySnapshot(allocation.targetKey); err != nil {
			e.logger.Warnf("<%s> extension %s failed to verify session %s before restore into slot %d for generation %d: %v", e.modelID, e.name, allocation.targetKey, allocation.slot, runtime.Generation, err)
			result.err = err
			return result
		}
		e.logger.Infof("<%s> extension %s restoring session %s into slot %d for generation %d", e.modelID, e.name, allocation.targetKey, allocation.slot, runtime.Generation)
		if err := e.client.action(ctx, runtime, allocation.slot, "restore", e.snapshotFilename(allocation.targetKey)); err != nil {
			e.logger.Warnf("<%s> extension %s failed to restore session %s into slot %d for generation %d: %v", e.modelID, e.name, allocation.targetKey, allocation.slot, runtime.Generation, err)
			result.err = fmt.Errorf("restore session: %w", err)
		} else {
			e.logger.Infof("<%s> extension %s restored session %s into slot %d for generation %d", e.modelID, e.name, allocation.targetKey, allocation.slot, runtime.Generation)
		}
		return result
	}
	if err := e.client.action(ctx, runtime, allocation.slot, "erase", ""); err != nil {
		result.err = fmt.Errorf("erase slot before assignment: %w", err)
	}
	return result
}

// finishPreparation commits the reserved mapping or rolls it back under mu.
// Durable victim-save results are retained even when target preparation fails.
func (e *Extension) finishPreparation(allocation allocation, prepared prepareResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	target := e.sessions[allocation.targetKey]
	resource := e.resources[allocation.slot]
	var victim *sessionState
	if allocation.victimKey != "" {
		victim = e.sessions[allocation.victimKey]
	}
	if prepared.victimSaved && victim != nil {
		victim.Snapshot = true
		victim.Dirty = false
	}

	if prepared.err == nil {
		// Target ownership was installed during reservation; success only needs to
		// retire the previous logical binding and wake waiters.
		if victim != nil {
			victim.Slot = -1
			victim.Busy = false
		}
		e.broadcastLocked()
		return
	}

	// Failed preparation cannot return a lease. Undo its active reservation and
	// restore the victim only when no backend mutation could have occurred.
	if target != nil {
		target.Slot = -1
		target.Busy = false
	}
	if e.active > 0 {
		e.active--
	}
	if resource != nil {
		resource.Busy = false
		if victim != nil && !prepared.modified {
			resource.Session = victim.Key
			victim.Slot = resource.ID
			victim.Busy = false
		} else {
			resource.Session = ""
			if victim != nil {
				victim.Slot = -1
				victim.Busy = false
			}
		}
	}
	e.broadcastLocked()
}

func unavailableError(message string) *extension.RequestError {
	return &extension.RequestError{Status: http.StatusServiceUnavailable, Code: "backend_session_unavailable", Message: message}
}
