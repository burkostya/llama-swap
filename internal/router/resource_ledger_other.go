//go:build !linux

package router

import (
	"fmt"
	"github.com/mostlygeek/llama-swap/internal/process"
)

type resourceOwner struct {
	Model      string
	Generation uint64
	Resources  []string
	Identity   *process.ResourceIdentity
}
type resourceLedger struct {
	NextGeneration uint64
	Owners         map[string]resourceOwner
}

func openResourceLedger(string) (*resourceLedger, error) {
	return nil, fmt.Errorf("resources router requires Linux")
}
func (*resourceLedger) save() error  { return fmt.Errorf("resources router requires Linux") }
func (*resourceLedger) close() error { return nil }

func (*resourceLedger) unlock() error { return nil }
func (*resourceLedger) relock() error { return nil }
