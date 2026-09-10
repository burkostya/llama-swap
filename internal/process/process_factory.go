package process

import (
	"context"
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Factory creates the process implementation used by a router for one model.
// Injection keeps routers independent of optional fork-specific construction.
type Factory interface {
	New(context.Context, string, config.ModelConfig, *logmon.Monitor, *logmon.Monitor) (Process, error)
}

// DefaultFactory preserves upstream behavior by constructing a plain process.
type DefaultFactory struct{}

// New constructs a process without an extension chain.
func (DefaultFactory) New(parentCtx context.Context, modelID string, cfg config.ModelConfig, processLog, proxyLog *logmon.Monitor) (Process, error) {
	return New(parentCtx, modelID, cfg, processLog, proxyLog)
}

// ChainProvider resolves the long-lived model chain attached to a new process.
type ChainProvider interface {
	ModelChain(string) (*extension.Chain, error)
}

// ExtendedFactory attaches a manager-owned extension chain while retaining the
// same process contract used by group and matrix routers.
type ExtendedFactory struct {
	Manager ChainProvider // Manager owns the chains shared with new processes.
}

// New constructs a process with the chain belonging to modelID.
func (f ExtendedFactory) New(parentCtx context.Context, modelID string, cfg config.ModelConfig, processLog, proxyLog *logmon.Monitor) (Process, error) {
	if f.Manager == nil {
		return nil, fmt.Errorf("extension manager is nil")
	}
	chain, err := f.Manager.ModelChain(modelID)
	if err != nil {
		return nil, err
	}
	return New(parentCtx, modelID, cfg, processLog, proxyLog, WithExtensions(chain))
}
