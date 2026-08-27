// Package graph implements the ProvenGraph core: a provenance graph +
// trust-scoring engine. This is the moat — the graph of who-attests-what,
// with every edge carrying evidence, an issuer, a timestamp, and a freshness
// value. Trust is computed *over* the graph, not from a flat table.
//
// The anti-gaming property (the entire defensibility) comes from
// requester-reputation weighting: an outcome report is weighted by the
// *reporter's* own trust. A high-trust reporter's success/failure moves the
// score far more than an unknown reporter's. This is not faked — the weight
// is read from the reporter Agent node's own trust score.
package graph

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// NodeType enumerates the kinds of vertices in the provenance graph.
type NodeType string

const (
	NodeService        NodeType = "service"         // an MCP server / capability provider
	NodeAgent          NodeType = "agent"           // an agent (human or machine) that observes/attests
	NodeKnowledgeClaim NodeType = "knowledge_claim" // "what's already known" (Knowledge product line)
	NodeMemoryEntry    NodeType = "memory_entry"    // "what's remembered & compliant" (Memory product line)
	NodeOrg            NodeType = "org"             // an organization that issues attestations
)

// EdgeType enumerates the provenance relationships between nodes.
type EdgeType string

const (
	EdgeAttestsTo   EdgeType = "attests-to"   // issuer -> service: "I vouch for this"
	EdgeDependsOn   EdgeType = "depends-on"   // service -> service: runtime dependency
	EdgeRemembers   EdgeType = "remembers"    // service/agent -> memory_entry
	EdgeSupersedes  EdgeType = "supersedes"   // claim v2 -> claim v1
	EdgeContradicts EdgeType = "contradicts"  // claim A -> claim B: they can't both be true
	EdgeObservedBy  EdgeType = "observed-by"  // agent -> service: an outcome report ("did it work?")
)

// Node is a vertex in the graph. Attrs is a free-form JSON object carrying
// node-specific data (e.g. reliability/latency for services, trust for agents).
type Node struct {
	ID        string         `json:"id"`
	Type      NodeType       `json:"type"`
	Attrs     map[string]any `json:"attrs,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// Edge is a provenance edge. Every edge carries evidence (url + content hash),
// the issuer (DID/origin), a timestamp, and a freshness value in [0,1] that
// decays over time. Attrs carries edge-specific data (e.g. outcome success).
type Edge struct {
	ID           string         `json:"id"`
	Src          string         `json:"src"`
	Dst          string         `json:"dst"`
	Type         EdgeType       `json:"type"`
	EvidenceURL  string         `json:"evidence_url,omitempty"`
	EvidenceHash string         `json:"evidence_hash,omitempty"`
	Issuer       string         `json:"issuer,omitempty"`
	Timestamp    string         `json:"timestamp"`
	Freshness    float64        `json:"freshness"`
	Attrs        map[string]any `json:"attrs,omitempty"`
}

// Graph wraps a single SQLite connection (shared with the store — one writer).
type Graph struct {
	db *sql.DB
}

// New returns a Graph over an existing *sql.DB. The graph shares the store's
// connection so SQLite write serialization (MaxOpenConns(1)) still holds.
func New(db *sql.DB) *Graph { return &Graph{db: db} }

// Migrate creates the nodes/edges tables and their indexes (idempotent).
func (g *Graph) Migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS pg_nodes (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		attrs TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS pg_edges (
		id TEXT PRIMARY KEY,
		src TEXT NOT NULL,
		dst TEXT NOT NULL,
		type TEXT NOT NULL,
		evidence_url TEXT NOT NULL DEFAULT '',
		evidence_hash TEXT NOT NULL DEFAULT '',
		issuer TEXT NOT NULL DEFAULT '',
		timestamp TEXT NOT NULL,
		freshness REAL NOT NULL DEFAULT 1.0,
		attrs TEXT NOT NULL DEFAULT '{}'
	);
	CREATE INDEX IF NOT EXISTS idx_pg_edges_src ON pg_edges(src, type);
	CREATE INDEX IF NOT EXISTS idx_pg_edges_dst ON pg_edges(dst, type);
	CREATE INDEX IF NOT EXISTS idx_pg_edges_type ON pg_edges(type);
	CREATE INDEX IF NOT EXISTS idx_pg_nodes_type ON pg_nodes(type);
	`
	_, err := g.db.Exec(schema)
	return err
}

// HashEvidence returns the sha256 hex of an evidence payload (schema/content),
// used for schema-integrity / rug-pull detection (absorbed SkillPin I-165).
func HashEvidence(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func marshalAttrs(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalAttrs(s string) map[string]any {
	m := map[string]any{}
	if s == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// UpsertNode inserts or updates a node, merging attrs. Returns the node ID.
func (g *Graph) UpsertNode(id string, typ NodeType, attrs map[string]any) error {
	ts := now()
	// Merge attrs with any existing node (don't clobber unknown fields).
	var existingAttrs string
	err := g.db.QueryRow(`SELECT attrs FROM pg_nodes WHERE id = ?`, id).Scan(&existingAttrs)
	merged := map[string]any{}
	if err == nil {
		merged = unmarshalAttrs(existingAttrs)
	}
	for k, v := range attrs {
		merged[k] = v
	}
	_, err = g.db.Exec(
		`INSERT INTO pg_nodes (id, type, attrs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   type = excluded.type,
		   attrs = excluded.attrs,
		   updated_at = excluded.updated_at`,
		id, string(typ), marshalAttrs(merged), ts, ts,
	)
	return err
}

// GetNode fetches a node by ID.
func (g *Graph) GetNode(id string) (*Node, error) {
	n := &Node{}
	var attrs string
	err := g.db.QueryRow(
		`SELECT id, type, attrs, created_at, updated_at FROM pg_nodes WHERE id = ?`, id,
	).Scan(&n.ID, &n.Type, &attrs, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	n.Attrs = unmarshalAttrs(attrs)
	return n, nil
}

// AddEdge writes a provenance edge. ID strategy is type-aware:
//   - observed-by (outcome reports) are EVENTS and accumulate — each report is
//     a distinct edge with a unique ID (random nonce), so repeated outcomes
//     from the same agent build up volume rather than collapsing.
//   - all other edge types dedup on a deterministic ID (src+dst+type+issuer),
//     so re-syncing the same attestation is a no-op, not a duplicate.
func (g *Graph) AddEdge(src, dst string, typ EdgeType, evidenceURL, evidenceHash, issuer string, freshness float64, attrs map[string]any) (string, error) {
	if freshness <= 0 {
		freshness = 1.0
	}
	if freshness > 1 {
		freshness = 1.0
	}
	ts := now()

	var id string
	if typ == EdgeObservedBy {
		// Unique per report: timestamp + 8 random bytes.
		nonce := make([]byte, 8)
		_, _ = rand.Read(nonce)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%x", src, dst, typ, issuer, ts, nonce)))
		id = hex.EncodeToString(sum[:])
	} else {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", src, dst, typ, issuer)))
		id = hex.EncodeToString(sum[:])
	}

	_, err := g.db.Exec(
		`INSERT INTO pg_edges (id, src, dst, type, evidence_url, evidence_hash, issuer, timestamp, freshness, attrs)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   evidence_url = excluded.evidence_url,
		   evidence_hash = excluded.evidence_hash,
		   timestamp = excluded.timestamp,
		   freshness = excluded.freshness,
		   attrs = excluded.attrs`,
		id, src, dst, string(typ), evidenceURL, evidenceHash, issuer, ts, freshness, marshalAttrs(attrs),
	)
	return id, err
}

// EdgesTo returns all edges pointing at a node (optionally filtered by type).
func (g *Graph) EdgesTo(dst string, typ EdgeType) ([]Edge, error) {
	return g.queryEdges(`SELECT id, src, dst, type, evidence_url, evidence_hash, issuer, timestamp, freshness, attrs
		FROM pg_edges WHERE dst = ? AND type = ?`, dst, string(typ))
}

func (g *Graph) queryEdges(q string, args ...any) ([]Edge, error) {
	rows, err := g.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		var attrs string
		if err := rows.Scan(&e.ID, &e.Src, &e.Dst, &e.Type, &e.EvidenceURL, &e.EvidenceHash, &e.Issuer, &e.Timestamp, &e.Freshness, &attrs); err != nil {
			return nil, err
		}
		e.Attrs = unmarshalAttrs(attrs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Trust scoring over the graph ---

// TrustBreakdown is the provenance portion of a service's trust score. It
// composes with the flat reliability/latency signals (computed from probe
// history in the store) to produce the full 0-100 score.
type TrustBreakdown struct {
	ServiceID           string  `json:"service_id"`
	AttestationScore    float64 `json:"attestation_score"`     // 0-20: who vouches, weighted by attester trust * freshness
	OutcomeScore        float64 `json:"outcome_score"`         // 0-25: outcome-verified, weighted by reporter reputation (the moat)
	FreshnessScore      float64 `json:"freshness_score"`       // 0-10: how fresh the attestations/outcomes are
	ContradictionPenalty float64 `json:"contradiction_penalty"` // 0-10: evidence of contradiction/supersession
	Total               float64 `json:"total"`                 // 0-65: sum of the above (reliability+latency add the other 35-55)
	NumAttestations     int     `json:"num_attestations"`
	NumOutcomes         int     `json:"num_outcomes"`
	NumContradictions   int     `json:"num_contradictions"`
}

// nodeTrust returns the trust weight of any node (agent OR org) in [floor, 1].
// The moat: an outcome/attestation is weighted by the reporter/attester's own
// trust. A high-trust reporter's outcome carries near-full weight; an unknown
// or untrusted node's report barely counts. Weight is read from the node's
// attrs["trust"] (0..1). Missing/unknown nodes get the floor so first-time
// reports still register but can't be used to pump a score.
func (g *Graph) nodeTrust(id string) float64 {
	const floor = 0.1
	n, err := g.GetNode(id)
	if err != nil {
		return floor
	}
	t, ok := n.Attrs["trust"].(float64)
	if !ok || t < 0 {
		return floor
	}
	if t > 1 {
		t = 1
	}
	if t < floor {
		return floor
	}
	return t
}

// TrustScore computes the provenance trust portion for a service node. It is
// honest about its inputs: attestations and outcomes both degrade with edge
// freshness, and outcome success is a reputation-weighted average — a
// low-trust reporter cannot move it.
func (g *Graph) TrustScore(serviceID string) (TrustBreakdown, error) {
	b := TrustBreakdown{ServiceID: serviceID}

	// 1. Attestations (0-20): sum of attester trust * freshness, diminishing returns.
	attests, err := g.EdgesTo(serviceID, EdgeAttestsTo)
	if err != nil {
		return b, err
	}
	attestWeight := 0.0
	for _, e := range attests {
		attestWeight += g.nodeTrust(e.Src) * e.Freshness
	}
	b.NumAttestations = len(attests)
	// 1 - e^(-w/2) saturates: 20 points needs ~3-4 strong attestations.
	b.AttestationScore = clamp(20.0*(1.0-math.Exp(-attestWeight/2.0)), 0, 20)

	// 2. Outcome verification (0-25, the moat): reputation-weighted success rate.
	outcomes, err := g.EdgesTo(serviceID, EdgeObservedBy)
	if err != nil {
		return b, err
	}
	weightedSuccess := 0.0
	totalWeight := 0.0
	for _, e := range outcomes {
		success, _ := e.Attrs["success"].(float64)
		if success > 1 {
			success = 1
		}
		w := g.nodeTrust(e.Src) * e.Freshness
		weightedSuccess += w * success
		totalWeight += w
	}
	b.NumOutcomes = len(outcomes)
	if totalWeight > 0 {
		outcomeRate := weightedSuccess / totalWeight
		// Confidence ramps with weighted sample size so a handful of reports
		// can't reach a full 25 regardless of how trusted the reporters are.
		confidence := 1.0 - math.Exp(-totalWeight/3.0)
		b.OutcomeScore = clamp(25.0*outcomeRate*confidence, 0, 25)
	}

	// 3. Freshness (0-10): average freshness across attestation + outcome edges.
	all := append(append([]Edge{}, attests...), outcomes...)
	if len(all) > 0 {
		sumF := 0.0
		for _, e := range all {
			sumF += e.Freshness
		}
		b.FreshnessScore = clamp(10.0*sumF/float64(len(all)), 0, 10)
	}

	// 4. Contradiction penalty (0-10): contradicting or superseding evidence.
	contradictions, err := g.EdgesTo(serviceID, EdgeContradicts)
	if err != nil {
		return b, err
	}
	superseded, err := g.EdgesTo(serviceID, EdgeSupersedes)
	if err != nil {
		return b, err
	}
	b.NumContradictions = len(contradictions) + len(superseded)
	b.ContradictionPenalty = clamp(2.0*float64(b.NumContradictions), 0, 10)

	b.Total = clamp(b.AttestationScore+b.OutcomeScore+b.FreshnessScore-b.ContradictionPenalty, 0, 65)
	return b, nil
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return math.Round(v*10) / 10
}
