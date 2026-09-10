package llamacppsession

import (
	"fmt"
	"os"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"gopkg.in/yaml.v3"
)

// Type is the compile-time registry name for llama.cpp session state.
const Type = "llamacpp.session-state.v1"

// Factory builds model-scoped llama.cpp session managers.
type Factory struct{}

// NewFactory returns a stateless registry factory.
func NewFactory() Factory { return Factory{} }

// Type returns the registered extension type.
func (Factory) Type() string { return Type }

// Specification requires an earlier provider of the backend-independent
// session identity contract.
func (Factory) Specification() extension.Specification {
	return extension.Specification{Type: Type, Requires: []string{sessionv1.Capability}}
}

// Build validates storage, loads logical persistent sessions, and prepares an
// instance that can later attach to successive backend generations.
func (Factory) Build(ctx extension.BuildContext, node yaml.Node) (extension.Extension, error) {
	var cfg Config
	if node.Kind != 0 {
		if err := node.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.PersistencePath, 0o700); err != nil {
		return nil, fmt.Errorf("create persistencePath: %w", err)
	}
	ext := &Extension{
		name:      ctx.ExtensionName,
		modelID:   ctx.ModelID,
		config:    cfg,
		logger:    ctx.Host.Logger,
		client:    newBackendClient(cfg.BackendTimeout.Value()),
		sessions:  make(map[string]*sessionState),
		resources: make(map[int]*resourceState),
		changed:   make(chan struct{}),
	}
	if err := ext.loadMetadata(); err != nil {
		return nil, err
	}
	return ext, nil
}
