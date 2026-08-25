package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/store"
)

type listServersResponse struct {
	Servers    []serverJSON `json:"servers"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type exportResponse struct {
	ExportedAt string       `json:"exported_at"`
	Servers    []serverJSON `json:"servers"`
}

type statsResponse struct {
	ServersActive int `json:"servers_active"`
	ServersTotal  int `json:"servers_total"`
	UpCount       int `json:"up_count"`
	Resolutions24 int `json:"resolutions_24h"`
	Probes24      int `json:"probes_24h"`
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	status, ok := listStatus(params.Get("status"))
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", map[string]string{
			"status": "must be active, delisted, or all",
		})
		return
	}

	servers, nextCursor, err := s.store.ListServers(
		strings.TrimSpace(params.Get("query")),
		strings.TrimSpace(params.Get("capability")),
		status,
		strings.TrimSpace(params.Get("cursor")),
		listLimit(params.Get("limit")),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", map[string]string{
				"cursor": "must reference an existing server",
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list servers")
		return
	}

	writeJSON(w, http.StatusOK, listServersResponse{
		Servers:    serversJSON(servers),
		NextCursor: nextCursor,
	})
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	capability := strings.TrimSpace(r.URL.Query().Get("capability"))
	if capability == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", map[string]string{
			"capability": "is required",
		})
		return
	}

	servers, err := s.store.GetUpServersByCapability(capability)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to resolve servers")
		return
	}

	if err := events.Log(s.store, "resolve", map[string]any{
		"capability":   capability,
		"result_count": len(servers),
		"ua_tag":       uaTag(r.Header.Get("User-Agent")),
	}, r.RemoteAddr); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to log resolve event")
		return
	}

	for _, server := range servers {
		_ = s.store.IncrementResolutionCount(server.ID)
	}

	writeJSON(w, http.StatusOK, serversJSON(servers))
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ExportAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to export servers")
		return
	}

	writeJSON(w, http.StatusOK, exportResponse{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Servers:    serversJSON(servers),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	activeCount, err := s.store.CountServers("active")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to count active servers")
		return
	}
	totalCount, err := s.store.CountServers("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to count servers")
		return
	}
	upCount, err := s.store.CountUpServers("active")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to count up servers")
		return
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	resolveCount, err := s.store.CountEventsSince("resolve", cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to count resolve events")
		return
	}
	probeCount, err := s.store.CountEventsSince("probe", cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to count probe events")
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		ServersActive: activeCount,
		ServersTotal:  totalCount,
		UpCount:       upCount,
		Resolutions24: resolveCount,
		Probes24:      probeCount,
	})
}

func listStatus(status string) (string, bool) {
	switch strings.TrimSpace(status) {
	case "", "active":
		return "active", true
	case "delisted":
		return "delisted", true
	case "all":
		return "", true
	default:
		return "", false
	}
}

func listLimit(raw string) int {
	if raw == "" {
		return 20
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}

	return limit
}

func serversJSON(servers []store.Server) []serverJSON {
	out := make([]serverJSON, len(servers))
	for i, server := range servers {
		out[i] = toServerJSON(server)
	}

	return out
}

func uaTag(userAgent string) string {
	fields := strings.Fields(userAgent)
	if len(fields) == 0 {
		return ""
	}

	return fields[0]
}

type capabilitiesResponse struct {
	Capabilities []store.CapabilityInfo `json:"capabilities"`
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	server, err := s.store.GetServer(serverID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load server")
		return
	}
	writeJSON(w, http.StatusOK, toServerJSON(server))
}

func (s *Server) handleListCapabilities(w http.ResponseWriter, r *http.Request) {
	caps, err := s.store.ListCapabilities()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list capabilities")
		return
	}
	writeJSON(w, http.StatusOK, capabilitiesResponse{Capabilities: caps})
}
