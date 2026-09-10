package llamacppsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

type fakeBackend struct {
	server  *httptest.Server
	dir     string
	mu      sync.Mutex
	actions []string
}

func newFakeBackend(t *testing.T, slots int) *fakeBackend {
	t.Helper()
	fake := &fakeBackend{dir: t.TempDir()}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/slots" {
			response := make([]slotInfo, slots)
			for i := range response {
				response[i].ID = i
			}
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if r.Method != http.MethodPost || len(parts) != 2 || parts[0] != "slots" {
			http.NotFound(w, r)
			return
		}
		slot, err := strconv.Atoi(parts[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		action := r.URL.Query().Get("action")
		fake.mu.Lock()
		fake.actions = append(fake.actions, fmt.Sprintf("%s:%d:%s", action, slot, body["filename"]))
		fake.mu.Unlock()
		switch action {
		case "save":
			if err := os.WriteFile(filepath.Join(fake.dir, body["filename"]), []byte("snapshot"), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "restore":
			if _, err := os.Stat(filepath.Join(fake.dir, body["filename"])); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
		case "erase":
		default:
			http.Error(w, "invalid action", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeBackend) runtime(t *testing.T) extension.BackendRuntime {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return extension.BackendRuntime{ModelID: "model", Generation: 1, BaseURL: u}
}

func (f *fakeBackend) recordedActions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

func buildTestExtension(t *testing.T, fake *fakeBackend) *Extension {
	t.Helper()
	var node yaml.Node
	configYAML := fmt.Sprintf("persistencePath: %q\nqueueTimeout: 2s\nbackendTimeout: 2s\n", fake.dir)
	if err := yaml.Unmarshal([]byte(configYAML), &node); err != nil {
		t.Fatal(err)
	}
	built, err := (Factory{}).Build(extension.BuildContext{
		ModelID: "model", ExtensionName: "kv-session", Host: extension.Host{Logger: logmon.New()},
	}, node)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ext := built.(*Extension)
	if err := ext.BackendReady(t.Context(), fake.runtime(t)); err != nil {
		t.Fatalf("BackendReady: %v", err)
	}
	return ext
}

func TestExtension_LRUSavesAndLazilyRestoresSessions(t *testing.T) {
	fake := newFakeBackend(t, 1)
	ext := buildTestExtension(t, fake)
	var slots []int64
	handler := ext.WrapUpstream(fake.runtime(t), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, gjson.GetBytes(raw, "id_slot").Int())
		if !gjson.GetBytes(raw, "cache_prompt").Bool() {
			t.Error("cache_prompt was not enabled")
		}
	}))

	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	serveSession(t, handler, keyA)
	serveSession(t, handler, keyB)
	serveSession(t, handler, keyA)

	if fmt.Sprint(slots) != "[0 0 0]" {
		t.Fatalf("slots = %v", slots)
	}
	actions := fake.recordedActions()
	want := []string{
		"erase:0:",
		"save:0:session-" + keyA + ".bin",
		"erase:0:",
		"save:0:session-" + keyB + ".bin",
		"restore:0:session-" + keyA + ".bin",
	}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("actions = %#v, want %#v", actions, want)
	}
}

func TestExtension_BackendStoppingDrainsActiveLease(t *testing.T) {
	fake := newFakeBackend(t, 1)
	ext := buildTestExtension(t, fake)
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := ext.WrapUpstream(fake.runtime(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		serveSession(t, handler, strings.Repeat("c", 64))
		close(done)
	}()
	<-entered

	stopDone := make(chan error, 1)
	go func() { stopDone <- ext.BackendStopping(t.Context(), fake.runtime(t)) }()
	select {
	case err := <-stopDone:
		t.Fatalf("BackendStopping returned before lease drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("BackendStopping: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BackendStopping did not finish")
	}
	if _, err := os.Stat(ext.snapshotPath(strings.Repeat("c", 64))); err != nil {
		t.Fatalf("drain did not save session: %v", err)
	}
}

func TestExtension_GracefulRestartSavesAndRestoresPersistentSession(t *testing.T) {
	fake := newFakeBackend(t, 1)
	ext := buildTestExtension(t, fake)
	runtime1 := fake.runtime(t)
	key := strings.Repeat("f", 64)
	handler1 := ext.WrapUpstream(runtime1, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serveSession(t, handler1, key)

	if err := ext.BackendStopping(t.Context(), runtime1); err != nil {
		t.Fatalf("BackendStopping generation 1: %v", err)
	}
	ext.BackendStopped(t.Context(), runtime1, extension.StopResult{})

	runtime2 := fake.runtime(t)
	runtime2.Generation = 2
	if err := ext.BackendReady(t.Context(), runtime2); err != nil {
		t.Fatalf("BackendReady generation 2: %v", err)
	}
	handler2 := ext.WrapUpstream(runtime2, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serveSession(t, handler2, key)

	actions := fake.recordedActions()
	want := []string{
		"erase:0:",
		"save:0:session-" + key + ".bin",
		"restore:0:session-" + key + ".bin",
	}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("actions = %#v, want %#v", actions, want)
	}
}

func TestExtension_SerializesRequestsForSameSession(t *testing.T) {
	fake := newFakeBackend(t, 1)
	ext := buildTestExtension(t, fake)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	var calls atomic.Int32
	handler := ext.WrapUpstream(fake.runtime(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		if calls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		} else {
			close(secondEntered)
		}
	}))
	key := strings.Repeat("d", 64)
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	go func() { serveSession(t, handler, key); close(firstDone) }()
	<-firstEntered
	go func() { serveSession(t, handler, key); close(secondDone) }()
	select {
	case <-secondEntered:
		t.Fatal("second request entered before first completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second request remained blocked")
	}
	<-secondDone
}

func TestExtension_GlobalLimitAndCancelledSessionQueue(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			fake := newFakeBackend(t, 1)
			ext := buildTestExtension(t, fake)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{})
			var calls atomic.Int32
			upstream := ext.WrapUpstream(fake.runtime(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						w.Write([]byte("data: {}\n\n"))
						w.(http.Flusher).Flush()
					}
					close(entered)
					<-r.Context().Done()
				} else {
					w.WriteHeader(http.StatusOK)
				}
			}))
			// Signal entry after admission by upstream's global semaphore.
			queued := make(chan struct{})
			handler := server.CreateConcurrencyLimitMiddleware(2)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Test-Queued") == "true" {
					close(queued)
				}
				upstream.ServeHTTP(w, r)
			}))
			request := func(ctx context.Context) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/json")
				return sessionv1.WithIdentity(r, sessionv1.Identity{Key: strings.Repeat("a", 64), Mode: sessionv1.ModePersistent})
			}
			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				handler.ServeHTTP(httptest.NewRecorder(), request(ctx))
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first request did not reach backend")
			}
			secondDone := make(chan struct{})
			secondResult := httptest.NewRecorder()
			go func() {
				defer close(secondDone)
				r := request(t.Context())
				r.Header.Set("X-Test-Queued", "true")
				handler.ServeHTTP(secondResult, r)
			}()
			select {
			case <-queued:
			case <-time.After(3 * time.Second):
				t.Fatal("second request did not acquire global permit")
			}
			thirdResult := httptest.NewRecorder()
			handler.ServeHTTP(thirdResult, request(t.Context()))
			if thirdResult.Code != http.StatusTooManyRequests || calls.Load() != 1 {
				t.Fatalf("global rejection = %d, backend calls = %d", thirdResult.Code, calls.Load())
			}
			cancel()
			for _, done := range []chan struct{}{firstDone, secondDone} {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled request did not release the session lease")
				}
			}
			if secondResult.Code != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("queued response = %d, backend calls = %d", secondResult.Code, calls.Load())
			}
			finalResult := httptest.NewRecorder()
			handler.ServeHTTP(finalResult, request(t.Context()))
			if finalResult.Code != http.StatusOK || calls.Load() != 3 {
				t.Fatalf("permits were not released: response=%d calls=%d", finalResult.Code, calls.Load())
			}
		})
	}
}

func TestExtension_ControlSuspendAndDelete(t *testing.T) {
	fake := newFakeBackend(t, 1)
	ext := buildTestExtension(t, fake)
	handler := ext.WrapUpstream(fake.runtime(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	key := strings.Repeat("e", 64)
	serveSession(t, handler, key)

	w := httptest.NewRecorder()
	ext.ControlHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions/"+key+"/suspend", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("suspend status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(ext.snapshotPath(key)); err != nil {
		t.Fatalf("suspend snapshot: %v", err)
	}

	w = httptest.NewRecorder()
	ext.ControlHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"resident":false`) {
		t.Fatalf("sessions status=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	ext.ControlHandler().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/sessions/"+key, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(ext.snapshotPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists after delete: %v", err)
	}
}

func serveSession(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/completion", strings.NewReader(`{"prompt":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	r = sessionv1.WithIdentity(r, sessionv1.Identity{Key: key, Namespace: "test", Mode: sessionv1.ModePersistent})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code >= http.StatusBadRequest {
		t.Errorf("status=%d body=%s", w.Code, w.Body.String())
	}
}
