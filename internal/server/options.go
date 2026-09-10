package server

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

// Option customizes server construction while preserving the original call
// shape for builds that do not link fork extensions.
type Option func(*options) error

type options struct {
	extensionRegistry *extension.Registry
}

// WithExtensionRegistry enables compile-time registered extensions for this
// server configuration. The server owns the resulting manager and closes it
// only after its model processes have stopped.
func WithExtensionRegistry(registry *extension.Registry) Option {
	return func(opts *options) error {
		if registry == nil {
			return fmt.Errorf("extension registry is nil")
		}
		opts.extensionRegistry = registry
		return nil
	}
}

func resolveOptions(opts []Option) (options, error) {
	var resolved options
	for _, opt := range opts {
		if err := opt(&resolved); err != nil {
			return options{}, err
		}
	}
	return resolved, nil
}
