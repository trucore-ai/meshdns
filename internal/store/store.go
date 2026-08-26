package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Server is a registered MCP server record.
type Server struct {
	ID           string
	Name         string
	Description  string
	ServerURL    string
	HealthURL    string
	WriteKeyHash string
	OwnerContact string
	Status       string
	ProbeMethod  string
	CreatedAt    string
	UpdatedAt    string
}

// ServerWithState includes runtime state for list/resolve responses.
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
	Source        string   `json:"source,omitempty"`
	ToolCount     int      `json:"tool_count,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// Export represents the full registry dump.
type Export struct {
	ExportedAt string            `json:"exported_at"`
	Servers    []ServerWithState `json:"servers"`
}

// Stats represents registry statistics.
type Stats struct {
	ServersActive  int            `json:"servers_active"`
	ServersTotal   int            `json:"servers_total"`
	Resolutions24h int            `json:"resolutions_24h"`
	Probes24h      int            `json:"probes_24h"`
	UpCount        int            `json:"up_count"`
	ToolCountTotal int            `json:"tool_count_total"`
	SourceBreakdown map[string]int `json:"source_breakdown"`
}

// CapabilityInfo holds an aggregated capability name with its active server count.
type CapabilityInfo struct {
	Name  string `json:"name"`
	Count int    `json:"server_count"`
}

// Store wraps the SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open creates or opens the SQLite database, runs migrations, and enables WAL mode.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite serializes writes
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS servers (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		description TEXT DEFAULT '',
		server_url TEXT NOT NULL,
		health_url TEXT DEFAULT '',
		write_key_hash TEXT NOT NULL,
		owner_contact TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		probe_method TEXT NOT NULL DEFAULT '',
		source TEXT DEFAULT '',
		tool_count INTEGER DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS capabilities (
		server_id TEXT NOT NULL,
		capability TEXT NOT NULL,
		PRIMARY KEY (server_id, capability),
		FOREIGN KEY (server_id) REFERENCES servers(id)
	);
	CREATE TABLE IF NOT EXISTS probes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		server_id TEXT NOT NULL,
		ts TEXT NOT NULL,
		up INTEGER NOT NULL,
		latency_ms INTEGER DEFAULT 0,
		FOREIGN KEY (server_id) REFERENCES servers(id)
	);
	CREATE INDEX IF NOT EXISTS idx_probes_server_ts ON probes(server_id, ts);
	CREATE TABLE IF NOT EXISTS server_state (
		server_id TEXT PRIMARY KEY,
		up INTEGER NOT NULL DEFAULT 0,
		last_checked_at TEXT DEFAULT '',
		uptime_30d REAL NOT NULL DEFAULT 0.0,
		FOREIGN KEY (server_id) REFERENCES servers(id)
	);
	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL,
		type TEXT NOT NULL,
		payload TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_events_type_ts ON events(type, ts);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Add columns that may be missing in pre-existing DBs
	return s.ensureServerColumns()
}

// ensureServerColumns adds columns introduced after initial deployment
// (CREATE TABLE IF NOT EXISTS does not modify pre-existing tables).
func (s *Store) ensureServerColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(servers)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	hasProbeMethod := false
	hasSource := false
	hasToolCount := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		switch name {
		case "probe_method":
			hasProbeMethod = true
		case "source":
			hasSource = true
		case "tool_count":
			hasToolCount = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if !hasProbeMethod {
		if _, err := s.db.Exec(`ALTER TABLE servers ADD COLUMN probe_method TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasSource {
		if _, err := s.db.Exec(`ALTER TABLE servers ADD COLUMN source TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasToolCount {
		if _, err := s.db.Exec(`ALTER TABLE servers ADD COLUMN tool_count INTEGER DEFAULT 0`); err != nil {
			return err
		}
	}

	return nil
}

// CreateServer inserts a new server record. Returns the generated UUID.
func (s *Store) CreateServer(srv *Server) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	srv.CreatedAt = now
	srv.UpdatedAt = now
	if srv.Status == "" {
		srv.Status = "active"
	}
	if srv.ProbeMethod == "" {
		srv.ProbeMethod = "GET"
	}

	_, err := s.db.Exec(
		`INSERT INTO servers (id, name, description, server_url, health_url, write_key_hash,
		 owner_contact, status, probe_method, source, tool_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		srv.ID, srv.Name, srv.Description, srv.ServerURL, srv.HealthURL,
		srv.WriteKeyHash, srv.OwnerContact, srv.Status, srv.ProbeMethod,
		"", 0, srv.CreatedAt, srv.UpdatedAt,
	)
	if err != nil {
		return "", err
	}
	return srv.ID, nil
}

// GetServer retrieves a server by ID.
func (s *Store) GetServer(id string) (*Server, error) {
	srv := &Server{}
	err := s.db.QueryRow(
		`SELECT id, name, description, server_url, health_url, write_key_hash,
		 owner_contact, status, probe_method, created_at, updated_at
		 FROM servers WHERE id = ?`, id,
	).Scan(&srv.ID, &srv.Name, &srv.Description, &srv.ServerURL, &srv.HealthURL,
		&srv.WriteKeyHash, &srv.OwnerContact, &srv.Status, &srv.ProbeMethod,
		&srv.CreatedAt, &srv.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// GetServerByName finds a server by name (for duplicate check).
func (s *Store) GetServerByName(name string) (*Server, error) {
	srv := &Server{}
	err := s.db.QueryRow(
		`SELECT id, name, description, server_url, health_url, write_key_hash,
		 owner_contact, status, probe_method, created_at, updated_at
		 FROM servers WHERE name = ?`, name,
	).Scan(&srv.ID, &srv.Name, &srv.Description, &srv.ServerURL, &srv.HealthURL,
		&srv.WriteKeyHash, &srv.OwnerContact, &srv.Status, &srv.ProbeMethod,
		&srv.CreatedAt, &srv.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// UpdateServer updates a server manifest. Requires correct write_key_hash.
func (s *Store) UpdateServer(id, writeKeyHash string, updates *Server) error {
	// Verify write key
	var storedHash string
	err := s.db.QueryRow(`SELECT write_key_hash FROM servers WHERE id = ?`, id).Scan(&storedHash)
	if err != nil {
		return err
	}
	if storedHash != writeKeyHash {
		return fmt.Errorf("unauthorized: write key mismatch")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(
		`UPDATE servers SET name=?, description=?, server_url=?, health_url=?,
		 owner_contact=?, probe_method=?, updated_at=? WHERE id=?`,
		updates.Name, updates.Description, updates.ServerURL, updates.HealthURL,
		updates.OwnerContact, updates.ProbeMethod, now, id,
	)
	return err
}

// SetServerProbeMethod updates only the probe_method for a server.
func (s *Store) SetServerProbeMethod(id, method string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`UPDATE servers SET probe_method = ?, updated_at = ? WHERE id = ?`,
		method, now, id)
	return err
}

// DelistServer soft-deletes a server (sets status=delisted).
func (s *Store) DelistServer(id, writeKeyHash string) error {
	var storedHash string
	err := s.db.QueryRow(`SELECT write_key_hash FROM servers WHERE id = ?`, id).Scan(&storedHash)
	if err != nil {
		return err
	}
	if storedHash != writeKeyHash {
		return fmt.Errorf("unauthorized: write key mismatch")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(`UPDATE servers SET status='delisted', updated_at=? WHERE id=?`, now, id)
	return err
}

// VerifyWriteKey checks if the provided hash matches the stored hash for a server.
func (s *Store) VerifyWriteKey(id, writeKeyHash string) bool {
	var stored string
	err := s.db.QueryRow(`SELECT write_key_hash FROM servers WHERE id = ?`, id).Scan(&stored)
	if err != nil {
		return false
	}
	return stored == writeKeyHash
}

// ListServers returns paginated server results with optional filters.
// status: "active", "delisted", "all"
func (s *Store) ListServers(query, capability, status, cursor string, limit int) ([]ServerWithState, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	where := "WHERE 1=1"
	args := []any{}

	if status == "active" {
		where += " AND s.status = 'active'"
	} else if status == "delisted" {
		where += " AND s.status = 'delisted'"
	}
	if query != "" {
		where += " AND (s.name LIKE ? OR s.description LIKE ?)"
		q := "%" + query + "%"
		args = append(args, q, q)
	}
	if capability != "" {
		where += " AND EXISTS (SELECT 1 FROM capabilities c WHERE c.server_id = s.id AND c.capability = ?)"
		args = append(args, capability)
	}
	if cursor != "" {
		where += " AND s.id > ?"
		args = append(args, cursor)
	}

	baseSQL := `SELECT s.id, s.name, s.description, s.server_url, s.health_url,
		s.status, s.owner_contact, s.probe_method,
		COALESCE(s.source, ''), COALESCE(s.tool_count, 0),
		s.created_at, s.updated_at,
		COALESCE(ss.up, 0), COALESCE(ss.uptime_30d, 0.0), COALESCE(ss.last_checked_at, '')
		FROM servers s
		LEFT JOIN server_state ss ON ss.server_id = s.id ` +
		where +
		` ORDER BY s.id ASC LIMIT ?`

	args = append(args, limit+1)

	rows, err := s.db.Query(baseSQL, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var results []ServerWithState
	for rows.Next() {
		var sws ServerWithState
		if err := rows.Scan(&sws.ID, &sws.Name, &sws.Description, &sws.ServerURL, &sws.HealthURL,
			&sws.Status, &sws.OwnerContact, &sws.ProbeMethod,
			&sws.Source, &sws.ToolCount,
			&sws.CreatedAt, &sws.UpdatedAt,
			&sws.Up, &sws.Uptime30d, &sws.LastCheckedAt); err != nil {
			return nil, "", err
		}
		results = append(results, sws)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	// Fetch capabilities after rows closed (avoid SQLite single-conn deadlock)
	for i := range results {
		caps, _ := s.GetCapabilities(results[i].ID)
		results[i].Capabilities = caps
	}

	var nextCursor string
	if len(results) > limit {
		nextCursor = results[limit-1].ID
		results = results[:limit]
	}

	return results, nextCursor, nil
}

// SetCapabilities replaces all capabilities for a server.
func (s *Store) SetCapabilities(serverID string, caps []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM capabilities WHERE server_id = ?`, serverID); err != nil {
		return err
	}
	for _, c := range caps {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO capabilities (server_id, capability) VALUES (?, ?)`,
			serverID, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetCapabilities retrieves all capabilities for a server.
func (s *Store) GetCapabilities(serverID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT capability FROM capabilities WHERE server_id = ? ORDER BY capability`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var caps []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		caps = append(caps, c)
	}
	return caps, nil
}

// ListCapabilities returns distinct capabilities with active server counts, ordered by count desc.
func (s *Store) ListCapabilities() ([]CapabilityInfo, error) {
	rows, err := s.db.Query(`SELECT c.capability, COUNT(*) as server_count 
		FROM capabilities c 
		JOIN servers s ON s.id = c.server_id 
		WHERE s.status = 'active' 
		GROUP BY c.capability 
		ORDER BY server_count DESC, c.capability`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var caps []CapabilityInfo
	for rows.Next() {
		var c CapabilityInfo
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		caps = append(caps, c)
	}
	return caps, rows.Err()
}

// RecordProbe inserts a probe result.
func (s *Store) RecordProbe(serverID string, up bool, latencyMs int) error {
	upInt := 0
	if up {
		upInt = 1
	}
	_, err := s.db.Exec(`INSERT INTO probes (server_id, ts, up, latency_ms) VALUES (?, ?, ?, ?)`,
		serverID, time.Now().UTC().Format(time.RFC3339), upInt, latencyMs)
	return err
}

// SetServerState updates the live state for a server.
func (s *Store) SetServerState(serverID string, up int, lastCheckedAt string, uptime30d float64) error {
	_, err := s.db.Exec(
		`INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(server_id) DO UPDATE SET up=?, last_checked_at=?, uptime_30d=?`,
		serverID, up, lastCheckedAt, uptime30d, up, lastCheckedAt, uptime30d,
	)
	return err
}

// GetUpServersByCapability returns UP, active servers matching a capability, ranked by uptime.
func (s *Store) GetUpServersByCapability(capability string) ([]ServerWithState, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.name, s.description, s.server_url, s.health_url,
			s.status, s.owner_contact, s.probe_method,
			COALESCE(s.source, ''), COALESCE(s.tool_count, 0),
			s.created_at, s.updated_at,
			COALESCE(ss.up, 1), COALESCE(ss.uptime_30d, 0.0), COALESCE(ss.last_checked_at, '')
		FROM servers s
		JOIN capabilities c ON c.server_id = s.id
		LEFT JOIN server_state ss ON ss.server_id = s.id
		WHERE s.status = 'active'
			AND c.capability = ?
			AND (ss.up = 1 OR ss.server_id IS NULL)
		ORDER BY COALESCE(ss.uptime_30d, 0.0) DESC
	`, capability)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect all rows first (close cursor before nested queries)
	var results []ServerWithState
	for rows.Next() {
		var sws ServerWithState
		if err := rows.Scan(&sws.ID, &sws.Name, &sws.Description, &sws.ServerURL, &sws.HealthURL,
			&sws.Status, &sws.OwnerContact, &sws.ProbeMethod,
			&sws.Source, &sws.ToolCount,
			&sws.CreatedAt, &sws.UpdatedAt,
			&sws.Up, &sws.Uptime30d, &sws.LastCheckedAt); err != nil {
			return nil, err
		}
		results = append(results, sws)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Now fetch capabilities for each result (safe — rows closed)
	for i := range results {
		caps, _ := s.GetCapabilities(results[i].ID)
		results[i].Capabilities = caps
	}
	return results, nil
}

// GetUptime30d computes 30-day uptime ratio from probe history.
func (s *Store) GetUptime30d(serverID string) (float64, error) {
	var total, up int
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(up), 0)
		FROM probes
		WHERE server_id = ? AND ts >= ?
	`, serverID, time.Now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339)).Scan(&total, &up)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 1.0, nil // no probes yet = assume healthy
	}
	return float64(up) / float64(total), nil
}

// AppendEvent writes an instrumentation event.
func (s *Store) AppendEvent(eventType, payload string) error {
	_, err := s.db.Exec(`INSERT INTO events (ts, type, payload) VALUES (?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), eventType, payload)
	return err
}

// CountEventsSince counts events of a given type since a timestamp.
func (s *Store) CountEventsSince(eventType, since string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE type = ? AND ts >= ?`,
		eventType, since,
	).Scan(&count)
	return count, err
}

// ExportAll returns all servers with full state (for /v0/export).
func (s *Store) ExportAll() (*Export, error) {
	servers, _, err := s.ListServers("", "", "all", "", 100000)
	if err != nil {
		return nil, err
	}
	return &Export{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Servers:    servers,
	}, nil
}

// GetStats computes current registry statistics.
func (s *Store) GetStats() (*Stats, error) {
	stats := &Stats{}

	s.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE status = 'active'`).Scan(&stats.ServersActive)
	s.db.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&stats.ServersTotal)
	s.db.QueryRow(`SELECT COUNT(*) FROM server_state WHERE up = 1`).Scan(&stats.UpCount)
	s.db.QueryRow(`SELECT COALESCE(SUM(tool_count), 0) FROM servers`).Scan(&stats.ToolCountTotal)

	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'resolve' AND ts >= ?`, since).Scan(&stats.Resolutions24h)
	s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'probe' AND ts >= ?`, since).Scan(&stats.Probes24h)

	// Source breakdown
	stats.SourceBreakdown = make(map[string]int)
	rows, err := s.db.Query(`SELECT COALESCE(source, ''), COUNT(*) FROM servers WHERE status = 'active' GROUP BY COALESCE(source, '')`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var src string
			var count int
			if err := rows.Scan(&src, &count); err == nil {
				if src == "" {
					src = "manual"
				}
				stats.SourceBreakdown[src] = count
			}
		}
	}

	return stats, nil
}