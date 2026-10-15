//go:build linux

package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mostlygeek/llama-swap/internal/process"
)

type resourceOwner struct {
	Model      string                    `json:"model"`
	Generation uint64                    `json:"generation"`
	Resources  []string                  `json:"resources"`
	Identity   *process.ResourceIdentity `json:"identity,omitempty"`
	Runtime    *resourceRuntimeIdentity  `json:"runtime,omitempty"`
}

type resourceLedger struct {
	dir            string
	lock           *os.File
	NextGeneration uint64                   `json:"nextGeneration"`
	Owners         map[string]resourceOwner `json:"owners"`
}

func openResourceLedger(dir string) (*resourceLedger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("resource stateDir is already in use: %w", err)
	}
	l := &resourceLedger{dir: dir, lock: lock, Owners: map[string]resourceOwner{}}
	data, err := os.ReadFile(filepath.Join(dir, "owners.json"))
	if err != nil && !os.IsNotExist(err) {
		l.close()
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, l); err != nil {
			l.close()
			return nil, fmt.Errorf("invalid resource ledger: %w", err)
		}
	}
	if l.Owners == nil {
		l.close()
		return nil, fmt.Errorf("resource ledger owners is missing")
	}
	for id, owner := range l.Owners {
		if id != owner.Model || owner.Generation == 0 {
			l.close()
			return nil, fmt.Errorf("invalid owner %q in resource ledger", id)
		}
		if owner.Generation > l.NextGeneration {
			l.NextGeneration = owner.Generation
		}
		if owner.Runtime != nil && (owner.Runtime.Type != "docker" || owner.Runtime.Model != id || owner.Runtime.Generation != owner.Generation || !filepath.IsAbs(owner.Runtime.Command) || owner.Runtime.Name == "") {
			l.close()
			return nil, fmt.Errorf("invalid Docker reservation for %q", id)
		}
		if owner.Identity != nil && owner.Runtime == nil {
			exited, err := process.ResourceGroupExited(*owner.Identity)
			if err == nil && exited {
				delete(l.Owners, id)
			}
		}
	}
	if err = l.save(); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}

func (l *resourceLedger) save() error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.dir, ".owners-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(l.dir, "owners.json")); err != nil {
		return err
	}
	d, err := os.Open(l.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (l *resourceLedger) close() error { return l.lock.Close() }

func (l *resourceLedger) unlock() error { return syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN) }
func (l *resourceLedger) relock() error {
	return syscall.Flock(int(l.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
