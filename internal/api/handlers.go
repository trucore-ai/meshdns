package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/graph"
	"github.com/trucore-ai/meshdns/internal/store"
)

// --- Embedded content ---

//go:embed llms.txt
var llmsTxtContent string

//go:embed index.html
var landingPageHTML string

//go:embed docs.html
var docsPageHTML string

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
	AvgLatencyMs int     `json:"avg_latency_ms,omitempty"`
	TrustScore  float64  `json:"trust_score,omitempty"`
	TrustTier   string   `json:"trust_tier,omitempty"`
	Verified    bool     `json:"verified,omitempty"`
	OutcomeCount int     `json:"outcome_count,omitempty"`
	OutcomeRate  float64 `json:"outcome_success_rate,omitempty"`
	Provenance   *graph.TrustBreakdown `json:"provenance,omitempty"`
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

// computeTrust derives a 0-100 trust score and tier from live state.
//
// Model (documented so agents can reason about it):
//   - Reliability (0-55): 30-day uptime ratio * 55
//   - Latency (0-20): 0ms -> 20 pts, >=1500ms -> 0; unknown -> neutral 10
//   - Provenance (0-20): known catalogs (MCP Registry/Smithery/npm) -> 20, other named source -> 15, manual -> 5
//   - Richness (0-5): discoverable tool_count > 0 -> 5
//
// First-party TruCore servers are fully verified (100). A currently-DOWN server
// is capped at 40 regardless of history.
// `source` is the resolved provenance label (already passed through computeSource).
func computeTrust(sws store.ServerWithState, source string) (float64, string, bool) {
	if source == "TruCore" {
		return 100.0, "verified", true
	}

	score := sws.Uptime30d * 55

	if sws.AvgLatencyMs > 0 {
		lat := 1.0 - float64(sws.AvgLatencyMs)/1500.0
		if lat < 0 {
			lat = 0
		}
		score += lat * 20
	} else {
		score += 10
	}

	src := strings.ToLower(source)
	switch {
	case strings.Contains(src, "mcp registry"), strings.Contains(src, "smithery"), strings.Contains(src, "npm"):
		score += 20
	case src != "" && src != "manual" && src != "custom":
		score += 15
	default:
		score += 5
	}

	if sws.ToolCount > 0 {
		score += 5
	}

	// Outcome verification (the trust moat): agent-reported "did it work?"
	// results. Requires >= 3 reports to be statistically meaningful; a perfect
	// success rate adds up to +5 (total is capped at 100 below).
	if sws.OutcomeCount >= 3 {
		score += sws.OutcomeRate * 5
	}

	if sws.Up == 0 && score > 40 {
		score = 40
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	score = math.Round(score*10) / 10

	switch {
	case score >= 85:
		return score, "verified", true
	case score >= 70:
		return score, "trusted", false
	case score >= 50:
		return score, "provisional", false
	default:
		return score, "untrusted", false
	}
}

func (s *Server) storeServerWithStateToAPI(sws store.ServerWithState) ServerWithState {
	source, sourceURL := computeSource(sws)
	if sws.Source != "" {
		source = sws.Source
	}
	toolNames, toolCount := computeToolInfo(sws.Description)
	if sws.ToolCount > 0 {
		toolCount = sws.ToolCount
	}
	curlSnippet := computeCurlSnippet(sws.ServerURL)
	trustScore, trustTier, verified := computeTrust(sws, source)
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
		AvgLatencyMs:  sws.AvgLatencyMs,
		TrustScore:    trustScore,
		TrustTier:     trustTier,
		Verified:      verified,
		OutcomeCount:  sws.OutcomeCount,
		OutcomeRate:   sws.OutcomeRate,
		Provenance:    s.provenanceFor(sws.ID),
	}
}

// provenanceFor reads the ProvenGraph core for a service's provenance breakdown.
// Returns nil when the graph has no data for it (so the field stays omitted).
func (s *Server) provenanceFor(id string) *graph.TrustBreakdown {
	if s.Graph == nil {
		return nil
	}
	gb, err := s.Graph.TrustScore(id)
	if err != nil {
		return nil
	}
	if gb.NumAttestations == 0 && gb.NumOutcomes == 0 && gb.NumContradictions == 0 {
		return nil
	}
	return &gb
}

// syncServerToGraph writes a service + its issuing org + attestation edge into
// the provenance graph. Idempotent (upsert + deterministic attestation edges).
// The flat trust score stays authoritative for now; the graph is the provenance
// source of truth that the trust score will read once backfilled + validated.
func (s *Server) syncServerToGraph(sws store.ServerWithState) {
	if s.Graph == nil {
		return
	}
	attrs := map[string]any{
		"name":            sws.Name,
		"server_url":      sws.ServerURL,
		"capabilities":    sws.Capabilities,
		"uptime_30d":      sws.Uptime30d,
		"avg_latency_ms":  sws.AvgLatencyMs,
		"source":          sws.Source,
		"owner_contact":   sws.OwnerContact,
	}
	_ = s.Graph.UpsertNode(sws.ID, graph.NodeService, attrs)

	// Issuer = provenance source (catalog), falling back to owner contact.
	src, _ := computeSource(sws)
	if sws.Source != "" {
		src = sws.Source
	}
	issuer := src
	if issuer == "" || issuer == "manual" {
		issuer = sws.OwnerContact
	}
	if issuer == "" {
		issuer = "unknown"
	}

	// Known catalogs + first-party are trusted issuers; "manual"/unknown are not.
	orgTrust := 0.3
	switch strings.ToLower(issuer) {
	case "trucore", "mcp registry", "smithery", "npm":
		orgTrust = 1.0
	}

	orgID := "org:" + strings.ToLower(strings.TrimSpace(issuer))
	_ = s.Graph.UpsertNode(orgID, graph.NodeOrg, map[string]any{"name": issuer, "trust": orgTrust})
	_, _ = s.Graph.AddEdge(orgID, sws.ID, graph.EdgeAttestsTo, "", "", issuer, 1.0, nil)
}

// SyncAllToGraph backfills every active server into the provenance graph.
// Used by the provenance-sync command (one-time migration) and safe to re-run
// (upserts + deterministic attestation edges are idempotent).
func SyncAllToGraph(s *store.Store, g *graph.Graph) (int, error) {
	srv := &Server{Store: s, Graph: g}
	servers, _, err := s.ListServers("", "", "active", "", 0)
	if err != nil {
		return 0, err
	}
	for _, sws := range servers {
		srv.syncServerToGraph(sws)
	}
	return len(servers), nil
}

// syncOutcomeToGraph records an outcome report as an observed-by edge from the
// reporter agent. The reporter's own trust (set separately) weights the outcome
// — this is the anti-gaming moat.
func (s *Server) syncOutcomeToGraph(serverID string, success bool, rating int, reporter string) {
	if s.Graph == nil {
		return
	}
	reporterID := reporter
	if reporterID == "" {
		reporterID = "unknown"
	}
	reporterID = "agent:" + strings.ToLower(strings.TrimSpace(reporterID))
	// Ensure the reporter node exists (default trust 0.3; first-party verifier higher).
	_ = s.Graph.UpsertNode(reporterID, graph.NodeAgent, map[string]any{"name": reporter})
	succ := 0.0
	if success {
		succ = 1.0
	}
	_, _ = s.Graph.AddEdge(reporterID, serverID, graph.EdgeObservedBy, "", "", reporter, 1.0, map[string]any{
		"success": succ,
		"rating":  rating,
	})
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
	if strings.HasPrefix(r.URL.Path, "/v0/") || r.URL.Path == "/health" || r.URL.Path == "/llms.txt" || r.URL.Path == "/docs" {
		writeError(w, 404, "not_found", "endpoint not found")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(landingPageHTML))
}

// GET /docs
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(docsPageHTML))
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

	// Sync to the ProvenGraph core (service node + org + attestation edge).
	s.syncServerToGraph(store.ServerWithState{
		ID:           serverID,
		Name:         req.Name,
		Description:  req.Description,
		ServerURL:    req.ServerURL,
		Capabilities: req.Capabilities,
		OwnerContact: req.OwnerContact,
	})

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
			result.AvgLatencyMs = sws.AvgLatencyMs
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
	result.CurlSnippet = computeCurlSnippet(result.ServerURL)
	result.TrustScore, result.TrustTier, result.Verified = computeTrust(store.ServerWithState{
		Up:           result.Up,
		Uptime30d:    result.Uptime30d,
		AvgLatencyMs: result.AvgLatencyMs,
		ToolCount:    result.ToolCount,
	}, result.Source)

	if result.Capabilities == nil {
		result.Capabilities = []string{}
	}
	result.Provenance = s.provenanceFor(serverID)

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
		result[i] = s.storeServerWithStateToAPI(sws)
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
		result[i] = s.storeServerWithStateToAPI(sws)
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
	// Run every server through the API conversion so computed fields
	// (source, auth, tool_names, curl_snippet, trust_score, trust_tier) are present.
	converted := make([]ServerWithState, len(export.Servers))
	for i, sws := range export.Servers {
		converted[i] = s.storeServerWithStateToAPI(sws)
	}
	if converted == nil {
		converted = []ServerWithState{}
	}
	writeJSON(w, 200, map[string]any{
		"exported_at": export.ExportedAt,
		"servers":     converted,
	})
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

// OutcomeReportRequest is the JSON body for POST /v0/outcomes.
type OutcomeReportRequest struct {
	ServerID string `json:"server_id"`
	Success  bool   `json:"success"`
	Rating   int    `json:"rating"`
	Reporter string `json:"reporter"`
}

// POST /v0/outcomes — record an agent-reported outcome ("did it work?").
// This is the trust moat: requester-verified success signals that feed the
// outcome-verification term in computeTrust.
func (s *Server) handleReportOutcome(w http.ResponseWriter, r *http.Request) {
	var req OutcomeReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}
	if req.ServerID == "" {
		writeError(w, 422, "validation_error", "server_id is required")
		return
	}
	if req.Rating < 0 || req.Rating > 5 {
		writeError(w, 422, "validation_error", "rating must be 0-5")
		return
	}
	if _, err := s.Store.GetServer(req.ServerID); err != nil {
		writeError(w, 404, "not_found", "server not found")
		return
	}
	if err := s.Store.RecordOutcome(req.ServerID, req.Success, req.Rating, req.Reporter); err != nil {
		writeError(w, 500, "db_error", "failed to record outcome")
		return
	}
	// Sync the outcome to the provenance graph (observed-by edge, the moat).
	s.syncOutcomeToGraph(req.ServerID, req.Success, req.Rating, req.Reporter)
	stats, _ := s.Store.GetOutcomeStats(req.ServerID)
	writeJSON(w, 201, map[string]any{"status": "recorded", "outcome": stats})
}

// --- Tool / capabilities endpoints ---

type toolEntry struct {
	Name         string   `json:"name"`
	ServerName   string   `json:"server_name"`
	ServerURL    string   `json:"server_url"`
	ServerID     string   `json:"server_id"`
	Uptime30d    float64  `json:"uptime_30d"`
	Up           int      `json:"up"`
	Capabilities []string `json:"capabilities"`
	Source       string   `json:"source,omitempty"`
	OwnerContact string   `json:"owner_contact,omitempty"`
	TrustScore   float64  `json:"trust_score,omitempty"`
	TrustTier    string   `json:"trust_tier,omitempty"`
	Verified     bool     `json:"verified,omitempty"`
	CurlSnippet  string   `json:"curl_snippet"`
}

// GET /v0/tools — list tools extracted from server descriptions.
// Supports ?query=, ?capability=, and ?limit= filters.
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	capability := strings.TrimSpace(r.URL.Query().Get("capability"))
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
		if limit > 200 {
			limit = 200
		}
	}

	// Fetch all active servers; filter by capability in SQL when provided.
	servers, _, err := s.Store.ListServers(query, capability, "active", "", 0)
	if err != nil {
		writeError(w, 500, "db_error", "failed to list servers")
		return
	}

	type toolWithServer struct {
		tool  string
		entry toolEntry
	}
	var tools []toolWithServer
	seen := make(map[string]struct{})

	for _, server := range servers {
		names := extractToolNames(server.Description)
		for _, name := range names {
			if query != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(query)) {
				continue
			}
			key := server.ID + ":" + name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			curl := ""
			if server.ServerURL != "" {
				curl = fmt.Sprintf(
					`python3 ~/repo/hermes-trading/.hermes/scripts/lazymcp.py call --url %s --tool %s --args '{"key":"value"}'`,
					server.ServerURL, name)
			}

			capCopy := make([]string, len(server.Capabilities))
			copy(capCopy, server.Capabilities)

			serverSource, _ := computeSource(server)
			if server.Source != "" {
				serverSource = server.Source
			}
			trustScore, trustTier, verified := computeTrust(server, serverSource)

			tools = append(tools, toolWithServer{
				tool: name,
				entry: toolEntry{
					Name:         name,
					ServerName:   server.Name,
					ServerURL:    server.ServerURL,
					ServerID:     server.ID,
					Uptime30d:    server.Uptime30d,
					Up:           server.Up,
					Capabilities: capCopy,
					Source:       serverSource,
					OwnerContact: server.OwnerContact,
					TrustScore:   trustScore,
					TrustTier:    trustTier,
					Verified:     verified,
					CurlSnippet:  curl,
				},
			})
		}
	}

	sort.Slice(tools, func(i, j int) bool {
		return strings.ToLower(tools[i].tool) < strings.ToLower(tools[j].tool)
	})

	if len(tools) > limit {
		tools = tools[:limit]
	}

	entries := make([]toolEntry, len(tools))
	for i, t := range tools {
		entries[i] = t.entry
	}

	writeJSON(w, 200, map[string]any{"tools": entries})
}

// GET /v0/capabilities — list all capabilities with active server counts
func (s *Server) handleListCapabilities(w http.ResponseWriter, r *http.Request) {
	caps, err := s.Store.ListCapabilities()
	if err != nil {
		writeError(w, 500, "db_error", "failed to list capabilities")
		return
	}
	if caps == nil {
		caps = []store.CapabilityInfo{}
	}
	writeJSON(w, 200, map[string]any{"capabilities": caps})
}

// extractToolNames parses tool names from a server description using multiple patterns.
func extractToolNames(description string) []string {
	seen := make(map[string]struct{})
	var names []string

	addNames := func(raw string) {
		for _, n := range strings.Split(raw, ",") {
			n = strings.TrimSpace(n)
			if idx := strings.IndexByte(n, '['); idx >= 0 {
				n = strings.TrimSpace(n[:idx])
			}
			if n == "" {
				continue
			}
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				names = append(names, n)
			}
		}
	}

	// Pattern 1: Smithery " — tools: name1, name2 [...]"
	if idx := strings.Index(description, " — tools: "); idx >= 0 {
		rest := description[idx+len(" — tools: "):]
		if end := strings.IndexByte(rest, '['); end >= 0 {
			rest = rest[:end]
		}
		addNames(rest)
	}

	// Pattern 2: "Provides:" or "provides:" followed by a list
	for _, prefix := range []string{"Provides: ", "provides: "} {
		if idx := strings.Index(description, prefix); idx >= 0 {
			rest := description[idx+len(prefix):]
			if end := strings.Index(rest, "\n"); end >= 0 {
				rest = rest[:end]
			}
			addNames(rest)
		}
	}

	// Pattern 3: "Tools:" at the start of a line
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Tools:") || strings.HasPrefix(line, "tools:") {
			addNames(strings.TrimPrefix(strings.TrimPrefix(line, "Tools:"), "tools:"))
		}
	}

	// Pattern 4: Comma-separated list in parentheses after "tools"
	if idx := strings.Index(description, "tools ("); idx >= 0 {
		rest := description[idx+len("tools ("):]
		if end := strings.IndexByte(rest, ')'); end >= 0 {
			addNames(rest[:end])
		}
	}

	return names
}

// init ensures time import is used (for landing page timestamp)
var _ = time.Now