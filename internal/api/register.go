package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/store"
)

var serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

type serverJSON struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	ServerURL     string   `json:"server_url"`
	HealthURL     string   `json:"health_url"`
	Capabilities  []string `json:"capabilities"`
	Status        string   `json:"status"`
	Up            bool     `json:"up"`
	LastCheckedAt string   `json:"last_checked_at"`
	Uptime30d     float64  `json:"uptime_30d"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

func toServerJSON(server store.Server) serverJSON {
	caps := make([]string, len(server.Capabilities))
	copy(caps, server.Capabilities)

	return serverJSON{
		ID:            server.ID,
		Name:          server.Name,
		Description:   server.Description,
		ServerURL:     server.ServerURL,
		HealthURL:     server.HealthURL,
		Capabilities:  caps,
		Status:        server.Status,
		Up:            server.Up,
		LastCheckedAt: server.LastCheckedAt,
		Uptime30d:     server.Uptime30d,
		CreatedAt:     server.CreatedAt,
		UpdatedAt:     server.UpdatedAt,
	}
}

type registerServerRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ServerURL    string   `json:"server_url"`
	HealthURL    string   `json:"health_url"`
	Capabilities []string `json:"capabilities"`
	OwnerContact string   `json:"owner_contact"`
}

type updateServerRequest struct {
	Description  *string   `json:"description"`
	ServerURL    *string   `json:"server_url"`
	HealthURL    *string   `json:"health_url"`
	OwnerContact *string   `json:"owner_contact"`
	Capabilities *[]string `json:"capabilities"`
}

func (s *Server) handleRegisterServer(w http.ResponseWriter, r *http.Request) {
	var req registerServerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	// Fast path: check for duplicate name before validation
	// (gives a clear 409 instead of a confusing validation error when name is taken)
	if _, err := s.store.GetServerByName(req.Name); err == nil {
		writeError(w, http.StatusConflict, "duplicate_name", "server name already exists")
		return
	}

	if detail := validateRegisterServer(req); len(detail) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", detail)
		return
	}

	serverID, err := uuidV4()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate server id")
		return
	}
	writeKey, err := randomHexToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate write key")
		return
	}

	server := store.Server{
		ID:           serverID,
		Name:         req.Name,
		Description:  req.Description,
		ServerURL:    req.ServerURL,
		HealthURL:    req.HealthURL,
		OwnerContact: req.OwnerContact,
		Capabilities: normalizeCapabilities(req.Capabilities),
	}
	if err := s.store.CreateServer(server, hashWriteKey(writeKey)); err != nil {
		if errors.Is(err, store.ErrDuplicateName) {
			writeError(w, http.StatusConflict, "duplicate_name", "server name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create server")
		return
	}

	if err := events.Log(s.store, "register", map[string]any{
		"server_id":        serverID,
		"capability_count": len(server.Capabilities),
	}, r.RemoteAddr); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to log registration event")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"server_id": serverID,
		"write_key": writeKey,
	})
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	server, ok := s.requireAuthorizedServer(w, r)
	if !ok {
		return
	}

	var req updateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if detail := validateUpdateServer(req); len(detail) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", detail)
		return
	}

	if req.Description != nil {
		server.Description = *req.Description
	}
	if req.ServerURL != nil {
		server.ServerURL = *req.ServerURL
	}
	if req.HealthURL != nil {
		server.HealthURL = *req.HealthURL
	}
	if req.OwnerContact != nil {
		server.OwnerContact = *req.OwnerContact
	}
	if req.Capabilities != nil {
		server.Capabilities = normalizeCapabilities(*req.Capabilities)
	}

	if err := s.store.UpdateServer(server); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to update server")
		return
	}

	updated, err := s.store.GetServer(server.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load updated server")
		return
	}
	writeJSON(w, http.StatusOK, toServerJSON(updated))
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	server, ok := s.requireAuthorizedServer(w, r)
	if !ok {
		return
	}

	if err := s.store.DelistServer(server.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delist server")
		return
	}

	if err := events.Log(s.store, "delist", map[string]any{
		"server_id": server.ID,
	}, r.RemoteAddr); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to log delist event")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) requireAuthorizedServer(w http.ResponseWriter, r *http.Request) (store.Server, bool) {
	serverID := r.PathValue("id")
	server, err := s.store.GetServer(serverID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "server not found")
			return store.Server{}, false
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load server")
		return store.Server{}, false
	}

	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) || strings.TrimSpace(strings.TrimPrefix(auth, prefix)) == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid write key")
		return store.Server{}, false
	}

	storedHash, err := s.store.GetServerWriteKeyHash(serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load write key hash")
		return store.Server{}, false
	}
	gotHash := hashWriteKey(strings.TrimSpace(strings.TrimPrefix(auth, prefix)))
	if subtle.ConstantTimeCompare([]byte(gotHash), []byte(storedHash)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid write key")
		return store.Server{}, false
	}

	return server, true
}

func decodeJSON(r *http.Request, v any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return err
	}

	return nil
}

func validateRegisterServer(req registerServerRequest) map[string]string {
	detail := make(map[string]string)
	if !serverNamePattern.MatchString(req.Name) {
		detail["name"] = "must match ^[a-z0-9][a-z0-9-]{1,63}$"
	}
	if !validHTTPURL(req.ServerURL) {
		detail["server_url"] = "must be a valid http or https URL"
	}
	if req.HealthURL != "" && !validHTTPURL(req.HealthURL) {
		detail["health_url"] = "must be a valid http or https URL"
	}
	if err := validateCapabilities(req.Capabilities); err != "" {
		detail["capabilities"] = err
	}
	if len(req.OwnerContact) > 200 {
		detail["owner_contact"] = "must be 200 characters or fewer"
	}

	return detail
}

func validateUpdateServer(req updateServerRequest) map[string]string {
	detail := make(map[string]string)
	if req.ServerURL != nil && !validHTTPURL(*req.ServerURL) {
		detail["server_url"] = "must be a valid http or https URL"
	}
	if req.HealthURL != nil && *req.HealthURL != "" && !validHTTPURL(*req.HealthURL) {
		detail["health_url"] = "must be a valid http or https URL"
	}
	if req.Capabilities != nil {
		if err := validateCapabilities(*req.Capabilities); err != "" {
			detail["capabilities"] = err
		}
	}
	if req.OwnerContact != nil && len(*req.OwnerContact) > 200 {
		detail["owner_contact"] = "must be 200 characters or fewer"
	}

	return detail
}

func validateCapabilities(caps []string) string {
	if len(caps) == 0 {
		return "must include at least 1 capability"
	}
	if len(caps) > 20 {
		return "must include no more than 20 capabilities"
	}
	for _, cap := range caps {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			return "must not include empty capabilities"
		}
		if len(cap) > 128 {
			return "each capability must be 128 characters or fewer"
		}
	}

	return ""
}

func validHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}

	return parsed.Host != ""
}

func normalizeCapabilities(caps []string) []string {
	seen := make(map[string]struct{}, len(caps))
	normalized := make([]string, 0, len(caps))
	for _, cap := range caps {
		cap = strings.TrimSpace(cap)
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		normalized = append(normalized, cap)
	}
	sort.Strings(normalized)

	return normalized
}

func hashWriteKey(writeKey string) string {
	sum := sha256.Sum256([]byte(writeKey))
	return hex.EncodeToString(sum[:])
}
