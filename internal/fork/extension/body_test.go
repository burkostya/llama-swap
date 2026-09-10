package extension

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBodyBuffer_SharedAndApplied(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"m"}`))
	first, err := RequestBody(r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RequestBody(r)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("RequestBody returned different buffers")
	}
	if err := first.SetJSON("id_slot", 3); err != nil {
		t.Fatal(err)
	}
	first.Apply(r)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(raw, "id_slot").Int(); got != 3 {
		t.Fatalf("id_slot = %d", got)
	}
	if r.ContentLength != int64(len(raw)) {
		t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(raw))
	}
}
