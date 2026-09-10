package config

import (
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML duration that accepts Go duration strings (for example,
// "30s") or an integer number of seconds.
type Duration time.Duration

// UnmarshalYAML decodes either a Go duration string or a non-negative integer
// number of seconds. Supporting both forms keeps extension timeouts consistent
// with existing llama-swap configuration conventions.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var parsed time.Duration
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!int" {
			var seconds int64
			if err := value.Decode(&seconds); err != nil {
				return err
			}
			if seconds < 0 {
				return errors.New("duration must not be negative")
			}
			parsed = time.Duration(seconds) * time.Second
		} else {
			var raw string
			if err := value.Decode(&raw); err != nil {
				return err
			}
			var err error
			parsed, err = time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", raw, err)
			}
			if parsed < 0 {
				return errors.New("duration must not be negative")
			}
		}
	default:
		return errors.New("duration must be a string or integer seconds")
	}
	*d = Duration(parsed)
	return nil
}

// Value returns the duration in the standard library representation used by
// runtime code.
func (d Duration) Value() time.Duration { return time.Duration(d) }

// ExtensionConfig is intentionally opaque to core. The registered extension
// factory owns decoding and validation of Config.
type ExtensionConfig struct {
	Name          string    `yaml:"name"`          // Name addresses this instance in logs and the control API.
	Type          string    `yaml:"type"`          // Type selects a compile-time registered factory.
	Enabled       *bool     `yaml:"enabled"`       // Enabled defaults to true while preserving an explicit false.
	Requires      []string  `yaml:"requires"`      // Requires adds configuration-level capability dependencies.
	FailurePolicy string    `yaml:"failurePolicy"` // FailurePolicy controls backend-ready error handling.
	ReadyTimeout  Duration  `yaml:"readyTimeout"`  // ReadyTimeout bounds initialization for each generation.
	StopTimeout   Duration  `yaml:"stopTimeout"`   // StopTimeout bounds this extension within the process deadline.
	Config        yaml.Node `yaml:"config"`        // Config remains opaque until the selected factory decodes it.
}
