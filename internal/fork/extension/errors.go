package extension

import (
	"encoding/json"
	"net/http"
)

// RequestError is the stable HTTP error shape returned by request wrappers and
// extension control handlers.
type RequestError struct {
	Status  int    // Status defaults to 500 when unset.
	Code    string // Code is the stable machine-readable error identifier.
	Message string // Message is safe to return to the caller.
}

// Error returns the client-facing message.
func (e *RequestError) Error() string { return e.Message }

// WriteRequestError serializes an extension error without exposing internal
// implementation details or backend response bodies.
func WriteRequestError(w http.ResponseWriter, err *RequestError) {
	status := err.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    err.Code,
			"message": err.Message,
		},
	})
}
