package extension

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Manager owns all model chains for one immutable server configuration. A hot
// reload builds a replacement manager before the active server is swapped.
type Manager struct {
	chains map[string]*Chain
	logger *logmon.Monitor

	closeOnce sync.Once
	closeErr  error
}

// NewManager validates and constructs every model chain transactionally. If
// any model fails, instances already built for earlier models are closed.
func NewManager(cfg config.Config, registry *Registry, logger *logmon.Monitor) (*Manager, error) {
	if registry == nil {
		return nil, fmt.Errorf("extension registry is nil")
	}
	m := &Manager{chains: make(map[string]*Chain, len(cfg.Models)), logger: logger}
	modelIDs := make([]string, 0, len(cfg.Models))
	for modelID := range cfg.Models {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)

	for _, modelID := range modelIDs {
		modelCfg := cfg.Models[modelID]
		chain, err := buildModelChain(modelID, modelCfg, registry, logger)
		if err != nil {
			_ = m.Close(context.Background())
			return nil, fmt.Errorf("model %s extensions: %w", modelID, err)
		}
		m.chains[modelID] = chain
	}
	return m, nil
}

// buildModelChain validates entries from left to right because dependency order
// also defines middleware nesting and lifecycle order.
func buildModelChain(modelID string, modelCfg config.ModelConfig, registry *Registry, logger *logmon.Monitor) (chain *Chain, buildErr error) {
	entries := make([]Entry, 0, len(modelCfg.Extensions))
	// Factory construction may allocate resources. Treat a partially built
	// chain as a transaction and close it before returning the validation error.
	defer func() {
		if buildErr != nil && len(entries) > 0 {
			_ = newChain(modelID, entries, logger).Close(context.Background())
		}
	}()
	provided := make(map[string]string)
	names := make(map[string]struct{})

	for index, extensionCfg := range modelCfg.Extensions {
		if extensionCfg.Enabled != nil && !*extensionCfg.Enabled {
			continue
		}
		if strings.TrimSpace(extensionCfg.Name) == "" {
			return nil, fmt.Errorf("entry %d has an empty name", index)
		}
		if strings.Contains(extensionCfg.Name, "/") {
			return nil, fmt.Errorf("extension name %q must not contain /", extensionCfg.Name)
		}
		if _, duplicate := names[extensionCfg.Name]; duplicate {
			return nil, fmt.Errorf("duplicate extension name %q", extensionCfg.Name)
		}
		names[extensionCfg.Name] = struct{}{}

		factory, ok := registry.Factory(extensionCfg.Type)
		if !ok {
			return nil, fmt.Errorf("extension %s uses unknown type %q (registered: %s)", extensionCfg.Name, extensionCfg.Type, strings.Join(registry.Types(), ", "))
		}
		spec := factorySpecification(factory)
		requires := append(append([]string{}, spec.Requires...), extensionCfg.Requires...)
		for _, requirement := range requires {
			if _, ok := provided[requirement]; !ok {
				return nil, fmt.Errorf("extension %s requires %q from an earlier provider", extensionCfg.Name, requirement)
			}
		}
		if previous, duplicate := provided[extensionCfg.Type]; duplicate {
			return nil, fmt.Errorf("extension %s type %q conflicts with earlier instance %s", extensionCfg.Name, extensionCfg.Type, previous)
		}
		policy := FailurePolicy(strings.TrimSpace(extensionCfg.FailurePolicy))
		if policy == "" {
			policy = FailureFail
		}
		switch policy {
		case FailureFail, FailureDisable, FailureWarn:
		default:
			return nil, fmt.Errorf("extension %s has invalid failurePolicy %q", extensionCfg.Name, extensionCfg.FailurePolicy)
		}
		for _, capability := range spec.Provides {
			if previous, conflict := provided[capability]; conflict {
				return nil, fmt.Errorf("extension %s capability %q conflicts with provider %s", extensionCfg.Name, capability, previous)
			}
		}

		instance, err := factory.Build(BuildContext{
			ModelID:       modelID,
			ExtensionName: extensionCfg.Name,
			Model:         modelCfg,
			Host:          Host{Logger: logger},
		}, extensionCfg.Config)
		if err != nil {
			return nil, fmt.Errorf("build extension %s (%s): %w", extensionCfg.Name, extensionCfg.Type, err)
		}
		if instance == nil {
			return nil, fmt.Errorf("build extension %s (%s): factory returned nil", extensionCfg.Name, extensionCfg.Type)
		}
		if instance.Name() != extensionCfg.Name {
			if closer, ok := instance.(Closer); ok {
				_ = closer.Close(context.Background())
			}
			return nil, fmt.Errorf("build extension %s (%s): instance reported name %q", extensionCfg.Name, extensionCfg.Type, instance.Name())
		}

		for _, capability := range spec.Provides {
			provided[capability] = extensionCfg.Name
		}
		// Type names are accepted as dependencies for simple extensions that do
		// not publish a separate capability contract.
		provided[extensionCfg.Type] = extensionCfg.Name

		entries = append(entries, Entry{
			Name:          extensionCfg.Name,
			Type:          extensionCfg.Type,
			Extension:     instance,
			FailurePolicy: policy,
			ReadyTimeout:  extensionCfg.ReadyTimeout.Value(),
			StopTimeout:   extensionCfg.StopTimeout.Value(),
		})
	}
	return newChain(modelID, entries, logger), nil
}

// ModelChain returns the chain owned by modelID for process construction.
func (m *Manager) ModelChain(modelID string) (*Chain, error) {
	chain, ok := m.chains[modelID]
	if !ok {
		return nil, fmt.Errorf("no extension chain for model %q", modelID)
	}
	return chain, nil
}

// Close releases every model chain exactly once. Reverse deterministic model
// order makes shutdown logs and error aggregation reproducible.
func (m *Manager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		modelIDs := make([]string, 0, len(m.chains))
		for modelID := range m.chains {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(modelIDs)))
		var errs []error
		for _, modelID := range modelIDs {
			if err := m.chains[modelID].Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("model %s: %w", modelID, err))
			}
		}
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}
