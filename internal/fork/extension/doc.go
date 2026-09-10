// Package extension provides the compile-time extension framework used by the
// fork to attach model-specific behavior to llama-swap without teaching core
// process and routing code about backend-specific concepts.
//
// Extensions are built once per configured model and may observe multiple
// backend generations. The chain runs startup hooks in configuration order,
// wraps HTTP handlers so the first extension is outermost, and unwinds stop and
// close hooks in reverse order. Synchronous hooks are used for correctness
// boundaries such as save-before-stop; the asynchronous event bus remains an
// observability mechanism only.
package extension
