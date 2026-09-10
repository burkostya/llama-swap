package sessionidentity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	sessionv1 "github.com/mostlygeek/llama-swap/internal/fork/contracts/session/v1"
	"github.com/mostlygeek/llama-swap/internal/fork/extension"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

// WrapUpstream resolves and publishes a session identity before downstream
// wrappers allocate backend resources. Unmanaged paths retain normal upstream
// behavior and do not require a session identifier.
func (e *Extension) WrapUpstream(_ extension.BackendRuntime, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.matchesPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		externalID, sourceMode, err := e.firstValue(r, e.config.Sources)
		if err != nil {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_identity_invalid", Message: err.Error()})
			return
		}
		if externalID == "" {
			switch e.config.MissingPolicy {
			case MissingPassthrough:
				next.ServeHTTP(w, r)
				return
			case MissingEphemeral:
				externalID, err = randomIdentity()
				if err != nil {
					extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusInternalServerError, Code: "session_identity_unavailable", Message: "could not create ephemeral session"})
					return
				}
				sourceMode = sessionv1.ModeEphemeral
			default:
				extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_identity_missing", Message: "no configured session identity source was present"})
				return
			}
		}

		tenant, _, err := e.firstValue(r, e.config.TenantSources)
		if err != nil {
			extension.WriteRequestError(w, &extension.RequestError{Status: http.StatusBadRequest, Code: "session_tenant_invalid", Message: err.Error()})
			return
		}
		if tenant == "" {
			tenant = e.config.DefaultTenant
		}
		mode := sourceMode
		if mode == "" {
			mode = e.config.Mode
		}
		// NUL separators make the tuple unambiguous: different namespace, tenant,
		// and external-ID boundaries cannot collapse to the same HMAC input.
		key := e.digest(e.config.Namespace + "\x00" + tenant + "\x00" + externalID)
		tenantKey := e.digest("tenant\x00" + tenant)[:16]
		identity := sessionv1.Identity{
			Key:       key,
			Namespace: e.config.Namespace + ":" + tenantKey,
			Mode:      mode,
		}
		r = sessionv1.WithIdentity(r, identity)
		// Only the opaque key is observable; raw caller identifiers remain local
		// to this wrapper and are not written to logs or request metadata.
		_ = swaputil.SetReqData(r.Context(), e.config.MetadataKey, identity.Key)
		next.ServeHTTP(w, r)
	})
}

func (e *Extension) firstValue(r *http.Request, sources []Source) (string, sessionv1.Mode, error) {
	// Configuration order is precedence order, allowing cheap header lookup to
	// win without reading a JSON body unless a later source needs it.
	for _, source := range sources {
		if source.Header != "" {
			if value := strings.TrimSpace(r.Header.Get(source.Header)); value != "" {
				return value, source.Mode, nil
			}
			continue
		}
		body, err := extension.RequestBody(r)
		if err != nil {
			return "", "", fmt.Errorf("read JSON session source %q: %w", source.JSONPath, err)
		}
		result := gjson.GetBytes(body.Bytes(), source.JSONPath)
		if result.Exists() {
			if value := strings.TrimSpace(result.String()); value != "" {
				return value, source.Mode, nil
			}
		}
	}
	return "", "", nil
}

func (e *Extension) matchesPath(path string) bool {
	for _, configured := range e.config.Paths {
		if strings.HasSuffix(configured, "*") {
			if strings.HasPrefix(path, strings.TrimSuffix(configured, "*")) {
				return true
			}
		} else if path == configured {
			return true
		}
	}
	return false
}

func (e *Extension) digest(value string) string {
	mac := hmac.New(sha256.New, []byte(e.config.HMACSecret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomIdentity() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
