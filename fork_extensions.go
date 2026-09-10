package main

import (
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/fork/extensions/llamacppsession"
	"github.com/mostlygeek/llama-swap/internal/fork/extensions/sessionidentity"
)

// buildExtensionRegistry is the single fork-owned composition root. Explicit
// registration avoids init side effects and makes linked extension types
// visible during upstream rebases.
func buildExtensionRegistry() *extension.Registry {
	registry := extension.NewRegistry()
	registry.MustRegister(sessionidentity.NewFactory())
	registry.MustRegister(llamacppsession.NewFactory())
	return registry
}
