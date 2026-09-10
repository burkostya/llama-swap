package extension

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type controlEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Active bool   `json:"active"`
	Path   string `json:"path,omitempty"`
}

// Handler returns the generic control-plane router. Core only resolves the
// model and extension instance; the extension owns all remaining path and
// response semantics.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/extensions")
		path = strings.Trim(path, "/")
		if path == "" {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			m.writeIndex(w)
			return
		}

		modelID, extensionName, rest, ok := m.resolveControlPath(path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		chain := m.chains[modelID]
		for _, entry := range chain.Entries() {
			if entry.Name != extensionName {
				continue
			}
			provider, ok := entry.Extension.(ControlProvider)
			if !ok {
				http.NotFound(w, r)
				return
			}
			// Present an extension-relative path so providers remain independent of
			// the server's global route layout.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + rest
			chain.recoverHandler(entry, provider.ControlHandler()).ServeHTTP(w, r2)
			return
		}
		http.NotFound(w, r)
	})
}

// resolveControlPath uses model-aware prefix matching rather than splitting the
// first path component because configured model IDs may themselves contain '/'.
func (m *Manager) resolveControlPath(path string) (modelID, extensionName, rest string, ok bool) {
	models := make([]string, 0, len(m.chains))
	for id := range m.chains {
		models = append(models, id)
	}
	// Model IDs may contain path separators. Longest-prefix matching prevents a
	// shorter model ID from stealing the control subtree of a more specific ID.
	sort.Slice(models, func(i, j int) bool { return len(models[i]) > len(models[j]) })
	for _, id := range models {
		prefix := id + "/"
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		remaining := strings.TrimPrefix(path, prefix)
		parts := strings.SplitN(remaining, "/", 2)
		if parts[0] == "" {
			return "", "", "", false
		}
		rest := ""
		if len(parts) == 2 {
			rest = parts[1]
		}
		return id, parts[0], rest, true
	}
	return "", "", "", false
}

func (m *Manager) writeIndex(w http.ResponseWriter) {
	result := make(map[string][]controlEntry, len(m.chains))
	for modelID, chain := range m.chains {
		for _, entry := range chain.Entries() {
			item := controlEntry{Name: entry.Name, Type: entry.Type, Active: entry.active}
			if _, ok := entry.Extension.(ControlProvider); ok {
				item.Path = "/api/extensions/" + modelID + "/" + entry.Name + "/"
			}
			result[modelID] = append(result[modelID], item)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": result})
}
