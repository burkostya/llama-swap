// Package sessionv1 defines the backend-independent request contract shared by
// session identity providers and backend-specific session consumers.
//
// The contract deliberately contains no slot, KV-cache, or persistence
// concepts. Producers attach an opaque Identity to a request; consumers use it
// to implement their own affinity or state-management policy.
package sessionv1
