package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
)

func TestServer_ExtensionControlAuthenticationAndPlainConstruction(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "plain"
		if enabled {
			name = "extensions"
		}
		t.Run(name, func(t *testing.T) {
			cfg, err := config.LoadConfigFromReader(strings.NewReader("models: {}\napiKeys: [test-key]\n"))
			if err != nil {
				t.Fatal(err)
			}
			st, err := sqlite.New("")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			logs := logmon.NewGroup(io.Discard, true, true, true)
			var opts []Option
			if enabled {
				opts = append(opts, WithExtensionRegistry(extension.NewRegistry()))
			}
			srv, err := New(cfg, logs, nil, st, BuildInfo{}, nil, nil, nil, nil, opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := srv.Shutdown(time.Second); err != nil {
					t.Error(err)
				}
			})
			if srv.tools == nil {
				t.Fatal("upstream MCP tool registry was lost")
			}
			for _, path := range []string{"/api/extensions", "/api/extensions/model/instance/sessions"} {
				for _, authorized := range []bool{false, true} {
					r := httptest.NewRequest(http.MethodGet, path, nil)
					if authorized {
						r.Header.Set("Authorization", "Bearer test-key")
					}
					w := httptest.NewRecorder()
					srv.ServeHTTP(w, r)
					want := http.StatusNotFound
					if enabled {
						if !authorized {
							want = http.StatusUnauthorized
						} else if path == "/api/extensions" {
							want = http.StatusOK
						}
					}
					if w.Code != want {
						t.Fatalf("%s authorized=%t status=%d want=%d body=%s", path, authorized, w.Code, want, w.Body.String())
					}
				}
			}
		})
	}
}
