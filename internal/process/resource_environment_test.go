//go:build linux

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestProcess_ResourceEnvironmentHelper(t *testing.T) {
	if os.Getenv("LLAMA_SWAP_ENV_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	values := map[string]string{}
	for _, key := range config.ResourceReservedEnvironment {
		values[key] = os.Getenv(key)
	}
	data, _ := json.Marshal(values)
	if err := os.WriteFile(os.Getenv("LLAMA_SWAP_ENV_FILE"), data, 0600); err != nil {
		os.Exit(2)
	}
	<-signals
	os.Exit(0)
}

func TestProcess_ResourceEnvironmentOverridesInheritedValues(t *testing.T) {
	for _, key := range config.ResourceReservedEnvironment {
		t.Setenv(key, "forged-inherited")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "environment.json")
	owned := []string{}
	for _, key := range config.ResourceReservedEnvironment {
		owned = append(owned, key+"=owned-"+key)
	}
	p := newProcessCommand(t, config.ModelConfig{Cmd: fmt.Sprintf("%q -test.run=^TestProcess_ResourceEnvironmentHelper$", exe), Proxy: "http://localhost:5800", CheckEndpoint: "none", ResourceManaged: true, ResourceEnvironment: owned, Env: []string{"LLAMA_SWAP_ENV_HELPER=1", "LLAMA_SWAP_ENV_FILE=" + file, "LLAMA_SWAP_RESOURCE_MODEL=forged-config"}, Drain: config.DrainConfig{Signal: "SIGTERM", Timeout: time.Second}})
	t.Cleanup(func() { _ = p.Stop(time.Second) })
	if err := p.EnsureReady(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(file)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	for _, key := range config.ResourceReservedEnvironment {
		if values[key] != "owned-"+key {
			t.Fatalf("reserved env %s was forged: %s", key, values[key])
		}
	}
}
