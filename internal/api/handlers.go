package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/store"
)

// --- Embedded content ---

//go:embed llms.txt
var llmsTxtContent string

//go:embed index.html
var landingPageHTML string

// --- Request/Response types ---

// RegisterRequest is the JSON body for POST /v0/servers.
type RegisterRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ServerURL    string   `json:"server_url"`
	HealthURL    string   `json:"health_url"`
	ProbeMethod  string   `json:"probe_method"`
	Capabilities []string `json:"capabilities"`
	OwnerContact string   `json:"owner_contact"`
}

// RegisterResponse is returned on successful registration.
type RegisterResponse struct {
	ServerID string `json:"server_id"`
	WriteKey string `json:"write_key"`
}

// UpdateRequest is the JSON body for PUT /v0/servers/{id}.
type UpdateRequest struct {
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	ServerURL    *string   `json:"server_url,omitempty"`
	HealthURL    *string   `json:"health_url,omitempty"`
	ProbeMethod  *string   `json:"probe_method,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	OwnerContact *string   `json:"owner_contact,omitempty"`
}

// ServerWithState is the public JSON shape for list/resolve responses.
type ServerWithState struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	ServerURL     string   `json:"server_url"`
	HealthURL     string   `json:"health_url,omitempty"`
	Capabilities  []string `json:"capabilities"`
	Status        string   `json:"status"`
	Up            int      `json:"up"`
	Uptime30d     float64  `json:"uptime_30d"`
	LastCheckedAt string   `json:"last_checked_at,omitempty"`
	OwnerContact  string   `json:"owner_contact,omitempty"`
	ProbeMethod   string   `json:"probe_method,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	// Computed fields (derived from existing data — not stored in DB)
	Source      string   `json:"source,omitempty"`
	SourceURL   string   `json:"source_url,omitempty"`
	Auth        string   `json:"auth,omitempty"`
	ToolCount   int      `json:"tool_count,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty"`
	CurlSnippet string   `json:"curl_snippet,omitempty"`
}

// --- Validation ---

var nameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validateRegisterRequest(req *RegisterRequest) (string, int) {
	if req.Name == "" {
		return "name is required", 422
	}
	if !nameRegex.MatchString(req.Name) {
		return "name must be 1-63 lowercase alphanumeric or hyphen, starting with alphanumeric", 422
	}
	if req.ServerURL == "" {
		return "server_url is required", 422
	}
	if !strings.HasPrefix(req.ServerURL, "http://") && !strings.HasPrefix(req.ServerURL, "https://") {
		return "server_url must start with http:// or https://", 422
	}
	if req.HealthURL != "" && !strings.HasPrefix(req.HealthURL, "http://") && !strings.HasPrefix(req.HealthURL, "https://") {
		return "health_url must start with http:// or https://", 422
	}
	if req.ProbeMethod != "" {
		switch strings.ToUpper(req.ProbeMethod) {
		case "GET", "POST":
		default:
			return "probe_method must be GET or POST", 422
		}
	}
	if len(req.Capabilities) < 1 || len(req.Capabilities) > 20 {
		return "capabilities must have 1-20 items", 422
	}
	return "", 0
}

// --- Crypto helpers ---

func hashWriteKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func generateWriteKey() string {
	b := make([]byte, 32)
	_ = uuid.New() // ensure import
	for i := 0; i < 4; i++ {
		id := uuid.New()
		copy(b[i*8:], id[:8])
	}
	return hex.EncodeToString(b)
}

// computeSource derives a source label and URL from server metadata.
func computeSource(sws store.ServerWithState) (string, string) {
	desc := strings.ToLower(sws.Description)
	contact := strings.ToLower(sws.OwnerContact)
	switch {
	case strings.Contains(contact, "smithery") || strings.Contains(desc, "smithery"):
		return "Smithery", "https://registry.smithery.ai"
	case strings.Contains(contact, "npm registry") || strings.Contains(desc, "npm"):
		return "npm", "https://www.npmjs.com"
	case strings.Contains(contact, "mcp registry") || strings.Contains(desc, "mcp registry") || strings.Contains(desc, "official mcp"):
		return "MCP Registry", "https://registry.modelcontextprotocol.io"
	case strings.Contains(desc, "trucore") || sws.Name == "meshdns-registry":
		return "TruCore", "https://www.trucore.xyz"
	default:
		return "manual", ""
	}
}

// computeAuth determines auth status: public, auth-required, or unknown.
func computeAuth(sws store.ServerWithState) string {
	hasHealth := strings.TrimSpace(sws.HealthURL) != ""
	isPostOnly := sws.ProbeMethod == "POST"
	descAuth := strings.Contains(strings.ToLower(sws.Description), "auth required") ||
		strings.Contains(strings.ToLower(sws.Description), "bring your own")
	if !hasHealth {
		return "unknown"
	} else if isPostOnly || descAuth {
		return "auth-required"
	}
	return "public"
}

// computeToolInfo extracts tool names and count from description (Smithery format: " — tools: name1, name2, ...")
func computeToolInfo(desc string) ([]string, int) {
	if idx := strings.Index(desc, " — tools: "); idx >= 0 {
		toolsStr := desc[idx+len(" — tools: "):]
		if end := strings.Index(toolsStr, " ["); end >= 0 {
			toolsStr = toolsStr[:end]
		}
		names := strings.Split(toolsStr, ", ")
		return names, len(names)
	}
	return nil, 0
}

// computeCurlSnippet builds a lazy MCP curl command.
func computeCurlSnippet(serverURL string) string {
	if serverURL == "" {
		return ""
	}
	return fmt.Sprintf(
		`python3 ~/repo/hermes-trading/.hermes/scripts/lazymcp.py call --url %s --tool <tool> --args '{"key":"value"}'`,
		serverURL)
}

func storeServerWithStateToAPI(sws store.ServerWithState) ServerWithState {
	source, sourceURL := computeSource(sws)
	if sws.Source != "" {
		source = sws.Source
	}
	toolNames, toolCount := computeToolInfo(sws.Description)
	if sws.ToolCount > 0 {
		toolCount = sws.ToolCount
	}
	curlSnippet := computeCurlSnippet(sws.ServerURL)
	return ServerWithState{
		ID:            sws.ID,
		Name:          sws.Name,
		Description:   sws.Description,
		ServerURL:     sws.ServerURL,
		HealthURL:     sws.HealthURL,
		Capabilities:  sws.Capabilities,
		Status:        sws.Status,
		Up:            sws.Up,
		Uptime30d:     sws.Uptime30d,
		LastCheckedAt: sws.LastCheckedAt,
		OwnerContact:  sws.OwnerContact,
		ProbeMethod:   sws.ProbeMethod,
		CreatedAt:     sws.CreatedAt,
		UpdatedAt:     sws.UpdatedAt,
		Source:        source,
		SourceURL:     sourceURL,
		Auth:          computeAuth(sws),
		ToolCount:     toolCount,
		ToolNames:     toolNames,
		CurlSnippet:   curlSnippet,
	}
}

// --- Route handlers ---

// GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write([]byte(`{"status":"ok"}`))
}

// GET /llms.txt
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(llmsTxtContent))
}

// GET /
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	// Don't serve the landing page for API paths
	if strings.HasPrefix(r.URL.Path, "/v0/") || r.URL.Path == "/health" || r.URL.Path == "/llms.txt" {
		writeError(w, 404, "not_found", "endpoint not found")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(landingPageHTML))
}

// POST /v0/servers — register a server
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}

	if msg, status := validateRegisterRequest(&req); msg != "" {
		writeError(w, status, "validation_error", msg)
		return
	}

	// Check for duplicate name
	existing, _ := s.Store.GetServerByName(req.Name)
	if existing != nil {
		writeError(w, 409, "duplicate_name", "a server with this name already exists")
		return
	}

	// Generate server_id and write_key
	serverID := uuid.New().String()
	writeKey := generateWriteKey()
	writeKeyHash := hashWriteKey(writeKey)

	probeMethod := strings.ToUpper(req.ProbeMethod)
	if probeMethod == "" {
		probeMethod = "GET"
	}

	_, err := s.Store.CreateServer(&store.Server{
		ID:           serverID,
		Name:         req.Name,
		Description:  req.Description,
		ServerURL:    req.ServerURL,
		HealthURL:    req.HealthURL,
		WriteKeyHash: writeKeyHash,
		OwnerContact: req.OwnerContact,
		ProbeMethod:  probeMethod,
	})
	if err != nil {
		writeError(w, 500, "db_error", "failed to create server: "+err.Error())
		return
	}

	// Set capabilities
	s.Store.SetCapabilities(serverID, req.Capabilities)

	// Log event
	events.Log(s.Store, "register", map[string]any{
		"server_id":        serverID,
		"capability_count": len(req.Capabilities),
	}, r)

	writeJSON(w, 201, RegisterResponse{ServerID: serverID, WriteKey: writeKey})
}

// GET /v0/servers/{id} — get a single server by ID
func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")

	srv, err := s.Store.GetServer(serverID)
	if err != nil {
		writeError(w, 404, "not_found", "server not found")
		return
	}

	caps, _ := s.Store.GetCapabilities(serverID)

	// Get state if available
	servers, _, _ := s.Store.ListServers("", "", "all", srv.ID, 2)

	result := ServerWithState{
		ID:           srv.ID,
		Name:         srv.Name,
		Description:  srv.Description,
		ServerURL:    srv.ServerURL,
		HealthURL:    srv.HealthURL,
		Capabilities: caps,
		Status:       srv.Status,
		OwnerContact: srv.OwnerContact,
		ProbeMethod:  srv.ProbeMethod,
		CreatedAt:    srv.CreatedAt,
		UpdatedAt:    srv.UpdatedAt,
	}

	for _, sws := range servers {
		if sws.ID == serverID {
			result.Up = sws.Up
			result.Uptime30d = sws.Uptime30d
			result.LastCheckedAt = sws.LastCheckedAt
			result.Source = sws.Source
			result.ToolCount = sws.ToolCount
			break
		}
	}

	if result.Source == "" {
		result.Source, result.SourceURL = computeSource(store.ServerWithState{
			Description:  result.Description,
			OwnerContact: result.OwnerContact,
			Name:         result.Name,
		})
	}
	if result.ToolCount == 0 {
		result.ToolNames, result.ToolCount = computeToolInfo(result.Description)
	}

	if result.Capabilities == nil {
		result.Capabilities = []string{}
	}

	writeJSON(w, 200, result)
}

// PUT /v0/servers/{id} — update server manifest
func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	writeKey := extractWriteKey(r)

	if writeKey == "" || !s.Store.VerifyWriteKey(serverID, hashWriteKey(writeKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}

	existing, err := s.Store.GetServer(serverID)
	if err != nil {
		writeError(w, 404, "not_found", "server not found")
		return
	}

	// Apply updates
	updates := &store.Server{
		Name:         existing.Name,
		Description:  existing.Description,
		ServerURL:    existing.ServerURL,
		HealthURL:    existing.HealthURL,
		OwnerContact: existing.OwnerContact,
		ProbeMethod:  existing.ProbeMethod,
	}
	if req.Name != nil {
		updates.Name = *req.Name
	}
	if req.Description != nil {
		updates.Description = *req.Description
	}
	if req.ServerURL != nil {
		updates.ServerURL = *req.ServerURL
	}
	if req.HealthURL != nil {
		updates.HealthURL = *req.HealthURL
	}
	if req.OwnerContact != nil {
		updates.OwnerContact = *req.OwnerContact
	}
	if req.ProbeMethod != nil {
		updates.ProbeMethod = strings.ToUpper(*req.ProbeMethod)
	}

	if err := s.Store.UpdateServer(serverID, hashWriteKey(writeKey), updates); err != nil {
		writeError(w, 500, "db_error", "failed to update server")
		return
	}

	if req.Capabilities != nil {
		s.Store.SetCapabilities(serverID, *req.Capabilities)
	}

	writeJSON(w, 200, map[string]string{"status": "updated"})
}

// DELETE /v0/servers/{id} — delist a server
func (s *Server) handleDelistServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")

	// Check server exists first
	if _, err := s.Store.GetServer(serverID); err != nil {
		writeError(w, 404, "not_found", "server not found")
		return
	}

	writeKey := extractWriteKey(r)
	if writeKey == "" || !s.Store.VerifyWriteKey(serverID, hashWriteKey(writeKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	if err := s.Store.DelistServer(serverID, hashWriteKey(writeKey)); err != nil {
		writeError(w, 500, "db_error", "failed to delist server")
		return
	}

	events.Log(s.Store, "delist", map[string]any{
		"server_id": serverID,
		"actor":     "owner",
	}, r)

	writeJSON(w, 200, map[string]string{"status": "delisted"})
}

// GET /v0/resolve?capability=X — resolve capability
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	capability := r.URL.Query().Get("capability")
	if capability == "" {
		writeError(w, 400, "missing_param", "capability query parameter is required")
		return
	}

	servers, err := s.Store.GetUpServersByCapability(capability)
	if err != nil {
		writeError(w, 500, "db_error", "failed to resolve capability")
		return
	}

	events.Log(s.Store, "resolve", map[string]any{
		"capability":   capability,
		"result_count": len(servers),
	}, r)

	// Convert store.ServerWithState to api.ServerWithState
	result := make([]ServerWithState, len(servers))
	for i, sws := range servers {
		result[i] = storeServerWithStateToAPI(sws)
	}

	// Ensure we return [] not null
	if result == nil {
		result = []ServerWithState{}
	}

	writeJSON(w, 200, result)
}

// GET /v0/servers — list with filters and pagination
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	capability := r.URL.Query().Get("capability")
	status := r.URL.Query().Get("status")
	cursor := r.URL.Query().Get("cursor")

	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		if l > 100 {
			l = 100
		}
		limit = l
	}

	if status == "" {
		status = "active"
	}

	servers, nextCursor, err := s.Store.ListServers(q, capability, status, cursor, limit)
	if err != nil {
		writeError(w, 500, "db_error", "failed to list servers")
		return
	}

	// Convert to api types
	result := make([]ServerWithState, len(servers))
	for i, sws := range servers {
		result[i] = storeServerWithStateToAPI(sws)
	}

	if result == nil {
		result = []ServerWithState{}
	}

	writeJSON(w, 200, map[string]any{
		"servers":     result,
		"next_cursor": nextCursor,
	})
}

// GET /v0/export — full registry dump
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	export, err := s.Store.ExportAll()
	if err != nil {
		writeError(w, 500, "db_error", "failed to export registry")
		return
	}
	writeJSON(w, 200, export)
}

// GET /v0/stats — registry statistics
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.GetStats()
	if err != nil {
		writeError(w, 500, "db_error", "failed to get stats")
		return
	}
	writeJSON(w, 200, stats)
}

// init ensures time import is used (for landing page timestamp)
var _ = time.Now