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
	"github.com/trucore-ai/provengraph/internal/events"
	"github.com/trucore-ai/provengraph/internal/graph"
	"github.com/trucore-ai/provengraph/internal/store"
)

// --- Embedded content ---

//go:embed llms.txt
var llmsTxtContent string

//go:embed index.html
var landingPageHTML string

//go:embed docs.html
var docsPageHTML string

//go:embed agents.txt
var agentsTxtContent string

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
	CostPerCall  *float64 `json:"cost_per_call"` // USD per call; nil = undisclosed
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
	CostPerCall  *float64  `json:"cost_per_call,omitempty"` // USD per call; nil = no change
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
	// Computed fields (derived from existing data  -  not stored in DB)
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
	CostPerCall  float64 `json:"cost_per_call,omitempty"` // USD; -1 = undisclosed
	Provenance   *graph.TrustBreakdown `json:"provenance,omitempty"`
}

// --- Knowledge claim types ---

// CreateClaimRequest is the JSON body for POST /v0/knowledge.
type CreateClaimRequest struct {
	Content string `json:"content"`
	Domain  string `json:"domain"`
	Issuer  string `json:"issuer"`
}

// UpdateClaimRequest is the JSON body for PUT /v0/knowledge/{id}.
type UpdateClaimRequest struct {
	Content *string `json:"content,omitempty"`
	Domain  *string `json:"domain,omitempty"`
	Status  *string `json:"status,omitempty"`
}

// ClaimResponse is the public JSON shape for knowledge claim responses.
type ClaimResponse struct {
	ID         string                `json:"id"`
	Content    string                `json:"content"`
	Domain     string                `json:"domain"`
	Status     string                `json:"status"`
	Version    int                   `json:"version"`
	Issuer     string                `json:"issuer"`
	CreatedAt  string                `json:"created_at"`
	UpdatedAt  string                `json:"updated_at"`
	Provenance *graph.ClaimBreakdown `json:"provenance,omitempty"`
}

// CreateClaimResponse is returned on successful claim creation.
type CreateClaimResponse struct {
	ClaimID  string `json:"claim_id"`
	WriteKey string `json:"write_key"`
}

// SupersedeRequest asserts one claim supersedes another.
type SupersedeRequest struct {
	SupersedesID string `json:"supersedes_id"`
}

// ContradictRequest asserts one claim contradicts another.
type ContradictRequest struct {
	ContradictsID string `json:"contradicts_id"`
}

// AttestRequest attests to a claim.
type AttestRequest struct {
	Issuer string `json:"issuer"`
}

// --- Memory entry types ---

// CreateMemoryRequest is the JSON body for POST /v0/memory.
type CreateMemoryRequest struct {
	Content   string `json:"content"`
	Category  string `json:"category"`
	Retention string `json:"retention"`
	Purpose   string `json:"purpose"`
	Subject   string `json:"subject"`
	Owner     string `json:"owner"`
}

// UpdateMemoryRequest is the JSON body for PUT /v0/memory/{id}.
type UpdateMemoryRequest struct {
	Content   *string `json:"content,omitempty"`
	Category  *string `json:"category,omitempty"`
	Retention *string `json:"retention,omitempty"`
	Purpose   *string `json:"purpose,omitempty"`
}

// MemoryResponse is the public JSON shape for memory entry responses.
type MemoryResponse struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Category  string `json:"category"`
	Retention string `json:"retention"`
	Purpose   string `json:"purpose"`
	Consent   bool   `json:"consent"`
	Subject   string `json:"subject"`
	Owner     string `json:"owner"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CreateMemoryResponse is returned on successful memory creation.
type CreateMemoryResponse struct {
	MemoryID string `json:"memory_id"`
	WriteKey string `json:"write_key"`
}

// RememberRequest creates a remembers edge from an agent to a memory entry.
type RememberRequest struct {
	AgentID string `json:"agent_id"`
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

// computeToolInfo extracts tool names and count from description (Smithery format: "  -  tools: name1, name2, ...")
func computeToolInfo(desc string) ([]string, int) {
	if idx := strings.Index(desc, "  -  tools: "); idx >= 0 {
		toolsStr := desc[idx+len("  -  tools: "):]
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

	// Cost transparency (0-3): a DISCLOSED cost (cost_per_call >= 0, i.e. not the
	// -1 "undisclosed" sentinel) is a mild honesty signal  -  hidden costs are a
	// surprise-billing risk at runtime. The magnitude of cost deliberately does
	// NOT affect trust (expensive ≠ trustworthy); cost is surfaced for ranking,
	// not scored.
	if sws.CostPerCall >= 0 {
		score += 3
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
		CostPerCall:   sws.CostPerCall,
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
		"cost_per_call":   sws.CostPerCall,
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
//  -  this is the anti-gaming moat.
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

// GET /agents.txt  -  agent surface manifest
func (s *Server) handleAgentsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	w.Write([]byte(agentsTxtContent))
}

// GET /
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	// Don't serve the landing page for API paths
	if strings.HasPrefix(r.URL.Path, "/v0/") || r.URL.Path == "/health" || r.URL.Path == "/llms.txt" || r.URL.Path == "/docs" || r.URL.Path == "/agents.txt" {
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

// POST /v0/servers  -  register a server
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

	// Cost: nil = undisclosed (-1 sentinel); otherwise the disclosed USD value
	// (0 = free is valid and distinct from undisclosed).
	costPerCall := -1.0
	if req.CostPerCall != nil {
		costPerCall = *req.CostPerCall
		if costPerCall < 0 {
			costPerCall = -1.0
		}
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
		CostPerCall:  costPerCall,
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

// GET /v0/servers/{id}  -  get a single server by ID
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
		CostPerCall:  srv.CostPerCall,
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
			result.CostPerCall = sws.CostPerCall
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
		CostPerCall:  result.CostPerCall,
	}, result.Source)

	if result.Capabilities == nil {
		result.Capabilities = []string{}
	}
	result.Provenance = s.provenanceFor(serverID)

	writeJSON(w, 200, result)
}

// PUT /v0/servers/{id}  -  update server manifest
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
		CostPerCall:  existing.CostPerCall,
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
	if req.CostPerCall != nil {
		updates.CostPerCall = *req.CostPerCall
		if updates.CostPerCall < 0 {
			updates.CostPerCall = -1.0
		}
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

// DELETE /v0/servers/{id}  -  delist a server
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

// GET /v0/resolve?capability=X  -  resolve capability
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

// GET /v0/servers  -  list with filters and pagination
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

// GET /v0/export  -  full registry dump
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

// GET /v0/stats  -  registry statistics
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

// POST /v0/outcomes  -  record an agent-reported outcome ("did it work?").
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

// GET /v0/tools  -  list tools extracted from server descriptions.
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

// GET /v0/capabilities  -  list all capabilities with active server counts
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

// --- Knowledge claim handlers ---

func claimFromNode(n graph.Node) ClaimResponse {
	return ClaimResponse{
		ID:        n.ID,
		Content:   stringOr(n.Attrs, "content", ""),
		Domain:    stringOr(n.Attrs, "domain", ""),
		Status:    stringOr(n.Attrs, "status", "active"),
		Version:   intOr(n.Attrs, "version", 1),
		Issuer:    stringOr(n.Attrs, "issuer", ""),
		CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt,
	}
}

func memoryFromNode(n graph.Node) MemoryResponse {
	return MemoryResponse{
		ID:        n.ID,
		Content:   stringOr(n.Attrs, "content", ""),
		Category:  stringOr(n.Attrs, "category", ""),
		Retention: stringOr(n.Attrs, "retention", "permanent"),
		Purpose:   stringOr(n.Attrs, "purpose", ""),
		Consent:   boolOr(n.Attrs, "consent", false),
		Subject:   stringOr(n.Attrs, "subject", ""),
		Owner:     stringOr(n.Attrs, "owner", ""),
		CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt,
	}
}

func stringOr(attrs map[string]any, key, def string) string {
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return def
}

func intOr(attrs map[string]any, key string, def int) int {
	if v, ok := attrs[key].(float64); ok {
		return int(v)
	}
	return def
}

func boolOr(attrs map[string]any, key string, def bool) bool {
	if v, ok := attrs[key].(bool); ok {
		return v
	}
	return def
}

// POST /v0/knowledge  -  create a knowledge claim
func (s *Server) handleCreateClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	var req CreateClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}
	if req.Content == "" || req.Domain == "" {
		writeError(w, 422, "validation_error", "content and domain are required")
		return
	}

	claimID := "claim:" + uuid.New().String()
	writeKey := generateWriteKey()
	writeKeyHash := hashWriteKey(writeKey)

	attrs := map[string]any{
		"content": req.Content,
		"domain":  req.Domain,
		"status":  "active",
		"version": 1,
		"issuer":  req.Issuer,
	}
	if err := s.Graph.UpsertNode(claimID, graph.NodeKnowledgeClaim, attrs); err != nil {
		writeError(w, 500, "db_error", "failed to create claim")
		return
	}
	_ = s.Store.SetWriteKey(claimID, "knowledge", writeKeyHash)

	// If issuer is provided, also upsert the issuer org node and attestation edge
	if req.Issuer != "" {
		issuerID := "org:" + strings.ToLower(strings.TrimSpace(req.Issuer))
		existing, _ := s.Graph.GetNode(issuerID)
		if existing == nil {
			_ = s.Graph.UpsertNode(issuerID, graph.NodeOrg, map[string]any{"name": req.Issuer, "trust": 0.3})
		}
		_, _ = s.Graph.AddEdge(issuerID, claimID, graph.EdgeAttestsTo, "", "", req.Issuer, 1.0, nil)
	}

	writeJSON(w, 201, CreateClaimResponse{ClaimID: claimID, WriteKey: writeKey})
}

// GET /v0/knowledge/{id}  -  get a claim with provenance
func (s *Server) handleGetClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	n, err := s.Graph.GetNode(id)
	if err != nil {
		writeError(w, 404, "not_found", "claim not found")
		return
	}
	if n.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "not a knowledge claim")
		return
	}
	resp := claimFromNode(*n)
	if cb, err := s.Graph.ClaimScore(id); err == nil && cb.NumAttestations+cb.NumContradictions > 0 {
		resp.Provenance = &cb
	}
	writeJSON(w, 200, resp)
}

// GET /v0/knowledge  -  list/search claims
func (s *Server) handleListClaims(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	domain := r.URL.Query().Get("domain")
	query := r.URL.Query().Get("q")
	limit := 50

	var nodes []graph.Node
	var err error

	if query != "" {
		nodes, err = s.Graph.SearchClaims(query, limit)
	} else if domain != "" {
		nodes, err = s.Graph.ClaimsByDomain(domain, limit)
	} else {
		nodes, err = s.Graph.SearchClaims("", limit)
	}
	if err != nil {
		writeError(w, 500, "db_error", "failed to list claims")
		return
	}

	claims := make([]ClaimResponse, 0, len(nodes))
	for _, n := range nodes {
		c := claimFromNode(n)
		if cb, err := s.Graph.ClaimScore(n.ID); err == nil && cb.NumAttestations+cb.NumContradictions > 0 {
			c.Provenance = &cb
		}
		claims = append(claims, c)
	}
	writeJSON(w, 200, map[string]any{"claims": claims})
}

// PUT /v0/knowledge/{id}  -  update a claim (requires write key)
func (s *Server) handleUpdateClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	rawKey := extractWriteKey(r)
	if rawKey == "" || !s.Store.CheckWriteKey(id, "knowledge", hashWriteKey(rawKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "claim not found")
		return
	}

	var req UpdateClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}

	attrs := map[string]any{}
	if req.Content != nil {
		attrs["content"] = *req.Content
	}
	if req.Domain != nil {
		attrs["domain"] = *req.Domain
	}
	if req.Status != nil {
		attrs["status"] = *req.Status
	}
	if err := s.Graph.UpsertNode(id, graph.NodeKnowledgeClaim, attrs); err != nil {
		writeError(w, 500, "db_error", "failed to update claim")
		return
	}

	// Re-read the updated node
	n, _ = s.Graph.GetNode(id)
	resp := claimFromNode(*n)
	writeJSON(w, 200, resp)
}

// POST /v0/knowledge/{id}/supersede  -  assert this claim supersedes another
func (s *Server) handleSupersedeClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	rawKey := extractWriteKey(r)
	if rawKey == "" || !s.Store.CheckWriteKey(id, "knowledge", hashWriteKey(rawKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	var req SupersedeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SupersedesID == "" {
		writeError(w, 422, "validation_error", "supersedes_id is required")
		return
	}

	// Verify both nodes exist
	if _, err := s.Graph.GetNode(id); err != nil {
		writeError(w, 404, "not_found", "source claim not found")
		return
	}
	oldClaim, err := s.Graph.GetNode(req.SupersedesID)
	if err != nil || oldClaim.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "target claim not found")
		return
	}

	// Get the issuer from the source claim
	n, _ := s.Graph.GetNode(id)
	issuer := stringOr(n.Attrs, "issuer", "")

	_, err = s.Graph.AddEdge(id, req.SupersedesID, graph.EdgeSupersedes, "", "", issuer, 1.0, nil)
	if err != nil {
		writeError(w, 500, "db_error", "failed to create supersede edge")
		return
	}

	// Mark the old claim as superseded
	_ = s.Graph.UpsertNode(req.SupersedesID, graph.NodeKnowledgeClaim, map[string]any{"status": "superseded"})

	writeJSON(w, 200, map[string]any{"status": "ok", "supersedes": req.SupersedesID})
}

// POST /v0/knowledge/{id}/contradict  -  assert this claim contradicts another
func (s *Server) handleContradictClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	rawKey := extractWriteKey(r)
	if rawKey == "" || !s.Store.CheckWriteKey(id, "knowledge", hashWriteKey(rawKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	var req ContradictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ContradictsID == "" {
		writeError(w, 422, "validation_error", "contradicts_id is required")
		return
	}

	if _, err := s.Graph.GetNode(id); err != nil {
		writeError(w, 404, "not_found", "source claim not found")
		return
	}
	targetClaim, err := s.Graph.GetNode(req.ContradictsID)
	if err != nil || targetClaim.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "target claim not found")
		return
	}

	n, _ := s.Graph.GetNode(id)
	issuer := stringOr(n.Attrs, "issuer", "")

	_, err = s.Graph.AddEdge(id, req.ContradictsID, graph.EdgeContradicts, "", "", issuer, 1.0, nil)
	if err != nil {
		writeError(w, 500, "db_error", "failed to create contradict edge")
		return
	}

	writeJSON(w, 200, map[string]any{"status": "ok", "contradicts": req.ContradictsID})
}

// POST /v0/knowledge/{id}/attest  -  attest to a claim
func (s *Server) handleAttestClaim(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")

	var req AttestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Issuer == "" {
		writeError(w, 422, "validation_error", "issuer is required")
		return
	}

	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "claim not found")
		return
	}

	issuerID := "org:" + strings.ToLower(strings.TrimSpace(req.Issuer))
	existing, _ := s.Graph.GetNode(issuerID)
	if existing == nil {
		_ = s.Graph.UpsertNode(issuerID, graph.NodeOrg, map[string]any{"name": req.Issuer, "trust": 0.3})
	}
	_, err = s.Graph.AddEdge(issuerID, id, graph.EdgeAttestsTo, "", "", req.Issuer, 1.0, nil)
	if err != nil {
		writeError(w, 500, "db_error", "failed to create attestation")
		return
	}

	writeJSON(w, 200, map[string]any{"status": "ok", "claim_id": id, "attested_by": req.Issuer})
}

// GET /v0/knowledge/{id}/provenance  -  claim provenance breakdown
func (s *Server) handleClaimProvenance(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeKnowledgeClaim {
		writeError(w, 404, "not_found", "claim not found")
		return
	}
	cb, err := s.Graph.ClaimScore(id)
	if err != nil {
		writeError(w, 500, "db_error", "failed to compute claim score")
		return
	}
	writeJSON(w, 200, cb)
}

// --- Memory entry handlers ---

// POST /v0/memory  -  create a memory entry
func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	var req CreateMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}
	if req.Content == "" || req.Owner == "" {
		writeError(w, 422, "validation_error", "content and owner are required")
		return
	}
	if req.Retention == "" {
		req.Retention = "permanent"
	}
	if req.Category == "" {
		req.Category = "fact"
	}

	memoryID := "memory:" + uuid.New().String()
	writeKey := generateWriteKey()
	writeKeyHash := hashWriteKey(writeKey)

	attrs := map[string]any{
		"content":   req.Content,
		"category":  req.Category,
		"retention": req.Retention,
		"purpose":   req.Purpose,
		"consent":   true,
		"subject":   req.Subject,
		"owner":     req.Owner,
	}
	if err := s.Graph.UpsertNode(memoryID, graph.NodeMemoryEntry, attrs); err != nil {
		writeError(w, 500, "db_error", "failed to create memory entry")
		return
	}
	_ = s.Store.SetWriteKey(memoryID, "memory", writeKeyHash)

	// Also create a remembers edge from the owner
	ownerID := "agent:" + strings.ToLower(strings.TrimSpace(req.Owner))
	_ = s.Graph.UpsertNode(ownerID, graph.NodeAgent, map[string]any{"name": req.Owner})
	_, _ = s.Graph.AddEdge(ownerID, memoryID, graph.EdgeRemembers, "", "", req.Owner, 1.0, nil)

	writeJSON(w, 201, CreateMemoryResponse{MemoryID: memoryID, WriteKey: writeKey})
}

// GET /v0/memory/{id}  -  get a memory entry
func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	n, err := s.Graph.GetNode(id)
	if err != nil {
		writeError(w, 404, "not_found", "memory entry not found")
		return
	}
	if n.Type != graph.NodeMemoryEntry {
		writeError(w, 404, "not_found", "not a memory entry")
		return
	}
	writeJSON(w, 200, memoryFromNode(*n))
}

// GET /v0/memory  -  list/search memories
func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	agentID := r.URL.Query().Get("agent")
	category := r.URL.Query().Get("category")
	query := r.URL.Query().Get("q")
	limit := 50

	var nodes []graph.Node
	var err error

	if query != "" {
		nodes, err = s.Graph.SearchMemories(agentID, query, limit)
	} else if agentID != "" {
		nodes, err = s.Graph.MemoriesByAgent(agentID, category, limit)
	} else {
		nodes, err = s.Graph.SearchMemories("", "", limit)
	}
	if err != nil {
		writeError(w, 500, "db_error", "failed to list memories")
		return
	}

	memories := make([]MemoryResponse, 0, len(nodes))
	for _, n := range nodes {
		memories = append(memories, memoryFromNode(n))
	}
	writeJSON(w, 200, map[string]any{"memories": memories})
}

// PUT /v0/memory/{id}  -  update a memory entry (requires write key)
func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	rawKey := extractWriteKey(r)
	if rawKey == "" || !s.Store.CheckWriteKey(id, "memory", hashWriteKey(rawKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeMemoryEntry {
		writeError(w, 404, "not_found", "memory entry not found")
		return
	}

	var req UpdateMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid_json", "could not parse request body")
		return
	}

	attrs := map[string]any{}
	if req.Content != nil {
		attrs["content"] = *req.Content
	}
	if req.Category != nil {
		attrs["category"] = *req.Category
	}
	if req.Retention != nil {
		attrs["retention"] = *req.Retention
	}
	if req.Purpose != nil {
		attrs["purpose"] = *req.Purpose
	}
	if err := s.Graph.UpsertNode(id, graph.NodeMemoryEntry, attrs); err != nil {
		writeError(w, 500, "db_error", "failed to update memory entry")
		return
	}

	n, _ = s.Graph.GetNode(id)
	writeJSON(w, 200, memoryFromNode(*n))
}

// DELETE /v0/memory/{id}  -  delete a memory entry (right to be forgotten)
func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	rawKey := extractWriteKey(r)
	if rawKey == "" || !s.Store.CheckWriteKey(id, "memory", hashWriteKey(rawKey)) {
		writeError(w, 401, "unauthorized", "valid write key required")
		return
	}

	if err := s.Graph.DeleteMemoryEntry(id); err != nil {
		writeError(w, 500, "db_error", "failed to delete memory entry")
		return
	}
	_ = s.Store.DeleteWriteKey(id, "memory")
	writeJSON(w, 200, map[string]any{"status": "deleted", "id": id})
}

// POST /v0/memory/{id}/remember  -  agent remembers a memory entry
func (s *Server) handleRemember(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")

	var req RememberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgentID == "" {
		writeError(w, 422, "validation_error", "agent_id is required")
		return
	}

	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeMemoryEntry {
		writeError(w, 404, "not_found", "memory entry not found")
		return
	}

	agentID := "agent:" + strings.ToLower(strings.TrimSpace(req.AgentID))
	_ = s.Graph.UpsertNode(agentID, graph.NodeAgent, map[string]any{"name": req.AgentID})
	_, err = s.Graph.AddEdge(agentID, id, graph.EdgeRemembers, "", "", req.AgentID, 1.0, nil)
	if err != nil {
		writeError(w, 500, "db_error", "failed to create remembers edge")
		return
	}

	writeJSON(w, 200, map[string]any{"status": "ok", "memory_id": id, "agent_id": req.AgentID})
}

// DELETE /v0/memory/{id}/forget  -  agent forgets a memory entry
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if s.Graph == nil {
		writeError(w, 500, "unavailable", "ProvenGraph not initialized")
		return
	}
	id := r.PathValue("id")
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		writeError(w, 422, "validation_error", "?agent= query param is required")
		return
	}

	agentNodeID := "agent:" + strings.ToLower(strings.TrimSpace(agentID))

	// Verify memory entry exists
	n, err := s.Graph.GetNode(id)
	if err != nil || n.Type != graph.NodeMemoryEntry {
		writeError(w, 404, "not_found", "memory entry not found")
		return
	}

	// Find the remembers edge from this agent to this memory
	edges, err := s.Graph.EdgesFrom(agentNodeID, graph.EdgeRemembers)
	if err != nil {
		writeError(w, 500, "db_error", "failed to query edges")
		return
	}

	found := 0
	for _, e := range edges {
		if e.Dst == id {
			_ = s.Graph.DeleteEdge(e.ID)
			found++
		}
	}

	writeJSON(w, 200, map[string]any{"status": "forgotten", "memory_id": id, "agent_id": agentID, "edges_removed": found})
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

	// Pattern 1: Smithery "  -  tools: name1, name2 [...]"
	if idx := strings.Index(description, "  -  tools: "); idx >= 0 {
		rest := description[idx+len("  -  tools: "):]
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

// GET /v0/agent-manifest  -  JSON agent surface manifest for programmatic discovery
func (s *Server) handleAgentManifest(w http.ResponseWriter, r *http.Request) {
	manifest := map[string]any{
		"service":     "ProvenGraph",
		"version":     "v0",
		"base_url":    "https://provengraph.trucore.xyz",
		"health":      "/health",
		"docs_url":    "https://provengraph.trucore.xyz/docs",
		"agents_txt":  "/agents.txt",
		"source":      "https://github.com/trucore-ai/provengraph",
		"license":     "MIT",
		"auth": map[string]any{
			"method":           "write_key",
			"header":           "X-Write-Key or Authorization: Bearer",
			"scope":            "per-resource",
			"public_endpoints": "GET endpoints are unauthenticated",
			"write_endpoints":  "POST/PUT/DELETE require write_key",
		},
		"pillars": map[string]any{
			"trust": map[string]any{
				"description": "MCP server registry with provenance-weighted trust scores",
				"purpose":     "Eliminate hardcoded server URLs. Agents resolve capabilities against live, probed endpoints ranked by uptime and outcome-verified reputation.",
				"endpoints": map[string]any{
					"register":       "POST /v0/servers {name, server_url, capabilities, health_url?, probe_method?} -> {server_id, write_key}",
					"resolve":        "GET /v0/resolve?capability=<name> -> [{server_id, name, server_url, uptime_30d, trust_score, trust_tier}]",
					"report_outcome": "POST /v0/outcomes {server_id, success: bool, rating: 0-5, reporter}",
					"list":           "GET /v0/servers?capability=X&query=Y&status=active",
					"get":            "GET /v0/servers/{id}",
					"update":         "PUT /v0/servers/{id} (write key required)",
					"delist":         "DELETE /v0/servers/{id} (write key required)",
					"stats":          "GET /v0/stats",
					"export":         "GET /v0/export",
					"capabilities":   "GET /v0/capabilities",
					"tools":          "GET /v0/tools",
				},
				"trust_scoring": map[string]any{
					"model":             "provenance_weighted",
					"reliability":       "0-55 (30-day uptime)",
					"latency":           "0-20 (response time)",
					"provenance":        "0-25 (attester trust + outcome reputation)",
					"outcome_moat":      true,
					"moat_description":  "Reporter's own trust weights the outcome report  -  throwaway agents can't pump scores",
					"tiers":             map[string]string{"verified": ">=85", "trusted": ">=70", "provisional": ">=50", "untrusted": "<50"},
				},
			},
			"knowledge": map[string]any{
				"description": "Verifiable knowledge claims with attestation-scored provenance",
				"purpose":     "Build a shared knowledge base where truth is attested, not asserted. Every claim carries who vouched, who contradicted, and whether it's been superseded.",
				"endpoints": map[string]any{
					"create":      "POST /v0/knowledge {content, domain, issuer?} -> {claim_id, write_key}",
					"get":         "GET /v0/knowledge/{id} -> claim with ClaimBreakdown",
					"list":        "GET /v0/knowledge?domain=X&q=Y -> {claims: [...]}",
					"update":      "PUT /v0/knowledge/{id} {content?, domain?, status?} (write key required)",
					"supersede":   "POST /v0/knowledge/{id}/supersede {supersedes_id} (write key required)",
					"contradict":  "POST /v0/knowledge/{id}/contradict {contradicts_id} (write key required)",
					"attest":      "POST /v0/knowledge/{id}/attest {issuer} (no auth required)",
					"provenance":  "GET /v0/knowledge/{id}/provenance -> ClaimBreakdown",
				},
				"claim_scoring": map[string]any{
					"attestation_score":     "0-30 (attester trust * freshness, diminishing returns)",
					"freshness_score":       "0-15 (average attestation freshness)",
					"contradiction_penalty": "0-15 (3 pts per inbound contradict edge)",
					"supersession_penalty":  "0-15 (binary: superseded or not)",
					"total":                 "0-100",
				},
			},
			"memory": map[string]any{
				"description": "Purpose-tagged agent memory with compliance metadata and retention policies",
				"purpose":     "Agents need searchable, auditable, deletable memory. Every entry records why it exists, how long it lives, who it concerns, and whether consent was obtained.",
				"endpoints": map[string]any{
					"create":   "POST /v0/memory {content, owner, category?, retention?, purpose?, subject?} -> {memory_id, write_key}",
					"get":      "GET /v0/memory/{id}",
					"list":     "GET /v0/memory?agent=X&category=Y&q=Z -> {memories: [...]}",
					"update":   "PUT /v0/memory/{id} {content?, category?, retention?, purpose?} (write key required)",
					"delete":   "DELETE /v0/memory/{id} (write key required)  -  right to be forgotten",
					"remember": "POST /v0/memory/{id}/remember {agent_id}",
					"forget":   "DELETE /v0/memory/{id}/forget?agent=X",
				},
				"compliance": map[string]any{
					"retention":            []string{"permanent", "session", "30d", "1y"},
					"categories":           []string{"fact", "preference", "interaction", "compliance_record"},
					"consent_tracked":      true,
					"purpose_required":     true,
					"right_to_be_forgotten": "DELETE /v0/memory/{id}",
				},
			},
		},
		"sdks": map[string]any{
			"python": map[string]any{
				"install":           "pip install meshdns-client",
				"import":            "from meshdns_client import MeshDNS",
				"constructor":       "MeshDNS(base_url=\"https://provengraph.trucore.xyz\")",
				"trust_methods":     []string{"resolve", "list_servers", "get_server", "register_server", "update_server", "delist_server", "report_outcome"},
				"knowledge_methods": []string{"create_claim", "get_claim", "list_claims", "update_claim", "supersede_claim", "contradict_claim", "attest_claim"},
				"memory_methods":    []string{"create_memory", "get_memory", "list_memories", "update_memory", "delete_memory", "remember", "forget"},
			},
			"typescript": map[string]any{
				"install":           "npm i @meshdns/client",
				"import":            "import { MeshDNS } from \"@meshdns/client\"",
				"constructor":       "new MeshDNS(\"https://provengraph.trucore.xyz\")",
				"trust_methods":     []string{"resolve", "listServers", "getServer", "registerServer", "updateServer", "delistServer", "reportOutcome"},
				"knowledge_methods": []string{"createClaim", "getClaim", "listClaims", "updateClaim", "supersedeClaim", "contradictClaim", "attestClaim"},
				"memory_methods":    []string{"createMemory", "getMemory", "listMemories", "updateMemory", "deleteMemory", "remember", "forget"},
			},
		},
		"cli": map[string]any{
			"binary":              "provengraph",
			"install":             "go install github.com/trucore-ai/provengraph/cmd/provengraph@latest",
			"global_flags":        []string{"--json", "--base-url"},
			"trust_commands":      []string{"register", "list", "resolve", "stats", "status", "capabilities", "delist", "doctor", "provenance-sync"},
			"knowledge_commands":  []string{"knowledge-create", "knowledge-get", "knowledge-list", "knowledge-supersede", "knowledge-contradict", "knowledge-attest"},
			"memory_commands":     []string{"memory-create", "memory-get", "memory-list", "memory-remember", "memory-forget", "memory-delete"},
		},
		"llms_txt": "/llms.txt",
	}
	writeJSON(w, 200, manifest)
}

// init ensures time import is used (for landing page timestamp)
var _ = time.Now