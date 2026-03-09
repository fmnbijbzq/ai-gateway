package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"ai-gateway/internal/upstream"
)

// Admin provides administrative HTTP endpoints.
type Admin struct {
	upstreams []*upstream.Upstream
	logger    *slog.Logger
}

// New creates an Admin handler.
func New(upstreams []*upstream.Upstream, logger *slog.Logger) *Admin {
	return &Admin{
		upstreams: upstreams,
		logger:    logger,
	}
}

// StatusResponse is the JSON response for GET /admin/status.
type StatusResponse struct {
	Upstreams []upstream.Status `json:"upstreams"`
}

// Register attaches admin routes to the given mux.
func (a *Admin) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/status", a.handleStatus)
	mux.HandleFunc("/admin/upstream/", a.handleUpstreamControl)
}

func (a *Admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	resp := StatusResponse{
		Upstreams: make([]upstream.Status, 0, len(a.upstreams)),
	}
	for _, u := range a.upstreams {
		resp.Upstreams = append(resp.Upstreams, u.GetStatus())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (a *Admin) handleUpstreamControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Parse: /admin/upstream/{name}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/admin/upstream/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, `{"error":"invalid path, expected /admin/upstream/{name}/{enable|disable}"}`,
			http.StatusBadRequest)
		return
	}

	name := parts[0]
	action := parts[1]

	var target *upstream.Upstream
	for _, u := range a.upstreams {
		if u.Name == name {
			target = u
			break
		}
	}

	if target == nil {
		http.Error(w, `{"error":"upstream not found"}`, http.StatusNotFound)
		return
	}

	switch action {
	case "enable":
		target.SetEnabled(true)
		a.logger.Info("upstream manually enabled", "upstream", name)
	case "disable":
		target.SetEnabled(false)
		a.logger.Info("upstream manually disabled", "upstream", name)
	default:
		http.Error(w, `{"error":"invalid action, expected enable or disable"}`,
			http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(target.GetStatus())
}
