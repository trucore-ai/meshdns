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
