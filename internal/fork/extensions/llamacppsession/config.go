package llamacppsession

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Config controls queueing and durable llama.cpp slot snapshots. The
// persistence path must be shared with llama-server's --slot-save-path because
// the backend writes snapshot files while this extension validates them.
type Config struct {
	PersistencePath string          `yaml:"persistencePath"` // PersistencePath is shared with llama-server.
	Eviction        string          `yaml:"eviction"`        // Eviction is "lru" in the v1 contract.
	QueueTimeout    config.Duration `yaml:"queueTimeout"`    // QueueTimeout bounds resource acquisition.
	BackendTimeout  config.Duration `yaml:"backendTimeout"`  // BackendTimeout bounds one slot-control call.
	MaxQueue        int             `yaml:"maxQueue"`        // MaxQueue limits admitted waiting requests.
	MetadataFile    string          `yaml:"metadataFile"`    // MetadataFile stores logical persistent sessions.
}

func (c *Config) defaults() {
	if c.Eviction == "" {
		c.Eviction = "lru"
	}
	if c.QueueTimeout.Value() == 0 {
		c.QueueTimeout = config.Duration(30 * time.Second)
	}
	if c.BackendTimeout.Value() == 0 {
		c.BackendTimeout = config.Duration(30 * time.Second)
	}
	if c.MaxQueue == 0 {
		c.MaxQueue = 128
	}
	if c.MetadataFile == "" {
		c.MetadataFile = ".llama-swap-sessions.json"
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.PersistencePath) == "" {
		return fmt.Errorf("persistencePath is required and must match llama-server --slot-save-path")
	}
	abs, err := filepath.Abs(c.PersistencePath)
	if err != nil {
		return fmt.Errorf("resolve persistencePath: %w", err)
	}
	c.PersistencePath = abs
	if c.Eviction != "lru" {
		return fmt.Errorf("eviction must be lru")
	}
	if c.MaxQueue < 0 {
		return fmt.Errorf("maxQueue must not be negative")
	}
	if filepath.Base(c.MetadataFile) != c.MetadataFile || c.MetadataFile == "." {
		return fmt.Errorf("metadataFile must be a filename without path separators")
	}
	return nil
}
