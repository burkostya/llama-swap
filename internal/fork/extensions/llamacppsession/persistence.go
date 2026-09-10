package llamacppsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
)

const metadataVersion = 1

var sessionKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type metadataDocument struct {
	Version  int                       `json:"version"`
	Sessions map[string]metadataRecord `json:"sessions"`
}

type metadataRecord struct {
	LastUsed time.Time `json:"lastUsed"`
	Snapshot bool      `json:"snapshot"`
}

func (e *Extension) metadataPath() string {
	return filepath.Join(e.config.PersistencePath, e.config.MetadataFile)
}

func (e *Extension) snapshotFilename(key string) string { return "session-" + key + ".bin" }

func (e *Extension) snapshotPath(key string) string {
	return filepath.Join(e.config.PersistencePath, e.snapshotFilename(key))
}

// loadMetadata restores only logical persistent sessions. Hot slot ownership is
// never durable, and a missing snapshot downgrades the record instead of
// promising a restore that cannot succeed.
func (e *Extension) loadMetadata() error {
	raw, err := os.ReadFile(e.metadataPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session metadata: %w", err)
	}
	var document metadataDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode session metadata: %w", err)
	}
	if document.Version != metadataVersion {
		return fmt.Errorf("unsupported session metadata version %d", document.Version)
	}
	for key, record := range document.Sessions {
		if !sessionKeyPattern.MatchString(key) {
			return fmt.Errorf("session metadata contains invalid key %q", key)
		}
		snapshot := record.Snapshot
		if snapshot {
			if _, err := os.Stat(e.snapshotPath(key)); err != nil {
				snapshot = false
			}
		}
		e.sessions[key] = &sessionState{
			Key:      key,
			Mode:     sessionv1.ModePersistent,
			Slot:     -1,
			Snapshot: snapshot,
			LastUsed: record.LastUsed,
		}
	}
	return nil
}

// persistMetadata snapshots in-memory state under mu, then performs filesystem
// I/O without blocking requests. persistMu preserves write order across
// concurrent eviction, control, and shutdown persistence.
func (e *Extension) persistMetadata() error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	document := metadataDocument{Version: metadataVersion, Sessions: make(map[string]metadataRecord)}
	for key, session := range e.sessions {
		if session.Mode != sessionv1.ModePersistent {
			continue
		}
		document.Sessions[key] = metadataRecord{LastUsed: session.LastUsed, Snapshot: session.Snapshot}
	}
	e.mu.Unlock()

	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	// Write, sync, and rename a private temporary file so readers never observe a
	// partially encoded metadata document after a crash.
	tmp, err := os.CreateTemp(e.config.PersistencePath, ".llama-swap-sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create session metadata temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows does not replace an existing destination with os.Rename. The
		// serialized writer minimizes the unavoidable remove/rename compatibility
		// window while keeping the format portable.
		_ = os.Remove(e.metadataPath())
	}
	if err := os.Rename(tmpName, e.metadataPath()); err != nil {
		return fmt.Errorf("replace session metadata: %w", err)
	}
	return nil
}

// verifySnapshot confirms that llama.cpp wrote the requested file into the
// configured shared directory before metadata advertises it as restorable.
func (e *Extension) verifySnapshot(key string) error {
	info, err := os.Stat(e.snapshotPath(key))
	if err != nil {
		return fmt.Errorf("snapshot not visible at persistencePath (ensure it matches llama-server --slot-save-path): %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot %s is not a regular file", e.snapshotFilename(key))
	}
	return nil
}
