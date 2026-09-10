package sessionv1

// Mode describes whether a logical session may be restored across backend
// generations.
type Mode string

const (
	// ModePersistent allows a backend extension to save durable session state.
	ModePersistent Mode = "persistent"
	// ModeEphemeral keeps session state generation-local and disposable.
	ModeEphemeral Mode = "ephemeral"
)

// Identity is the opaque session value passed between independent extensions.
// Key is safe to expose to internal metadata and control APIs; it must not
// contain the caller's raw session or tenant identifiers.
type Identity struct {
	Key       string // Key is the opaque stable session identifier.
	Namespace string // Namespace identifies the anonymized tenant scope.
	Mode      Mode   // Mode selects persistent or ephemeral handling.
}
