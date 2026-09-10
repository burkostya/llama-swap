package extension

import "net/url"

// BackendRuntime identifies one concrete lifetime of a model backend.
type BackendRuntime struct {
	ModelID    string   // ModelID identifies the process owner.
	Generation uint64   // Generation distinguishes repeated process lifetimes.
	BaseURL    *url.URL // BaseURL addresses this concrete backend process.
}

// StopResult describes how an intentional process stop completed. Extensions
// use it for cleanup and observability, not to retry or veto the stop.
type StopResult struct {
	Forced bool  // Forced reports whether graceful termination exhausted its budget.
	Err    error // Err contains the stop-hook failure, when one occurred.
}

// BackendExit describes a process that ended without passing through the
// intentional stop lifecycle.
type BackendExit struct {
	ExitCode int   // ExitCode is -1 when no process status is available.
	Err      error // Err describes why the exit is considered unexpected.
}
