package llamacppsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

type sessionView struct {
	Key      string         `json:"key"`
	Mode     sessionv1.Mode `json:"mode"`
	Resident bool           `json:"resident"`
	Slot     *int           `json:"slot,omitempty"`
	Busy     bool           `json:"busy"`
	Snapshot bool           `json:"snapshot"`
	Dirty    bool           `json:"dirty"`
	LastUsed time.Time      `json:"lastUsed"`
}

type resourceView struct {
	ID         int    `json:"id"`
	SessionKey string `json:"sessionKey,omitempty"`
	Busy       bool   `json:"busy"`
	Generation uint64 `json:"generation"`
}

// ControlHandler exposes session inspection, suspension, and deletion relative
// to this extension's generic control subtree.
func (e *Extension) ControlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodGet && (path == "" || path == "sessions"):
			e.writeSessions(w)
		case r.Method == http.MethodGet && path == "resources":
			e.writeResources(w)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "sessions/") && strings.HasSuffix(path, "/suspend"):
			key := strings.TrimSuffix(strings.TrimPrefix(path, "sessions/"), "/suspend")
			if err := e.suspend(r.Context(), key); err != nil {
				e.writeControlError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "sessions/"):
			key := strings.TrimPrefix(path, "sessions/")
			if err := e.deleteSession(r.Context(), key); err != nil {
				e.writeControlError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (e *Extension) writeSessions(w http.ResponseWriter) {
	e.mu.Lock()
	// Copy under the state lock and encode afterwards so a slow client cannot
	// block request allocation or lease release.
	views := make([]sessionView, 0, len(e.sessions))
	for _, session := range e.sessions {
		view := sessionView{
			Key: session.Key, Mode: session.Mode, Resident: session.Slot >= 0,
			Busy: session.Busy, Snapshot: session.Snapshot, Dirty: session.Dirty, LastUsed: session.LastUsed,
		}
		if session.Slot >= 0 {
			slot := session.Slot
			view.Slot = &slot
		}
		views = append(views, view)
	}
	e.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	writeJSON(w, map[string]any{"sessions": views})
}

func (e *Extension) writeResources(w http.ResponseWriter) {
	e.mu.Lock()
	// As with sessions, expose a point-in-time control-plane view without holding
	// mu during JSON encoding.
	views := make([]resourceView, 0, len(e.resources))
	for _, resource := range e.resources {
		views = append(views, resourceView{ID: resource.ID, SessionKey: resource.Session, Busy: resource.Busy, Generation: e.runtime.Generation})
	}
	e.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	writeJSON(w, map[string]any{"resources": views})
}

type controlError struct {
	status int
	code   string
	err    error
}

// suspend makes a resident session cold without deleting its logical record.
// Dirty persistent state is saved before the slot is erased; ephemeral or clean
// sessions can simply release their hot binding.
func (e *Extension) suspend(ctx context.Context, key string) error {
	if !sessionKeyPattern.MatchString(key) {
		return controlError{status: http.StatusBadRequest, code: "invalid_session_key", err: fmt.Errorf("invalid session key")}
	}
	e.mu.Lock()
	session := e.sessions[key]
	if session == nil {
		e.mu.Unlock()
		return controlError{status: http.StatusNotFound, code: "session_not_found", err: fmt.Errorf("session not found")}
	}
	if session.Busy {
		e.mu.Unlock()
		return controlError{status: http.StatusConflict, code: "session_busy", err: fmt.Errorf("session is busy")}
	}
	if session.Slot < 0 {
		e.mu.Unlock()
		return nil
	}
	if !e.accepting || e.runtime.Generation == 0 {
		e.mu.Unlock()
		return controlError{status: http.StatusServiceUnavailable, code: "backend_session_unavailable", err: fmt.Errorf("backend is unavailable")}
	}
	slot := session.Slot
	runtime := e.runtime
	dirty := session.Dirty
	mode := session.Mode
	// Count control work as active so backend shutdown drains it before saving or
	// killing the process, just like an inference lease.
	session.Busy = true
	e.active++
	resource := e.resources[slot]
	if resource != nil {
		resource.Busy = true
	}
	e.mu.Unlock()
	defer e.finishControlOperation(key)

	// Backend I/O runs outside mu while Busy reservations exclude request and
	// allocator access to both the logical session and its slot.
	saved := false
	var err error
	if mode == sessionv1.ModePersistent && dirty {
		err = e.client.action(ctx, runtime, slot, "save", e.snapshotFilename(key))
		if err == nil {
			err = e.verifySnapshot(key)
			saved = err == nil
		}
	}
	if err == nil {
		err = e.client.action(ctx, runtime, slot, "erase", "")
	}

	e.mu.Lock()
	session = e.sessions[key]
	if session != nil {
		session.Busy = false
		if saved {
			session.Snapshot = true
			session.Dirty = false
		}
		if err == nil {
			session.Slot = -1
		}
	}
	if resource := e.resources[slot]; resource != nil {
		resource.Busy = false
		if err == nil {
			resource.Session = ""
		}
	}
	e.broadcastLocked()
	e.mu.Unlock()
	if err != nil {
		return controlError{status: http.StatusBadGateway, code: "backend_session_operation_failed", err: err}
	}
	if saved {
		return e.persistMetadata()
	}
	return nil
}

// deleteSession removes hot backend state first, then its durable snapshot and
// logical metadata. A backend erase failure leaves the session recoverable
// instead of claiming deletion while KV is still resident.
func (e *Extension) deleteSession(ctx context.Context, key string) error {
	if !sessionKeyPattern.MatchString(key) {
		return controlError{status: http.StatusBadRequest, code: "invalid_session_key", err: fmt.Errorf("invalid session key")}
	}
	e.mu.Lock()
	session := e.sessions[key]
	if session == nil {
		e.mu.Unlock()
		return controlError{status: http.StatusNotFound, code: "session_not_found", err: fmt.Errorf("session not found")}
	}
	if session.Busy {
		e.mu.Unlock()
		return controlError{status: http.StatusConflict, code: "session_busy", err: fmt.Errorf("session is busy")}
	}
	slot := session.Slot
	runtime := e.runtime
	if slot >= 0 {
		if !e.accepting || runtime.Generation == 0 {
			e.mu.Unlock()
			return controlError{status: http.StatusServiceUnavailable, code: "backend_session_unavailable", err: fmt.Errorf("backend is unavailable")}
		}
		if resource := e.resources[slot]; resource != nil {
			resource.Busy = true
		}
	}
	session.Busy = true
	e.active++
	e.mu.Unlock()
	defer e.finishControlOperation(key)

	if slot >= 0 {
		// The slot must be erased before unlinking local state; otherwise an
		// untracked KV cache could be assigned to a different logical session.
		if err := e.client.action(ctx, runtime, slot, "erase", ""); err != nil {
			e.mu.Lock()
			if session := e.sessions[key]; session != nil {
				session.Busy = false
			}
			if resource := e.resources[slot]; resource != nil {
				resource.Busy = false
			}
			e.broadcastLocked()
			e.mu.Unlock()
			return controlError{status: http.StatusBadGateway, code: "backend_session_operation_failed", err: err}
		}
		e.mu.Lock()
		if session := e.sessions[key]; session != nil {
			session.Slot = -1
			session.Busy = false
			session.Dirty = false
		}
		if resource := e.resources[slot]; resource != nil && resource.Session == key {
			resource.Session = ""
			resource.Busy = false
		}
		e.broadcastLocked()
		e.mu.Unlock()
	}

	if err := os.Remove(e.snapshotPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return controlError{status: http.StatusInternalServerError, code: "session_snapshot_delete_failed", err: err}
	}
	e.mu.Lock()
	delete(e.sessions, key)
	e.broadcastLocked()
	e.mu.Unlock()
	return e.persistMetadata()
}

// finishControlOperation balances the active drain counter on every control
// return path and wakes requests that may now acquire a session or slot.
func (e *Extension) finishControlOperation(key string) {
	e.mu.Lock()
	if session := e.sessions[key]; session != nil {
		session.Busy = false
	}
	if e.active > 0 {
		e.active--
	}
	e.broadcastLocked()
	e.mu.Unlock()
}

func (e *Extension) writeControlError(w http.ResponseWriter, err error) {
	var controlled controlError
	if errors.As(err, &controlled) {
		extension.WriteRequestError(w, &extension.RequestError{Status: controlled.status, Code: controlled.code, Message: controlled.err.Error()})
		return
	}
	extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusInternalServerError, Code: "session_control_failed", Message: err.Error()})
}

func (e controlError) Error() string { return e.err.Error() }
func (e controlError) Unwrap() error { return e.err }

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
