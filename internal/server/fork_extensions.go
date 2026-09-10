package server

import (
	"context"
	"fmt"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
)

// buildExtensionManager converts the optional registry into both the server-
// owned manager and the process factory shared by either local router.
func buildExtensionManager(cfg config.Config, opts options, logger *logmon.Monitor) (*extension.Manager, []router.Option, error) {
	factory := process.Factory(process.DefaultFactory{})
	var manager *extension.Manager
	var err error
	if opts.extensionRegistry != nil {
		manager, err = extension.NewManager(cfg, opts.extensionRegistry, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("creating extension manager: %w", err)
		}
		factory = process.ExtendedFactory{Manager: manager}
	}
	return manager, []router.Option{router.WithProcessFactory(factory)}, nil
}

// discardExtensionManager rolls back extension instances when later server
// construction fails before ownership can transfer to Server.
func discardExtensionManager(manager *extension.Manager) {
	if manager != nil {
		_ = manager.Close(context.Background())
	}
}

// closeExtensionManager spends only the time left in the server's shutdown
// budget. Processes stop first because their wrappers and hooks still own state
// stored in the manager.
func closeExtensionManager(manager *extension.Manager, deadline time.Time, timeout time.Duration) error {
	if manager == nil {
		return nil
	}
	remaining := time.Until(deadline)
	if timeout <= 0 || remaining <= 0 {
		remaining = time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	return manager.Close(ctx)
}
