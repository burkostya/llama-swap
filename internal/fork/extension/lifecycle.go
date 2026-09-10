package extension

import (
	"context"
	"net/http"
)

// Extension is the minimal identity shared by every extension capability.
// One instance belongs to one model chain and survives all backend generations
// observed by that process object.
type Extension interface {
	Name() string
}

// BackendReadyHook initializes generation-local state after the backend health
// check succeeds but before its handler becomes reachable by callers.
type BackendReadyHook interface {
	BackendReady(context.Context, BackendRuntime) error
}

// UpstreamHandlerWrapper adds request behavior around the model's reverse
// proxy. The wrapper remains on the call stack until streaming responses end.
type UpstreamHandlerWrapper interface {
	WrapUpstream(BackendRuntime, http.Handler) http.Handler
}

// BackendStoppingHook drains or persists extension state after new requests
// have been rejected and before the backend receives its stop signal.
type BackendStoppingHook interface {
	BackendStopping(context.Context, BackendRuntime) error
}

// BackendStoppedHook discards transient bindings after an intentional stop or
// a failed startup. It is a cleanup notification, so it cannot veto shutdown.
type BackendStoppedHook interface {
	BackendStopped(context.Context, BackendRuntime, StopResult)
}

// BackendExitedHook invalidates generation-local state after an unexpected
// process exit while allowing logical extension state to survive a restart.
type BackendExitedHook interface {
	BackendExited(context.Context, BackendRuntime, BackendExit)
}

// Closer releases model-lifetime extension state after all backend processes
// have stopped during server shutdown or configuration replacement.
type Closer interface {
	Close(context.Context) error
}

// ControlProvider exposes an extension-owned subtree below the generic
// authenticated /api/extensions namespace.
type ControlProvider interface {
	ControlHandler() http.Handler
}
