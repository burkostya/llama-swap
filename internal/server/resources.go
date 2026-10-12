package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/router"
)

func (s *Server) resourceRouter(w http.ResponseWriter) *router.Resources {
	r, ok := s.local.(*router.Resources)
	if !ok {
		http.Error(w, "resources router is not enabled", http.StatusNotFound)
		return nil
	}
	return r
}

func decodeResourceJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid resource request JSON", 400)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "request must contain one JSON object", 400)
		return false
	}
	return true
}

func (s *Server) handleResourceDemand(w http.ResponseWriter, r *http.Request) {
	resources := s.resourceRouter(w)
	if resources == nil {
		return
	}
	// Require hasWork explicitly; an omitted boolean must not cancel demand.
	var payload struct {
		Model   string `json:"model"`
		Source  string `json:"source"`
		HasWork *bool  `json:"hasWork"`
	}
	if !decodeResourceJSON(w, r, &payload) {
		return
	}
	if payload.HasWork == nil {
		http.Error(w, "hasWork is required", 400)
		return
	}
	status, code, err := resources.Demand(router.ResourceDemand{Model: payload.Model, Source: payload.Source, HasWork: *payload.HasWork})
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	resources := s.resourceRouter(w)
	if resources == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resources.Snapshot())
}
func (s *Server) handleResourceForceStop(w http.ResponseWriter, r *http.Request) {
	resources := s.resourceRouter(w)
	if resources == nil {
		return
	}
	var request struct {
		Model      string `json:"model"`
		Generation uint64 `json:"generation"`
	}
	if !decodeResourceJSON(w, r, &request) {
		return
	}
	if request.Model == "" || request.Generation == 0 {
		http.Error(w, "model and nonzero generation are required", 400)
		return
	}
	if err := resources.ForceStop(request.Model, request.Generation); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) UsesResources() bool { return s.cfg.Routing.Router.Use == "resources" }

// PrepareResourceReload must run before constructing a replacement Server:
// New can preload processes and acquire the resource ledger lock.
func (s *Server) PrepareResourceReload(cfg config.Config) (func(), error) {
	if s.UsesResources() != (cfg.Routing.Router.Use == "resources") {
		return nil, fmt.Errorf("changing to/from resources router requires a restart")
	}
	resources, ok := s.local.(*router.Resources)
	if !ok {
		return func() {}, nil
	}
	if err := resources.PauseReload(); err != nil {
		return nil, err
	}
	return func() {
		if err := resources.ResumeReload(); err != nil {
			s.proxylog.Warnf("resources: cannot resume after rejected reload: %v", err)
		}
	}, nil
}

// DrainResources keeps the administrative listener usable while a blocked
// worker completes or the operator performs a generation-checked force-stop.
func (s *Server) DrainResources() error {
	if r, ok := s.local.(*router.Resources); ok {
		return r.Shutdown(0)
	}
	return nil
}
