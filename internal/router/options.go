package router

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// Option customizes local-router construction.
type Option func(*options) error

type options struct {
	processFactory process.Factory
}

// WithProcessFactory supplies the common process constructor used by both
// group and matrix routing modes.
func WithProcessFactory(factory process.Factory) Option {
	return func(opts *options) error {
		if factory == nil {
			return fmt.Errorf("process factory is nil")
		}
		opts.processFactory = factory
		return nil
	}
}

func resolveOptions(opts []Option) (options, error) {
	resolved := options{processFactory: process.DefaultFactory{}}
	for _, opt := range opts {
		if err := opt(&resolved); err != nil {
			return options{}, err
		}
	}
	return resolved, nil
}
