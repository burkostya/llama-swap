package llamacppsession

import (
	"sync"
	"time"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// sessionState is the logical model-lifetime record for one opaque identity.
// Slot and Busy are generation-local; Snapshot and LastUsed may survive a
// backend restart through metadata.
type sessionState struct {
	Key      string
	Mode     sessionv1.Mode
	Slot     int  // -1 means the session has no hot binding.
	Busy     bool // Busy serializes requests, preparation, and control actions.
	Snapshot bool // Snapshot means a validated durable file is available.
	Dirty    bool // Dirty means upstream may have changed KV since the last save.
	LastUsed time.Time
}

// resourceState models one slot in the current backend generation. Session is
// both its hot owner and its reservation while slow backend preparation runs.
type resourceState struct {
	ID      int
	Session string
	Busy    bool
}

// Extension owns logical sessions for one model and transient resources for at
// most one backend generation.
type Extension struct {
	name    string
	modelID string
	config  Config
	logger  *logmon.Monitor
	client  *backendClient

	// mu protects every field below except persistMu. Methods ending in Locked
	// require the caller to hold mu.
	mu sync.Mutex
	// persistMu serializes whole metadata snapshots so an older write cannot
	// replace a newer one after filesystem I/O completes.
	persistMu sync.Mutex
	// changed is closed and replaced under mu to wake resource and drain waiters
	// without dedicating a goroutine or condition variable per session.
	changed   chan struct{}
	accepting bool
	closed    bool
	runtime   extension.BackendRuntime
	sessions  map[string]*sessionState
	resources map[int]*resourceState
	active    int // Active leases, preparations, and control operations.
	waiting   int // Requests admitted to the bounded allocation queue.
}

// Name returns the configured instance name.
func (e *Extension) Name() string { return e.name }

// broadcastLocked wakes current waiters and gives future waiters a fresh
// channel, avoiding missed state transitions while they hold mu.
func (e *Extension) broadcastLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// invalidateRuntimeLocked drops only hot generation bindings. Logical sessions
// and verified snapshots remain available for lazy restore after restart.
func (e *Extension) invalidateRuntimeLocked() {
	e.accepting = false
	for _, session := range e.sessions {
		session.Slot = -1
		session.Busy = false
	}
	e.resources = make(map[int]*resourceState)
	e.runtime = extension.BackendRuntime{}
	e.broadcastLocked()
}
