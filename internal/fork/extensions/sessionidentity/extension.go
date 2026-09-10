package sessionidentity

// Extension publishes backend-independent session identities to downstream
// request wrappers.
type Extension struct {
	name   string
	config Config
}

// Name returns the configured instance name used for lifecycle diagnostics and
// control-plane addressing.
func (e *Extension) Name() string { return e.name }
