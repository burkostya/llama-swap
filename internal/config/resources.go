package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ResourceConfig describes static, exclusive resources. It does not discover
// hardware or promise isolation from processes outside this supervisor.
type ResourceConfig struct {
	StateDir      string                  `yaml:"stateDir"`
	DemandTimeout time.Duration           `yaml:"demandTimeout"`
	Pools         map[string]ResourcePool `yaml:"pools"`
}

type ResourcePool struct {
	Parameters map[string]string `yaml:"parameters" json:"parameters"`
}

// RuntimeConfig adds an external runtime fence to resource-managed processes.
type RuntimeConfig struct {
	Type    string `yaml:"type" json:"type"`
	Command string `yaml:"command" json:"command"`
}

var ResourceReservedEnvironment = []string{
	"LLAMA_SWAP_RESOURCE_MODEL", "LLAMA_SWAP_RESOURCE_GENERATION",
	"LLAMA_SWAP_CONTAINER_NAME", "LLAMA_SWAP_CONTAINER_LABEL_MODEL", "LLAMA_SWAP_CONTAINER_LABEL_GENERATION",
}

func (r *RuntimeConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw RuntimeConfig
	var v raw
	if err := decodeResourceFields(node, &v, "type", "command"); err != nil {
		return err
	}
	*r = RuntimeConfig(v)
	return nil
}

type DrainConfig struct {
	Signal    string        `yaml:"signal"`
	Target    string        `yaml:"target"`
	Timeout   time.Duration `yaml:"timeout"`
	OnTimeout string        `yaml:"onTimeout"`
}

func decodeResourceFields(node *yaml.Node, value any, fields ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("resource settings must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		if !slices.Contains(fields, node.Content[i].Value) {
			return fmt.Errorf("unknown resource setting %q", node.Content[i].Value)
		}
	}
	return node.Decode(value)
}

func (r *ResourceConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw ResourceConfig
	v := raw{DemandTimeout: 30 * time.Second}
	if err := decodeResourceFields(node, &v, "stateDir", "demandTimeout", "pools"); err != nil {
		return err
	}
	if v.DemandTimeout <= 0 {
		return fmt.Errorf("resources.demandTimeout must be positive")
	}
	*r = ResourceConfig(v)
	return nil
}
func (r *ResourcePool) UnmarshalYAML(node *yaml.Node) error {
	type raw ResourcePool
	var v raw
	if err := decodeResourceFields(node, &v, "parameters"); err != nil {
		return err
	}
	*r = ResourcePool(v)
	return nil
}
func (r *DrainConfig) UnmarshalYAML(node *yaml.Node) error {
	type raw DrainConfig
	v := raw{Signal: "SIGTERM", Target: "process", Timeout: 120 * time.Second, OnTimeout: "block"}
	if err := decodeResourceFields(node, &v, "signal", "target", "timeout", "onTimeout"); err != nil {
		return err
	}
	if v.Timeout <= 0 {
		return fmt.Errorf("drain.timeout must be positive")
	}
	*r = DrainConfig(v)
	return nil
}

var resourceReference = regexp.MustCompile(`\$\{RESOURCE:([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_-]+)\}`)

func validateResources(c *Config) error {
	enabled := c.Routing.Router.Use == "resources"
	rc := c.Routing.Router.Settings.Resources
	if !enabled {
		if rc != nil {
			return fmt.Errorf("routing.router.settings.resources requires router use: resources")
		}
		for id, m := range c.Models {
			if m.Runtime != nil || m.Activation != "" || len(m.Resources) > 0 || m.Drain != (DrainConfig{}) || strings.Contains(m.Cmd+m.CmdStop+strings.Join(m.Env, ""), "${RESOURCE:") {
				return fmt.Errorf("model %s: resource options require router use: resources", id)
			}
		}
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("resources router is supported on Linux only")
	}
	if rc == nil || strings.TrimSpace(rc.StateDir) == "" {
		return fmt.Errorf("routing.router.settings.resources.stateDir is required")
	}
	if rc.DemandTimeout == 0 {
		rc.DemandTimeout = 30 * time.Second
	}
	if rc.DemandTimeout < 0 {
		return fmt.Errorf("resources.demandTimeout must be positive")
	}
	for name, pool := range rc.Pools {
		if !macroNameRegex.MatchString(name) {
			return fmt.Errorf("invalid resource name %q", name)
		}
		for key := range pool.Parameters {
			if !macroNameRegex.MatchString(key) {
				return fmt.Errorf("invalid parameter %q on resource %s", key, name)
			}
		}
	}
	for id, m := range c.Models {
		if m.Runtime != nil && (m.Runtime.Type != "docker" || !filepath.IsAbs(m.Runtime.Command)) {
			return fmt.Errorf("model %s: runtime requires type docker and an absolute command", id)
		}
		for _, env := range m.Env {
			key, _, _ := strings.Cut(env, "=")
			if slices.Contains(ResourceReservedEnvironment, key) {
				return fmt.Errorf("model %s: reserved environment %s cannot be overridden", id, key)
			}
		}
		if m.Activation == "" {
			m.Activation = "request"
		}
		if !slices.Contains([]string{"request", "demand", "both"}, m.Activation) {
			return fmt.Errorf("model %s: invalid activation %q", id, m.Activation)
		}
		seen := map[string]bool{}
		for _, name := range m.Resources {
			if seen[name] {
				return fmt.Errorf("model %s: duplicate resource %s", id, name)
			}
			seen[name] = true
			if _, ok := rc.Pools[name]; !ok {
				return fmt.Errorf("model %s: unknown resource %s", id, name)
			}
		}
		if m.Drain.Signal == "" {
			m.Drain.Signal = "SIGTERM"
		}
		if m.Drain.Target == "" {
			m.Drain.Target = "process"
		}
		if m.Drain.Timeout == 0 {
			m.Drain.Timeout = 120 * time.Second
		}
		if m.Drain.OnTimeout == "" {
			m.Drain.OnTimeout = "block"
		}
		if !slices.Contains([]string{"SIGTERM", "SIGINT", "SIGUSR1", "SIGUSR2"}, m.Drain.Signal) || m.Drain.Target != "process" || m.Drain.Timeout <= 0 || m.Drain.OnTimeout != "block" {
			return fmt.Errorf("model %s: invalid drain configuration", id)
		}
		for _, value := range append([]string{m.Cmd, m.CmdStop}, m.Env...) {
			for _, ref := range resourceReference.FindAllStringSubmatch(value, -1) {
				if !seen[ref[1]] {
					return fmt.Errorf("model %s: resource reference %s is not assigned", id, ref[1])
				}
				if _, ok := rc.Pools[ref[1]].Parameters[ref[2]]; !ok {
					return fmt.Errorf("model %s: unknown resource parameter %s.%s", id, ref[1], ref[2])
				}
			}
			if strings.Contains(resourceReference.ReplaceAllString(value, ""), "${RESOURCE:") {
				return fmt.Errorf("model %s: malformed resource reference", id)
			}
		}
		// Never interpolate resource values into shell text. Substitute command
		// arguments after argv parsing, so whitespace remains one argument.
		m.ResourceManaged = true
		m.ResourceParameters = map[string]string{}
		for _, name := range m.Resources {
			for key, value := range rc.Pools[name].Parameters {
				m.ResourceParameters["${RESOURCE:"+name+"."+key+"}"] = value
			}
		}
		c.Models[id] = m
	}
	return nil
}

func (m ModelConfig) ResourceValue(value string) string {
	return resourceReference.ReplaceAllStringFunc(value, func(ref string) string { return m.ResourceParameters[ref] })
}
