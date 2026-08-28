package graph

import (
	"database/sql"
	"math"
	"testing"

	_ "modernc.org/sqlite"
)

func testGraph(t *testing.T) *Graph {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	g := New(db)
	if err := g.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return g
}

func mustUpsert(t *testing.T, g *Graph, id string, typ NodeType, attrs map[string]any) {
	t.Helper()
	if err := g.UpsertNode(id, typ, attrs); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestMigrateCreatesTables(t *testing.T) {
	g := testGraph(t)
	// Migrate is idempotent.
	if err := g.Migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if _, err := g.GetNode("does-not-exist"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestAddEdgeIdempotent(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)
	mustUpsert(t, g, "org", NodeOrg, nil)

	id1, err := g.AddEdge("org", "svc", EdgeAttestsTo, "https://ev", HashEvidence("payload"), "org", 1.0, nil)
	if err != nil {
		t.Fatalf("add edge 1: %v", err)
	}
	id2, err := g.AddEdge("org", "svc", EdgeAttestsTo, "https://ev", HashEvidence("payload"), "org", 1.0, nil)
	if err != nil {
		t.Fatalf("add edge 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("same attestation produced different edge IDs: %s vs %s", id1, id2)
	}
	edges, _ := g.EdgesTo("svc", EdgeAttestsTo)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge after dedup, got %d", len(edges))
	}
}

// The moat: a low-trust reporter's positive outcome must barely move the
// outcome score; a high-trust reporter's must move it materially.
func TestRequesterReputationWeighting(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)

	// A brand-new agent (no trust attr) → floor weight 0.1.
	mustUpsert(t, g, "unknown", NodeAgent, map[string]any{})
	// A highly-trusted agent → weight ~1.0.
	mustUpsert(t, g, "trusted", NodeAgent, map[string]any{"trust": 1.0})

	// Unknown agent reports success — should barely register.
	if _, err := g.AddEdge("unknown", "svc", EdgeObservedBy, "", "", "unknown", 1.0, map[string]any{"success": 1.0, "rating": 5}); err != nil {
		t.Fatalf("unknown outcome: %v", err)
	}
	low, _ := g.TrustScore("svc")

	// Trusted agent reports success — should move it a lot more.
	if _, err := g.AddEdge("trusted", "svc", EdgeObservedBy, "", "", "trusted", 1.0, map[string]any{"success": 1.0, "rating": 5}); err != nil {
		t.Fatalf("trusted outcome: %v", err)
	}
	high, _ := g.TrustScore("svc")

	if high.OutcomeScore <= low.OutcomeScore {
		t.Fatalf("expected high-trust reporter to raise outcome score above low-trust, got low=%v high=%v", low.OutcomeScore, high.OutcomeScore)
	}
	// The high-trust reporter should push outcome rate toward 1.0.
	if high.OutcomeScore <= 0 {
		t.Fatalf("expected positive outcome score, got %v", high.OutcomeScore)
	}
}

// A single trusted reporter cannot instantly max the outcome score — the
// confidence term requires accumulating weighted sample volume.
func TestOutcomeConfidenceRamps(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)
	mustUpsert(t, g, "trusted", NodeAgent, map[string]any{"trust": 1.0})

	if _, err := g.AddEdge("trusted", "svc", EdgeObservedBy, "", "", "trusted", 1.0, map[string]any{"success": 1.0}); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	one, _ := g.TrustScore("svc")
	if one.OutcomeScore >= 25.0 {
		t.Fatalf("single report must not max the outcome score, got %v", one.OutcomeScore)
	}
	// 10 more reports should push it much higher.
	for i := 0; i < 10; i++ {
		g.AddEdge("trusted", "svc", EdgeObservedBy, "", "", "trusted", 1.0, map[string]any{"success": 1.0})
	}
	many, _ := g.TrustScore("svc")
	if many.OutcomeScore <= one.OutcomeScore {
		t.Fatalf("expected outcome score to rise with more reports: %v -> %v", one.OutcomeScore, many.OutcomeScore)
	}
}

func TestContradictionPenalty(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)
	mustUpsert(t, g, "org", NodeOrg, nil)

	if _, err := g.AddEdge("org", "svc", EdgeContradicts, "", "", "org", 1.0, nil); err != nil {
		t.Fatalf("contradict: %v", err)
	}
	b, _ := g.TrustScore("svc")
	if b.ContradictionPenalty <= 0 {
		t.Fatalf("expected contradiction penalty, got %v", b.ContradictionPenalty)
	}
	if b.Total > 65 || math.IsNaN(b.Total) {
		t.Fatalf("total out of range: %v", b.Total)
	}
}

func TestEdgesFrom(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)
	mustUpsert(t, g, "org", NodeOrg, nil)

	_, err := g.AddEdge("org", "svc", EdgeAttestsTo, "https://ev", HashEvidence("payload"), "org", 1.0, nil)
	if err != nil {
		t.Fatalf("add edge: %v", err)
	}

	edges, err := g.EdgesFrom("org", EdgeAttestsTo)
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].Src != "org" || edges[0].Dst != "svc" {
		t.Fatalf("wrong edge: src=%s dst=%s", edges[0].Src, edges[0].Dst)
	}
}

func TestDeleteEdge(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "svc", NodeService, nil)
	mustUpsert(t, g, "org", NodeOrg, nil)

	id, err := g.AddEdge("org", "svc", EdgeAttestsTo, "", "", "org", 1.0, nil)
	if err != nil {
		t.Fatalf("add edge: %v", err)
	}

	edges, _ := g.EdgesTo("svc", EdgeAttestsTo)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge before delete, got %d", len(edges))
	}

	if err := g.DeleteEdge(id); err != nil {
		t.Fatalf("delete edge: %v", err)
	}

	edges, err = g.EdgesTo("svc", EdgeAttestsTo)
	if err != nil {
		t.Fatalf("edges to after delete: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges after delete, got %d", len(edges))
	}
}

func TestClaimScore(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "claim-1", NodeKnowledgeClaim, map[string]any{"content": "Earth is round", "domain": "science"})
	mustUpsert(t, g, "trusted-org", NodeOrg, map[string]any{"trust": 1.0})

	_, err := g.AddEdge("trusted-org", "claim-1", EdgeAttestsTo, "", "", "trusted-org", 1.0, nil)
	if err != nil {
		t.Fatalf("add attestation: %v", err)
	}

	b, err := g.ClaimScore("claim-1")
	if err != nil {
		t.Fatalf("claim score: %v", err)
	}
	if b.AttestationScore <= 0 {
		t.Fatalf("expected positive attestation score, got %v", b.AttestationScore)
	}
	if b.Total <= 0 {
		t.Fatalf("expected positive total, got %v", b.Total)
	}
	if b.NumAttestations != 1 {
		t.Fatalf("expected 1 attestation, got %d", b.NumAttestations)
	}
}

func TestClaimScoreContradiction(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "claim-1", NodeKnowledgeClaim, nil)
	mustUpsert(t, g, "claim-2", NodeKnowledgeClaim, nil)

	_, err := g.AddEdge("claim-2", "claim-1", EdgeContradicts, "", "", "claim-2", 1.0, nil)
	if err != nil {
		t.Fatalf("add contradiction: %v", err)
	}

	b, err := g.ClaimScore("claim-1")
	if err != nil {
		t.Fatalf("claim score: %v", err)
	}
	if b.ContradictionPenalty <= 0 {
		t.Fatalf("expected contradiction penalty > 0, got %v", b.ContradictionPenalty)
	}
	if b.NumContradictions != 1 {
		t.Fatalf("expected 1 contradiction, got %d", b.NumContradictions)
	}
	if b.Total > 0 {
		t.Fatalf("expected total to be 0 with contradiction penalty and no attestations, got %v", b.Total)
	}
}

func TestClaimScoreSupersession(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "claim-v1", NodeKnowledgeClaim, nil)
	mustUpsert(t, g, "claim-v2", NodeKnowledgeClaim, nil)

	_, err := g.AddEdge("claim-v2", "claim-v1", EdgeSupersedes, "", "", "claim-v2", 1.0, nil)
	if err != nil {
		t.Fatalf("add supersede: %v", err)
	}

	b, err := g.ClaimScore("claim-v1")
	if err != nil {
		t.Fatalf("claim score: %v", err)
	}
	if !b.Superseded {
		t.Fatalf("expected superseded=true")
	}
	if b.SupersessionPenalty != 15.0 {
		t.Fatalf("expected supersession penalty 15, got %v", b.SupersessionPenalty)
	}
}

func TestMemoriesByAgent(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "agent-1", NodeAgent, nil)
	mustUpsert(t, g, "mem-1", NodeMemoryEntry, map[string]any{"content": "remember this", "category": "general"})
	mustUpsert(t, g, "mem-2", NodeMemoryEntry, map[string]any{"content": "secret", "category": "private"})

	_, err := g.AddEdge("agent-1", "mem-1", EdgeRemembers, "", "", "agent-1", 1.0, nil)
	if err != nil {
		t.Fatalf("add remembers mem-1: %v", err)
	}
	_, err = g.AddEdge("agent-1", "mem-2", EdgeRemembers, "", "", "agent-1", 1.0, nil)
	if err != nil {
		t.Fatalf("add remembers mem-2: %v", err)
	}

	all, err := g.MemoriesByAgent("agent-1", "", 50)
	if err != nil {
		t.Fatalf("memories by agent: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(all))
	}

	general, err := g.MemoriesByAgent("agent-1", "general", 50)
	if err != nil {
		t.Fatalf("memories by agent filtered: %v", err)
	}
	if len(general) != 1 {
		t.Fatalf("expected 1 memory in general category, got %d", len(general))
	}
	if general[0].ID != "mem-1" {
		t.Fatalf("expected mem-1, got %s", general[0].ID)
	}
}

func TestSearchClaims(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "claim-a", NodeKnowledgeClaim, map[string]any{"content": "Go is fast", "domain": "programming"})
	mustUpsert(t, g, "claim-b", NodeKnowledgeClaim, map[string]any{"content": "Rust is safe", "domain": "programming"})
	mustUpsert(t, g, "claim-c", NodeKnowledgeClaim, map[string]any{"content": "Water is wet", "domain": "science"})

	results, err := g.SearchClaims("Go", 50)
	if err != nil {
		t.Fatalf("search claims: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'Go', got %d", len(results))
	}
	if results[0].ID != "claim-a" {
		t.Fatalf("expected claim-a, got %s", results[0].ID)
	}

	results, err = g.SearchClaims("programming", 50)
	if err != nil {
		t.Fatalf("search claims domain: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'programming', got %d", len(results))
	}

	results, err = g.SearchClaims("nonexistent", 50)
	if err != nil {
		t.Fatalf("search claims none: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for nonexistent, got %d", len(results))
	}
}

func TestDeleteMemoryEntry(t *testing.T) {
	g := testGraph(t)
	mustUpsert(t, g, "agent-1", NodeAgent, nil)
	mustUpsert(t, g, "mem-1", NodeMemoryEntry, map[string]any{"content": "to be deleted"})

	_, err := g.AddEdge("agent-1", "mem-1", EdgeRemembers, "", "", "agent-1", 1.0, nil)
	if err != nil {
		t.Fatalf("add remembers: %v", err)
	}

	n, err := g.GetNode("mem-1")
	if err != nil {
		t.Fatalf("get node before delete: %v", err)
	}
	if n == nil {
		t.Fatal("expected node to exist")
	}

	if err := g.DeleteMemoryEntry("mem-1"); err != nil {
		t.Fatalf("delete memory entry: %v", err)
	}

	_, err = g.GetNode("mem-1")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows after delete, got %v", err)
	}
}
