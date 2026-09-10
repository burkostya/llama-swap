package sessionv1

import (
	"context"
	"net/http"
)

// Capability is the versioned dependency name advertised by identity
// providers and required by session consumers.
const Capability = "io.llama-swap.session.identity/v1"

type identityKey struct{}

// WithIdentity returns a request carrying identity for downstream extensions.
func WithIdentity(r *http.Request, identity Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, identity))
}

// FromRequest retrieves the identity published by an earlier request wrapper.
func FromRequest(r *http.Request) (Identity, bool) {
	identity, ok := r.Context().Value(identityKey{}).(Identity)
	return identity, ok
}
