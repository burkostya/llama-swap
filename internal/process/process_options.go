package process

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

// Option customizes process construction without changing existing callers.
type Option func(*ProcessCommand) error

// WithExtensions attaches the model-scoped chain that receives process
// lifecycle events and wraps the final reverse-proxy handler.
func WithExtensions(chain *extension.Chain) Option {
	return func(process *ProcessCommand) error {
		if chain == nil {
			return fmt.Errorf("extension chain is nil")
		}
		process.extensions = chain
		return nil
	}
}
