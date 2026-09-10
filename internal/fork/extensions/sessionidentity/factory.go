package sessionidentity

import (
	"fmt"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"gopkg.in/yaml.v3"
)

// Type is the compile-time registry name for this extension contract.
const Type = "session.identity.v1"

// Factory builds session identity extensions from opaque YAML configuration.
type Factory struct{}

// NewFactory returns a stateless registry factory.
func NewFactory() Factory { return Factory{} }

// Type returns the registered extension type.
func (Factory) Type() string { return Type }

// Specification advertises the typed identity capability consumed by backend-
// specific session extensions.
func (Factory) Specification() extension.Specification {
	return extension.Specification{Type: Type, Provides: []string{sessionv1.Capability}}
}

// Build validates identity policy once and returns a model-scoped wrapper.
func (Factory) Build(ctx extension.BuildContext, node yaml.Node) (extension.Extension, error) {
	var cfg Config
	if node.Kind != 0 {
		if err := node.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	cfg.defaults(ctx.ModelID)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Extension{name: ctx.ExtensionName, config: cfg}, nil
}
