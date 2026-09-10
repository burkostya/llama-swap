package extension

import (
	"fmt"
	"sort"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"gopkg.in/yaml.v3"
)

// BuildContext supplies resolved model and host services without requiring an
// extension to reach back into server or process internals.
type BuildContext struct {
	ModelID       string             // ModelID owns the extension instance.
	ExtensionName string             // ExtensionName is the configured instance name.
	Model         config.ModelConfig // Model is the fully decoded model configuration.
	Host          Host               // Host exposes explicit core services.
}

// Host contains the small set of core services available while constructing an
// extension. Keeping this surface explicit limits coupling to upstream code.
type Host struct {
	Logger *logmon.Monitor // Logger writes model-scoped lifecycle diagnostics.
}

// Factory constructs one model-scoped instance for a registered extension
// type. The factory owns decoding and validating its opaque YAML node.
type Factory interface {
	// Type is the stable name referenced by YAML configuration.
	Type() string
	// Build decodes config and constructs one model-scoped Extension.
	Build(BuildContext, yaml.Node) (Extension, error)
}

// Specification declares the versioned capabilities a factory provides and
// requires. Requirements are satisfied only by earlier chain entries.
type Specification struct {
	Type     string   // Type must match Factory.Type when set.
	Provides []string // Provides are versioned capabilities added to the chain.
	Requires []string // Requires must be provided by earlier chain entries.
}

// SpecifiedFactory optionally adds capability metadata to Factory.
type SpecifiedFactory interface {
	Specification() Specification
}

// Registry maps compile-time linked extension types to explicit factories.
// It is safe for concurrent lookup after wiring and rejects duplicate types.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty explicit registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a factory after validating its type and optional specification.
func (r *Registry) Register(factory Factory) error {
	if factory == nil {
		return fmt.Errorf("extension factory is nil")
	}
	typ := factory.Type()
	if typ == "" {
		return fmt.Errorf("extension factory type is empty")
	}
	if sf, ok := factory.(SpecifiedFactory); ok {
		spec := sf.Specification()
		if spec.Type != "" && spec.Type != typ {
			return fmt.Errorf("extension factory type %q does not match specification type %q", typ, spec.Type)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[typ]; exists {
		return fmt.Errorf("extension factory %q already registered", typ)
	}
	r.factories[typ] = factory
	return nil
}

// MustRegister is intended for fixed binary wiring and panics on programmer
// errors such as duplicate or inconsistent factory types.
func (r *Registry) MustRegister(factory Factory) {
	if err := r.Register(factory); err != nil {
		panic(err)
	}
}

// Factory returns the factory registered for typ.
func (r *Registry) Factory(typ string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[typ]
	return factory, ok
}

// Types returns registered type names in deterministic order for diagnostics.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.factories))
	for typ := range r.factories {
		types = append(types, typ)
	}
	sort.Strings(types)
	return types
}

func factorySpecification(factory Factory) Specification {
	if sf, ok := factory.(SpecifiedFactory); ok {
		spec := sf.Specification()
		if spec.Type == "" {
			spec.Type = factory.Type()
		}
		return spec
	}
	return Specification{Type: factory.Type()}
}
