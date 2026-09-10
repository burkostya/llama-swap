// Package sessionidentity provides the session.identity.v1 extension.
//
// The extension converts an external session identifier and optional tenant
// identifier into an opaque, namespaced HMAC key. It publishes only the typed
// sessionv1 request contract, keeping backend resource allocation out of the
// identity layer.
package sessionidentity
