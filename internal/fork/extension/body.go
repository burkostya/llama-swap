package extension

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/tidwall/sjson"
)

type bodyBufferKey struct{}

// BodyBuffer is a request-scoped, lazily shared JSON body. It prevents multiple
// wrappers from independently consuming r.Body while still allowing the final
// reverse proxy to observe all mutations. Wrappers use it sequentially on one
// request; it is not intended for concurrent mutation.
type BodyBuffer struct {
	raw   []byte
	dirty bool
}

// RequestBody lazily reads a request body and shares the buffer through the
// request context with every extension in the chain.
func RequestBody(r *http.Request) (*BodyBuffer, error) {
	if body, ok := r.Context().Value(bodyBufferKey{}).(*BodyBuffer); ok {
		return body, nil
	}
	if r.Body == nil {
		return nil, fmt.Errorf("request body is empty")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	body := &BodyBuffer{raw: raw}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	*r = *r.WithContext(context.WithValue(r.Context(), bodyBufferKey{}, body))
	return body, nil
}

// Bytes returns the current buffered representation. The returned slice aliases
// the request-scoped buffer and must be treated as read-only.
func (b *BodyBuffer) Bytes() []byte { return b.raw }

// SetJSON assigns value at an sjson path and marks the request for reapplication.
func (b *BodyBuffer) SetJSON(path string, value any) error {
	raw, err := sjson.SetBytes(b.raw, path, value)
	if err != nil {
		return err
	}
	b.raw = raw
	b.dirty = true
	return nil
}

// DeleteJSON removes an sjson path and marks the request for reapplication.
func (b *BodyBuffer) DeleteJSON(path string) error {
	raw, err := sjson.DeleteBytes(b.raw, path)
	if err != nil {
		return err
	}
	b.raw = raw
	b.dirty = true
	return nil
}

// Apply replaces the request body and length metadata after pending mutations.
// Calling Apply repeatedly is safe and becomes a no-op once changes are applied.
func (b *BodyBuffer) Apply(r *http.Request) {
	if !b.dirty {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b.raw))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b.raw)), nil
	}
	r.ContentLength = int64(len(b.raw))
	r.Header.Del("Transfer-Encoding")
	r.Header.Set("Content-Length", strconv.Itoa(len(b.raw)))
	b.dirty = false
}
