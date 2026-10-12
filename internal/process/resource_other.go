//go:build !linux

package process

import (
	"fmt"
	"os/exec"
)

func readResourceIdentity(pid int) ResourceIdentity { return ResourceIdentity{PID: pid} }
func ResourceGroupExited(ResourceIdentity) (bool, error) {
	return false, fmt.Errorf("resource mode requires Linux")
}
func KillResourceGroup(ResourceIdentity) error { return fmt.Errorf("resource mode requires Linux") }
func signalResourceProcess(*exec.Cmd, string) error {
	return fmt.Errorf("resource mode requires Linux")
}

func ResourceLeaderExited(ResourceIdentity) (bool, error) {
	return false, fmt.Errorf("resources require Linux")
}
