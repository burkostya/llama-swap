// Package llamacppsession provides the llamacpp.session-state.v1 extension.
//
// The extension maps logical session identities to generation-local llama.cpp
// slots, serializes requests per session, and preserves persistent sessions as
// slot snapshots. Logical sessions outlive backend generations, while hot slot
// bindings are discarded whenever a backend stops or exits.
package llamacppsession
