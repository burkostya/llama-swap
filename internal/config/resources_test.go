//go:build linux

package config

import (
	"strings"
	"testing"
	"time"
)

const resourceConfigFixture = `
routing:
  router:
    use: resources
    settings:
      resources:
        stateDir: /tmp/test-resources
        pools:
          gpu0:
            parameters: {device: "a device with spaces"}
models:
  worker:
    proxy: http://127.0.0.1:5800
    cmd: worker --device ${RESOURCE:gpu0.device}
    cmdStop: stop --pid ${PID} --device ${RESOURCE:gpu0.device}
    env: ["DEVICE=${RESOURCE:gpu0.device}"]
    activation: demand
    resources: [gpu0]
`

func TestResources_ConfigDefaultsAndArgv(t *testing.T) {
	c, err := LoadConfigFromReader(strings.NewReader(resourceConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routing.Router.Use != "resources" || len(c.Groups) != 0 || c.Routing.Router.Settings.Resources.DemandTimeout != 30*time.Second {
		t.Fatal("resource mode defaults or normalization changed", c.Routing)
	}
	m := c.Models["worker"]
	if !m.ResourceManaged || m.Drain.Signal != "SIGTERM" || m.Drain.Timeout != 120*time.Second || m.Drain.OnTimeout != "block" {
		t.Fatal("unexpected lifecycle defaults", m.Drain)
	}
	args, err := m.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	for i := range args {
		args[i] = m.ResourceValue(args[i])
	}
	if len(args) != 3 || args[2] != "a device with spaces" {
		t.Fatalf("resource value changed argv boundaries: %#v", args)
	}
}

func TestResources_ConfigValidation(t *testing.T) {
	cases := map[string]string{
		"zero drain timeout":          strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: demand\n    drain: {timeout: 0s}"),
		"unknown drain key":           strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: demand\n    drain: {timout: 1s}"),
		"zero demand timeout":         strings.ReplaceAll(resourceConfigFixture, "stateDir: /tmp/test-resources", "stateDir: /tmp/test-resources\n        demandTimeout: 0s"),
		"unknown resource config key": strings.ReplaceAll(resourceConfigFixture, "stateDir: /tmp/test-resources", "stateDir: /tmp/test-resources\n        typo: 1"),
		"missing stateDir":            strings.ReplaceAll(resourceConfigFixture, "stateDir: /tmp/test-resources", "stateDir: ''"),
		"unknown resource":            strings.ReplaceAll(resourceConfigFixture, "resources: [gpu0]", "resources: [missing]"),
		"duplicate resource":          strings.ReplaceAll(resourceConfigFixture, "resources: [gpu0]", "resources: [gpu0, gpu0]"),
		"missing parameter":           strings.ReplaceAll(resourceConfigFixture, "${RESOURCE:gpu0.device}", "${RESOURCE:gpu0.missing}"),
		"malformed reference":         strings.ReplaceAll(resourceConfigFixture, "${RESOURCE:gpu0.device}", "${RESOURCE:gpu0}"),
		"unassigned reference":        strings.ReplaceAll(resourceConfigFixture, "resources: [gpu0]", "resources: []"),
		"bad activation":              strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: bogus"),
		"bad timeout":                 strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: demand\n    drain: {timeout: -1s}"),
		"forced timeout":              strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: demand\n    drain: {onTimeout: kill}"),
		"group signal":                strings.ReplaceAll(resourceConfigFixture, "activation: demand", "activation: demand\n    drain: {target: group}"),
		"old router":                  strings.ReplaceAll(resourceConfigFixture, "use: resources", "use: group"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfigFromReader(strings.NewReader(body)); err == nil {
				t.Fatal("invalid resource configuration accepted")
			}
		})
	}
}

func TestResources_DockerRuntimeConfiguration(t *testing.T) {
	valid := strings.Replace(resourceConfigFixture, "activation: demand", "activation: demand\n    runtime: {type: docker, command: /absolute/docker}", 1)
	if _, err := LoadConfigFromReader(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		strings.Replace(valid, "/absolute/docker", "docker", 1),
		strings.Replace(valid, "type: docker", "type: unknown", 1),
		strings.Replace(valid, "type: docker,", "type: docker, typo: 1,", 1),
		strings.Replace(valid, "use: resources", "use: group", 1),
	}
	for _, key := range ResourceReservedEnvironment {
		cases = append(cases, strings.Replace(valid, `env: ["DEVICE=${RESOURCE:gpu0.device}"]`, `env: ["`+key+`=forged"]`, 1))
	}
	for _, body := range cases {
		if _, err := LoadConfigFromReader(strings.NewReader(body)); err == nil {
			t.Fatalf("invalid runtime accepted: %s", body)
		}
	}
}
