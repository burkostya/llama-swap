package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

const resourceModelLabel = "io.llama-swap.resource.model"
const resourceGenerationLabel = "io.llama-swap.resource.generation"

// The reservation is durable before the runner can create a container. ID and
// DaemonID are learned asynchronously and may only strengthen this identity.
type resourceRuntimeIdentity struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	Generation uint64 `json:"generation"`
	ID         string `json:"id,omitempty"`
	DaemonID   string `json:"daemonID,omitempty"`
}

type resourceDockerResult struct {
	identity        resourceRuntimeIdentity
	released        bool
	err             error
	creationPending bool
}

type resourceRuntimeCommand func(context.Context, string, ...string) ([]byte, error)

func runResourceRuntimeCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func newResourceRuntime(command, model string, generation uint64) (*resourceRuntimeIdentity, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return &resourceRuntimeIdentity{Type: "docker", Command: command, Model: model, Generation: generation,
		Name: fmt.Sprintf("llama-swap-%d-%x", generation, nonce)}, nil
}

func (identity resourceRuntimeIdentity) environment() []string {
	return []string{
		"LLAMA_SWAP_CONTAINER_NAME=" + identity.Name,
		"LLAMA_SWAP_CONTAINER_LABEL_MODEL=" + resourceModelLabel + "=" + identity.Model,
		"LLAMA_SWAP_CONTAINER_LABEL_GENERATION=" + resourceGenerationLabel + "=" + strconv.FormatUint(identity.Generation, 10),
	}
}

func inspectResourceDocker(identity resourceRuntimeIdentity, run resourceRuntimeCommand) resourceDockerResult {
	result := resourceDockerResult{identity: identity}
	fail := func(err error) resourceDockerResult { result.err = err; return result }
	if identity.Type != "docker" || identity.Name == "" || identity.Model == "" || identity.Generation == 0 {
		return fail(fmt.Errorf("unconfirmed Docker reservation"))
	}
	ctx := context.Background()
	info, err := run(ctx, identity.Command, "info", "--format", "{{.ID}}")
	if err != nil {
		return fail(err)
	}
	daemon := strings.TrimSpace(string(info))
	if daemon == "" || (identity.DaemonID != "" && daemon != identity.DaemonID) {
		return fail(fmt.Errorf("docker daemon identity is unconfirmed or changed"))
	}
	result.identity.DaemonID = daemon
	target := identity.ID
	if target == "" {
		target = identity.Name
	}
	data, inspectErr := run(ctx, identity.Command, "container", "inspect", target)
	if inspectErr != nil {
		// An inspect failure is not absence: independently require a successful
		// daemon listing, and only an already-observed exact ID can be retired.
		filter := "name=^/" + identity.Name + "$"
		if identity.ID != "" {
			filter = "id=" + identity.ID
		}
		ids, err := run(ctx, identity.Command, "container", "ls", "--all", "--quiet", "--no-trunc", "--filter", filter)
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(string(ids)) == "" && identity.ID != "" && identity.DaemonID != "" {
			result.released = true
			return result
		}
		result.creationPending = identity.ID == "" && strings.TrimSpace(string(ids)) == ""
		return fail(fmt.Errorf("docker creation/absence is unconfirmed: %w", inspectErr))
	}
	var containers []struct {
		ID     string `json:"Id"`
		Name   string
		Config struct{ Labels map[string]string }
		State  struct {
			Status                      string
			Running, Paused, Restarting bool
		}
		HostConfig struct{ RestartPolicy struct{ Name string } }
	}
	if err := json.Unmarshal(data, &containers); err != nil || len(containers) != 1 {
		return fail(fmt.Errorf("invalid Docker inspect response"))
	}
	c := containers[0]
	decoded, err := hex.DecodeString(c.ID)
	if err != nil || len(decoded) != 32 || c.Name != "/"+identity.Name ||
		c.Config.Labels[resourceModelLabel] != identity.Model || c.Config.Labels[resourceGenerationLabel] != strconv.FormatUint(identity.Generation, 10) ||
		(identity.ID != "" && identity.ID != c.ID) {
		return fail(fmt.Errorf("docker container ID, name or generation labels do not match reservation"))
	}
	if policy := c.HostConfig.RestartPolicy.Name; policy != "" && policy != "no" {
		return fail(fmt.Errorf("docker resource containers must disable restart policy"))
	}
	result.identity.ID = c.ID
	if c.State.Running || c.State.Paused || c.State.Restarting {
		return result
	}
	if c.State.Status != "exited" && c.State.Status != "dead" {
		return fail(fmt.Errorf("docker terminal state is unconfirmed: %s", c.State.Status))
	}
	result.released = true
	return result
}

func (r *Resources) checkRuntime(b *resourceBackend) {
	if b.runtime == nil || b.runtimePending || b.runtimeReleased || r.now().Before(b.runtimeNextCheck) {
		return
	}
	b.runtimePending = true
	identity, id, generation, run, afterExit := *b.runtime, b.id, b.generation, r.runtimeCommand, b.processExitedConfirmed
	go func() {
		result := inspectResourceDocker(identity, run)
		r.submit(func() { r.finishRuntimeCheck(id, generation, result, afterExit) })
	}()
}

func (r *Resources) finishRuntimeCheck(id string, generation uint64, result resourceDockerResult, afterExit bool) {
	current := r.backends[id]
	if current == nil || current.generation != generation || current.runtime == nil {
		return
	}
	current.runtimePending = false
	current.runtimeNextCheck = r.now().Add(250 * time.Millisecond)
	if err := r.recordRuntimeIdentity(current, result.identity); err != nil {
		r.setState(current, "blocked", err.Error())
		return
	}
	if result.err != nil {
		// Startup can precede docker create. Keep observing, but never
		// turn a missing/unconfirmed container into released ownership.
		if !result.creationPending || current.startDone || current.recovered || current.state != "starting" {
			r.setState(current, "blocked", result.err.Error())
		}
		return
	}
	current.runtimeReleased = result.released && afterExit
}

func (r *Resources) recordRuntimeIdentity(b *resourceBackend, observed resourceRuntimeIdentity) error {
	current := b.runtime
	if (observed.ID != "" && current.ID != "" && observed.ID != current.ID) || (observed.DaemonID != "" && current.DaemonID != "" && observed.DaemonID != current.DaemonID) {
		return fmt.Errorf("docker identity changed during observation")
	}
	if (current.ID != "" || observed.ID == "") && (current.DaemonID != "" || observed.DaemonID == "") {
		return nil
	}
	if current.ID == "" {
		current.ID = observed.ID
	}
	if current.DaemonID == "" {
		current.DaemonID = observed.DaemonID
	}
	owner := r.ledger.Owners[b.id]
	owner.Runtime = current
	r.ledger.Owners[b.id] = owner
	if !r.persist() {
		return fmt.Errorf("docker identity could not be persisted")
	}
	return nil
}

func (r *Resources) forceRuntime(b *resourceBackend, reply chan error) {
	if b.forcePending {
		reply <- fmt.Errorf("force-stop already pending for this generation")
		return
	}
	b.forcePending = true
	r.beginDrain(b, "explicit force-stop")
	reservation, id, gen, run, p := *b.runtime, b.id, b.generation, r.runtimeCommand, b.p
	var group *process.ResourceIdentity
	if b.identity != nil {
		copy := *b.identity
		group = &copy
	}
	go func() {
		result := inspectResourceDocker(reservation, run)
		err := result.err
		if err == nil && !result.released {
			_, err = run(context.Background(), reservation.Command, "container", "kill", "--signal=KILL", result.identity.ID)
		}
		// The identity is immutable for this effect. Never signal a fresh
		// generation through a mutable backend pointer after an async query.
		var groupErr error
		if group != nil {
			groupErr = process.KillResourceGroup(*group)
		} else if p != nil {
			groupErr = p.ForceResourceStop()
		}
		if err == nil {
			err = groupErr
		}
		if !r.submit(func() {
			current := r.backends[id]
			if current == nil || current.generation != gen || current.runtime == nil {
				reply <- fmt.Errorf("backend generation changed during force-stop")
				return
			}
			current.forcePending = false
			if identityErr := r.recordRuntimeIdentity(current, result.identity); identityErr != nil {
				err = identityErr
			}
			current.runtimeReleased = false
			current.runtimeNextCheck = time.Time{}
			if err != nil {
				r.setState(current, "blocked", err.Error())
			}
			reply <- err
		}) {
			reply <- fmt.Errorf("resource coordinator stopped")
		}
	}()
}
