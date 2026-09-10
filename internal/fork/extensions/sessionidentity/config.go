package sessionidentity

import (
	"fmt"
	"strings"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
)

const (
	// MissingReject rejects managed requests without an external identity.
	MissingReject = "reject"
	// MissingEphemeral creates a cryptographically random one-request identity.
	MissingEphemeral = "ephemeral"
	// MissingPassthrough leaves requests without an identity unmanaged.
	MissingPassthrough = "passthrough"
)

var defaultPaths = []string{
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/completions",
	"/v1/messages",
	"/completion",
	"/infill",
	// Versionless /v/... routes are stripped to these paths by the server
	// before the request reaches the process wrapper.
	"/chat/completions",
	"/responses",
	"/completions",
	"/messages",
}

// Source describes one ordered identity lookup. Exactly one of Header or
// JSONPath must be set; the first non-empty source wins.
type Source struct {
	Header   string         `yaml:"header"`   // Header reads a trimmed HTTP header value.
	JSONPath string         `yaml:"jsonPath"` // JSONPath reads a gjson path from the shared body.
	Mode     sessionv1.Mode `yaml:"mode"`     // Mode optionally overrides the default session mode.
}

// Config controls identity extraction and anonymization. Raw external and
// tenant identifiers are never placed in request metadata or the shared
// session contract.
type Config struct {
	Sources       []Source       `yaml:"sources"`       // Sources are evaluated in order for the session ID.
	TenantSources []Source       `yaml:"tenantSources"` // TenantSources are evaluated separately for isolation.
	Namespace     string         `yaml:"namespace"`     // Namespace defaults to the model ID.
	DefaultTenant string         `yaml:"defaultTenant"` // DefaultTenant scopes requests without a tenant source.
	HMACSecret    string         `yaml:"hmacSecret"`    // HMACSecret protects raw external identifiers.
	Mode          sessionv1.Mode `yaml:"mode"`          // Mode is used when the winning source has no override.
	MissingPolicy string         `yaml:"missingPolicy"` // MissingPolicy selects reject, ephemeral, or passthrough.
	Paths         []string       `yaml:"paths"`         // Paths limits identity handling to inference routes.
	MetadataKey   string         `yaml:"metadataKey"`   // MetadataKey stores the opaque key for observability.
}

func (c *Config) defaults(modelID string) {
	if c.Namespace == "" {
		c.Namespace = modelID
	}
	if c.DefaultTenant == "" {
		c.DefaultTenant = "default"
	}
	if c.Mode == "" {
		c.Mode = sessionv1.ModePersistent
	}
	if c.MissingPolicy == "" {
		c.MissingPolicy = MissingReject
	}
	if len(c.Paths) == 0 {
		c.Paths = append([]string(nil), defaultPaths...)
	}
	if c.MetadataKey == "" {
		c.MetadataKey = "session_key"
	}
}

func (c Config) validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("sources must contain at least one header or jsonPath")
	}
	if strings.TrimSpace(c.HMACSecret) == "" {
		return fmt.Errorf("hmacSecret is required (use ${env.VAR} to keep it outside YAML)")
	}
	if c.Mode != sessionv1.ModePersistent && c.Mode != sessionv1.ModeEphemeral {
		return fmt.Errorf("mode must be %q or %q", sessionv1.ModePersistent, sessionv1.ModeEphemeral)
	}
	switch c.MissingPolicy {
	case MissingReject, MissingEphemeral, MissingPassthrough:
	default:
		return fmt.Errorf("missingPolicy must be reject, ephemeral, or passthrough")
	}
	for _, source := range append(append([]Source{}, c.Sources...), c.TenantSources...) {
		if (strings.TrimSpace(source.Header) == "") == (strings.TrimSpace(source.JSONPath) == "") {
			return fmt.Errorf("each source must set exactly one of header or jsonPath")
		}
		if source.Mode != "" && source.Mode != sessionv1.ModePersistent && source.Mode != sessionv1.ModeEphemeral {
			return fmt.Errorf("source mode must be %q or %q", sessionv1.ModePersistent, sessionv1.ModeEphemeral)
		}
	}
	for _, path := range c.Paths {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("path %q must start with /", path)
		}
	}
	return nil
}
