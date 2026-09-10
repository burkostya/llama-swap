package extension

import (
	"context"
	"net/http"
	"runtime/debug"
)

// responseStateWriter records whether headers or body bytes have reached the
// client, which determines whether panic recovery may still emit a JSON error.
type responseStateWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *responseStateWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStateWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

func (w *responseStateWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseStateWriter) Flush() {
	w.wrote = true
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// recoverHandler isolates one extension wrapper while preserving deferred
// cleanup in outer wrappers. After a streaming response starts, it can log and
// cancel context but must not append a second response format.
func (c *Chain) recoverHandler(entry Entry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &responseStateWriter{ResponseWriter: w}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				c.logger.Errorf("<%s> extension %s request panic: %v\n%s", c.modelID, entry.Name, recovered, debug.Stack())
				// Once an HTTP or SSE response has started, a second structured error
				// would corrupt the stream. Cancellation and deferred middleware cleanup
				// are the only safe recovery actions at that point.
				if !state.wrote {
					WriteRequestError(state, &RequestError{
						Status:  http.StatusInternalServerError,
						Code:    "extension_panic",
						Message: "extension request handler failed",
					})
				}
			}
		}()
		next.ServeHTTP(state, r.WithContext(ctx))
	})
}
