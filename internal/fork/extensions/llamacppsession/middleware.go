package llamacppsession

import (
	"net/http"
	"strings"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

// WrapUpstream binds identified JSON inference requests to a prepared slot and
// patches llama.cpp routing fields before invoking the reverse proxy.
func (e *Extension) WrapUpstream(_ extension.BackendRuntime, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := sessionv1.FromRequest(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_request_not_json", Message: "session-managed requests must use an application/json body"})
			return
		}

		lease, requestErr := e.acquire(r.Context(), identity)
		if requestErr != nil {
			extension.WriteRequestError(w, requestErr)
			return
		}
		upstreamCalled := false
		// ServeHTTP returns only after a normal or streaming response finishes, so
		// this defer keeps the session and slot exclusive for the entire SSE flow.
		defer func() { lease.release(upstreamCalled) }()

		body, err := extension.RequestBody(r)
		if err != nil {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_request_body_invalid", Message: err.Error()})
			return
		}
		if err := body.SetJSON("id_slot", lease.slot); err != nil {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_request_body_invalid", Message: "could not set id_slot"})
			return
		}
		if err := body.SetJSON("cache_prompt", true); err != nil {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_request_body_invalid", Message: "could not enable cache_prompt"})
			return
		}
		body.Apply(r)
		// Mark the session dirty only after control is handed to llama.cpp. Local
		// validation failures cannot have changed the backend KV state.
		upstreamCalled = true
		next.ServeHTTP(w, r)
	})
}
