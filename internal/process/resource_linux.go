//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func procStat(pid int) (string, int, string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", 0, "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 {
		return "", 0, "", fmt.Errorf("short process stat")
	}
	group, err := strconv.Atoi(fields[2])
	return fields[0], group, fields[19], err
}

func readResourceIdentity(pid int) ResourceIdentity {
	_, group, start, _ := procStat(pid)
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return ResourceIdentity{PID: pid, PGID: group, StartTime: start, BootID: strings.TrimSpace(string(boot))}
}

// ResourceGroupExited checks every remaining non-zombie group member. A
// reaped parent is not evidence that its descendants released the device.
func ResourceGroupExited(identity ResourceIdentity) (bool, error) {
	if identity.PID <= 0 || identity.PGID <= 0 || identity.StartTime == "" || identity.BootID == "" {
		return false, fmt.Errorf("process identity is unconfirmed")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(boot)) != identity.BootID {
		return true, nil
	}
	_, _, start, err := procStat(identity.PID)
	if err == nil && start != identity.StartTime {
		return true, nil
	} // original group cannot survive leader PID reuse
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		state, group, _, err := procStat(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if group == identity.PGID && state != "Z" && state != "X" {
			return false, nil
		}
	}
	return true, nil
}

func KillResourceGroup(identity ResourceIdentity) error {
	exited, err := ResourceGroupExited(identity)
	if err != nil || exited {
		return err
	}
	if err := syscall.Kill(-identity.PGID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func signalResourceProcess(cmd *exec.Cmd, signal string) error {
	sig := map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT, "SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2}[signal]
	if sig == 0 {
		return fmt.Errorf("unsupported drain signal %s", signal)
	}
	return cmd.Process.Signal(sig)
}

// ResourceLeaderExited also observes a reaped parent while cmd.Wait is still
// draining inherited pipes. Descendants are checked separately.
func ResourceLeaderExited(identity ResourceIdentity) (bool, error) {
	if identity.PID <= 0 || identity.StartTime == "" {
		return false, fmt.Errorf("process identity is unconfirmed")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(boot)) != identity.BootID {
		return true, nil
	}
	state, _, start, err := procStat(identity.PID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return start != identity.StartTime || state == "Z" || state == "X", nil
}
