package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrDuplicateName = errors.New("duplicate server name")

const migration = `
CREATE TABLE IF NOT EXISTS servers (
	id TEXT PRIMARY KEY,
	name TEXT UNIQUE NOT NULL,
	description TEXT,
	server_url TEXT NOT NULL,
	health_url TEXT,
	write_key_hash TEXT NOT NULL,
	owner_contact TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	probe_method TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS capabilities (
	server_id TEXT NOT NULL,
	capability TEXT NOT NULL,
	PRIMARY KEY (server_id, capability)
);
CREATE TABLE IF NOT EXISTS probes (
	id INTEGER PRIMARY KEY,
	server_id TEXT NOT NULL,
	ts TEXT NOT NULL,
	up INTEGER NOT NULL,
	latency_ms INTEGER
);
CREATE INDEX IF NOT EXISTS idx_probes_server_ts ON probes(server_id, ts);
CREATE TABLE IF NOT EXISTS server_state (
	server_id TEXT PRIMARY KEY,
	up INTEGER NOT NULL DEFAULT 0,
	last_checked_at TEXT,
	uptime_30d REAL NOT NULL DEFAULT 0,
	probe_count_30d INTEGER NOT NULL DEFAULT 0,
	up_count_30d INTEGER NOT NULL DEFAULT 0,
	avg_latency_ms INTEGER NOT NULL DEFAULT 0,
	latency_p50_ms INTEGER NOT NULL DEFAULT 0,
	resolution_count_24h INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY,
	ts TEXT NOT NULL,
	type TEXT NOT NULL,
	payload TEXT NOT NULL
);`

type Store struct {
	db *sql.DB
}

type Server struct {
	ID               string
	Name             string
	Description      string
	ServerURL        string
	HealthURL        string
	OwnerContact     string
	Status           string
	ProbeMethod      string
	Capabilities     []string
	CreatedAt        string
	UpdatedAt        string
	Up               bool
	LastCheckedAt    string
	Uptime30d        float64
	AvgLatencyMs     int
	LatencyP50Ms     int
	ResolutionCount  int
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec(migration); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := ensureServerColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := ensureRetentionColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// ensureServerColumns adds columns introduced after initial deployment
// (CREATE TABLE IF NOT EXISTS does not modify pre-existing tables).
func ensureServerColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(servers)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	hasProbeMethod := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "probe_method" {
			hasProbeMethod = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if !hasProbeMethod {
		if _, err := db.Exec(`ALTER TABLE servers ADD COLUMN probe_method TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	return nil
}

// ensureRetentionColumns adds counter/latency columns introduced for incremental uptime.
func ensureRetentionColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(server_state)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	has := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		has[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cols := []struct{ name, def string }{
		{"probe_count_30d", "INTEGER NOT NULL DEFAULT 0"},
		{"up_count_30d", "INTEGER NOT NULL DEFAULT 0"},
		{"avg_latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"latency_p50_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"resolution_count_24h", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, col := range cols {
		if !has[col.name] {
			if _, err := db.Exec(`ALTER TABLE server_state ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateServer(server Server, writeKeyHash string) error {
	now := nowRFC3339()
	if server.Status == "" {
		server.Status = "active"
	}
	if server.CreatedAt == "" {
		server.CreatedAt = now
	}
	if server.UpdatedAt == "" {
		server.UpdatedAt = server.CreatedAt
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	_, err = tx.Exec(`
INSERT INTO servers (
	id, name, description, server_url, health_url, write_key_hash,
	owner_contact, status, probe_method, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		server.ID,
		server.Name,
		server.Description,
		server.ServerURL,
		server.HealthURL,
		writeKeyHash,
		server.OwnerContact,
		server.Status,
		server.ProbeMethod,
		server.CreatedAt,
		server.UpdatedAt,
	)
	if isDuplicateNameErr(err) {
		return fmt.Errorf("%w: %s", ErrDuplicateName, err)
	}
	if err != nil {
		return err
	}

	if err := setCapabilitiesTx(tx, server.ID, server.Capabilities); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) GetServer(id string) (Server, error) {
	return s.getServer("servers.id = ?", id)
}

func (s *Store) GetServerByName(name string) (Server, error) {
	return s.getServer("servers.name = ?", name)
}

func (s *Store) GetServerWriteKeyHash(id string) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT write_key_hash FROM servers WHERE id = ?`, id).Scan(&hash)
	if err != nil {
		return "", err
	}

	return hash, nil
}

func (s *Store) UpdateServer(server Server) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	updatedAt := nowRFC3339()
	result, err := tx.Exec(`
UPDATE servers
SET description = ?, server_url = ?, health_url = ?, owner_contact = ?, probe_method = ?, updated_at = ?
WHERE id = ?`,
		server.Description,
		server.ServerURL,
		server.HealthURL,
		server.OwnerContact,
		server.ProbeMethod,
		updatedAt,
		server.ID,
	)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	if err := setCapabilitiesTx(tx, server.ID, server.Capabilities); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) SetServerProbeMethod(id string, method string) error {
	result, err := s.db.Exec(`UPDATE servers SET probe_method = ?, updated_at = ? WHERE id = ?`, method, nowRFC3339(), id)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	return nil
}

func (s *Store) DelistServer(id string) error {
	result, err := s.db.Exec(`UPDATE servers SET status = ?, updated_at = ? WHERE id = ?`, "delisted", nowRFC3339(), id)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	return nil
}

func (s *Store) ListServers(query, capability, status string, cursor string, limit int) ([]Server, string, error) {
	if limit <= 0 {
		limit = 100
	}

	args := make([]any, 0, 8)
	var b strings.Builder
	b.WriteString(baseServerQuery())
	if capability != "" {
		b.WriteString(` JOIN capabilities capability_filter ON capability_filter.server_id = servers.id`)
	}
	b.WriteString(` WHERE 1 = 1`)

	if query != "" {
		pattern := "%" + strings.ToLower(query) + "%"
		b.WriteString(` AND (lower(servers.name) LIKE ? OR lower(coalesce(servers.description, '')) LIKE ?)`)
		args = append(args, pattern, pattern)
	}
	if capability != "" {
		b.WriteString(` AND capability_filter.capability = ?`)
		args = append(args, capability)
	}
	if status != "" {
		b.WriteString(` AND servers.status = ?`)
		args = append(args, status)
	}
	if cursor != "" {
		var cursorName string
		err := s.db.QueryRow(`SELECT name FROM servers WHERE id = ?`, cursor).Scan(&cursorName)
		if err != nil {
			return nil, "", err
		}
		b.WriteString(` AND (servers.name > ? OR (servers.name = ? AND servers.id > ?))`)
		args = append(args, cursorName, cursorName, cursor)
	}

	b.WriteString(` ORDER BY servers.name, servers.id LIMIT ?`)
	args = append(args, limit+1)

	servers, err := s.queryServers(b.String(), args...)
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(servers) > limit {
		nextCursor = servers[limit-1].ID
		servers = servers[:limit]
	}

	return servers, nextCursor, nil
}

func (s *Store) SetCapabilities(serverID string, caps []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	if err := setCapabilitiesTx(tx, serverID, caps); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) RecordProbe(serverID string, ts string, up bool, latencyMs int) error {
	var latency any = latencyMs
	if latencyMs < 0 {
		latency = nil
	}

	_, err := s.db.Exec(`INSERT INTO probes (server_id, ts, up, latency_ms) VALUES (?, ?, ?, ?)`, serverID, ts, boolInt(up), latency)
	return err
}

func (s *Store) SetServerState(serverID string, up bool, lastCheckedAt string, uptime30d float64) error {
	_, err := s.db.Exec(`
INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d)
VALUES (?, ?, ?, ?)
ON CONFLICT(server_id) DO UPDATE SET
	up = excluded.up,
	last_checked_at = excluded.last_checked_at,
	uptime_30d = excluded.uptime_30d`,
		serverID,
		boolInt(up),
		lastCheckedAt,
		uptime30d,
	)
	return err
}

func (s *Store) GetUpServersByCapability(capability string) ([]Server, error) {
	return s.queryServers(`
SELECT
	servers.id, servers.name, coalesce(servers.description, ''), servers.server_url,
	coalesce(servers.health_url, ''), coalesce(servers.owner_contact, ''), servers.status,
	coalesce(servers.probe_method, ''),
	servers.created_at, servers.updated_at,
	server_state.up, server_state.last_checked_at, server_state.uptime_30d,
	server_state.avg_latency_ms, server_state.latency_p50_ms, server_state.resolution_count_24h
FROM servers
JOIN capabilities ON capabilities.server_id = servers.id
JOIN server_state ON server_state.server_id = servers.id
WHERE server_state.up = 1
	AND servers.status = 'active'
	AND capabilities.capability = ?
ORDER BY server_state.uptime_30d DESC, servers.name`, capability)
}

func (s *Store) GetUptime30d(serverID string) (float64, error) {
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	var total int
	var up int
	err := s.db.QueryRow(`
SELECT count(*), coalesce(sum(CASE WHEN up = 1 THEN 1 ELSE 0 END), 0)
FROM probes
WHERE server_id = ? AND ts >= ?`, serverID, cutoff).Scan(&total, &up)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 1, nil
	}

	return float64(up) / float64(total), nil
}

func (s *Store) AppendEvent(ts, eventType, payloadJSON string) error {
	_, err := s.db.Exec(`INSERT INTO events (ts, type, payload) VALUES (?, ?, ?)`, ts, eventType, payloadJSON)
	return err
}

func (s *Store) CountEventsSince(eventType, sinceTs string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM events WHERE type = ? AND ts >= ?`, eventType, sinceTs).Scan(&count)
	if err != nil {
		return 0, err
	}

	return count, nil
}

func (s *Store) CountServers(status string) (int, error) {
	var count int
	var err error
	if status == "" || status == "all" {
		err = s.db.QueryRow(`SELECT count(*) FROM servers`).Scan(&count)
	} else {
		err = s.db.QueryRow(`SELECT count(*) FROM servers WHERE status = ?`, status).Scan(&count)
	}
	if err != nil {
		return 0, err
	}

	return count, nil
}

func (s *Store) CountUpServers(status string) (int, error) {
	var count int
	var err error
	if status == "" || status == "all" {
		err = s.db.QueryRow(`
SELECT count(*)
FROM servers
JOIN server_state ON server_state.server_id = servers.id
WHERE server_state.up = 1`).Scan(&count)
	} else {
		err = s.db.QueryRow(`
SELECT count(*)
FROM servers
JOIN server_state ON server_state.server_id = servers.id
WHERE server_state.up = 1 AND servers.status = ?`, status).Scan(&count)
	}
	if err != nil {
		return 0, err
	}

	return count, nil
}

func (s *Store) ExportAll() ([]Server, error) {
	return s.queryServers(baseServerQuery() + ` ORDER BY servers.name, servers.id`)
}

func (s *Store) getServer(where string, args ...any) (Server, error) {
	servers, err := s.queryServers(baseServerQuery()+` WHERE `+where, args...)
	if err != nil {
		return Server{}, err
	}
	if len(servers) == 0 {
		return Server{}, sql.ErrNoRows
	}

	return servers[0], nil
}

func (s *Store) queryServers(query string, args ...any) ([]Server, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	servers := make([]Server, 0)
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range servers {
		caps, err := s.capabilities(servers[i].ID)
		if err != nil {
			return nil, err
		}
		servers[i].Capabilities = caps
	}

	return servers, nil
}

func (s *Store) capabilities(serverID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT capability FROM capabilities WHERE server_id = ? ORDER BY capability`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	caps := make([]string, 0)
	for rows.Next() {
		var cap string
		if err := rows.Scan(&cap); err != nil {
			return nil, err
		}
		caps = append(caps, cap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return caps, nil
}

func baseServerQuery() string {
	return `
SELECT
	servers.id, servers.name, coalesce(servers.description, ''), servers.server_url,
	coalesce(servers.health_url, ''), coalesce(servers.owner_contact, ''), servers.status,
	coalesce(servers.probe_method, ''),
	servers.created_at, servers.updated_at,
	coalesce(server_state.up, 0), coalesce(server_state.last_checked_at, ''),
	coalesce(server_state.uptime_30d, 0),
	coalesce(server_state.avg_latency_ms, 0),
	coalesce(server_state.latency_p50_ms, 0),
	coalesce(server_state.resolution_count_24h, 0)
FROM servers
LEFT JOIN server_state ON server_state.server_id = servers.id`
}

type serverScanner interface {
	Scan(dest ...any) error
}

func scanServer(scanner serverScanner) (Server, error) {
	var server Server
	var up int
	err := scanner.Scan(
		&server.ID,
		&server.Name,
		&server.Description,
		&server.ServerURL,
		&server.HealthURL,
		&server.OwnerContact,
		&server.Status,
		&server.ProbeMethod,
		&server.CreatedAt,
		&server.UpdatedAt,
		&up,
		&server.LastCheckedAt,
		&server.Uptime30d,
		&server.AvgLatencyMs,
		&server.LatencyP50Ms,
		&server.ResolutionCount,
	)
	if err != nil {
		return Server{}, err
	}
	server.Up = up == 1

	return server, nil
}

func setCapabilitiesTx(tx *sql.Tx, serverID string, caps []string) error {
	if _, err := tx.Exec(`DELETE FROM capabilities WHERE server_id = ?`, serverID); err != nil {
		return err
	}

	for _, cap := range normalizeCaps(caps) {
		if _, err := tx.Exec(`INSERT INTO capabilities (server_id, capability) VALUES (?, ?)`, serverID, cap); err != nil {
			return err
		}
	}

	return nil
}

func normalizeCaps(caps []string) []string {
	seen := make(map[string]struct{}, len(caps))
	normalized := make([]string, 0, len(caps))
	for _, cap := range caps {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		normalized = append(normalized, cap)
	}
	sort.Strings(normalized)

	return normalized
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func isDuplicateNameErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: servers.name")
}

func boolInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// CapabilityInfo holds an aggregated capability name with its count.
type CapabilityInfo struct {
	Name  string `json:"name"`
	Count int    `json:"server_count"`
}

// IncrementProbeCount atomically bumps the probe counter for a server.
func (s *Store) IncrementProbeCount(serverID string, up bool) error {
	inc := 0
	if up {
		inc = 1
	}
	_, err := s.db.Exec(`INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d, probe_count_30d, up_count_30d, avg_latency_ms, latency_p50_ms, resolution_count_24h) VALUES (?, 0, '', 0, 1, ?, 0, 0, 0) ON CONFLICT(server_id) DO UPDATE SET probe_count_30d = probe_count_30d + 1, up_count_30d = up_count_30d + ?`, serverID, inc, inc)
	return err
}

// ComputeUptimeFromCounters returns uptime ratio from incremental counters.
func (s *Store) ComputeUptimeFromCounters(serverID string) (float64, error) {
	var total, up int
	err := s.db.QueryRow(`SELECT probe_count_30d, up_count_30d FROM server_state WHERE server_id = ?`, serverID).Scan(&total, &up)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 1, nil
	}
	return float64(up) / float64(total), nil
}

// UpdateLatencyStats applies EWMA for average and a simple min-tracker for p50.
func (s *Store) UpdateLatencyStats(serverID string, latencyMs int) error {
	_, err := s.db.Exec(`INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d, probe_count_30d, up_count_30d, avg_latency_ms, latency_p50_ms, resolution_count_24h) VALUES (?, 0, '', 0, 0, 0, ?, ?, 0) ON CONFLICT(server_id) DO UPDATE SET avg_latency_ms = cast(avg_latency_ms * 0.9 + ? * 0.1 as integer), latency_p50_ms = CASE WHEN ? < latency_p50_ms OR latency_p50_ms = 0 THEN ? ELSE latency_p50_ms END`, serverID, latencyMs, latencyMs, latencyMs, latencyMs, latencyMs)
	return err
}

// IncrementResolutionCount bumps the 24h resolution counter.
func (s *Store) IncrementResolutionCount(serverID string) error {
	_, err := s.db.Exec(`INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d, probe_count_30d, up_count_30d, avg_latency_ms, latency_p50_ms, resolution_count_24h) VALUES (?, 0, '', 0, 0, 0, 0, 0, 1) ON CONFLICT(server_id) DO UPDATE SET resolution_count_24h = resolution_count_24h + 1`, serverID)
	return err
}

// ListCapabilities returns distinct capabilities with active server counts.
func (s *Store) ListCapabilities() ([]CapabilityInfo, error) {
	rows, err := s.db.Query(`SELECT DISTINCT c.capability, count(*) as server_count FROM capabilities c JOIN servers s ON s.id = c.server_id WHERE s.status = 'active' GROUP BY c.capability ORDER BY server_count DESC, c.capability`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	caps := make([]CapabilityInfo, 0)
	for rows.Next() {
		var c CapabilityInfo
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		caps = append(caps, c)
	}
	return caps, rows.Err()
}

// PruneProbes deletes probes older than the given retention days.
func (s *Store) PruneProbes(retentionDays int) (int64, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour).Format(time.RFC3339)
	result, err := s.db.Exec(`DELETE FROM probes WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneEvents deletes events older than the given retention days.
func (s *Store) PruneEvents(retentionDays int) (int64, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour).Format(time.RFC3339)
	result, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RebuildAllUptimeCounters recomputes probe_count_30d and up_count_30d from the probes table.
func (s *Store) RebuildAllUptimeCounters() error {
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT server_id, count(*) as total, coalesce(sum(CASE WHEN up = 1 THEN 1 ELSE 0 END), 0) as up_count FROM probes WHERE ts >= ? GROUP BY server_id`, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var serverID string
		var total, upCount int
		if err := rows.Scan(&serverID, &total, &upCount); err != nil {
			return err
		}
		uptime := 1.0
		if total > 0 {
			uptime = float64(upCount) / float64(total)
		}
		_, err := s.db.Exec(`INSERT INTO server_state (server_id, up, last_checked_at, uptime_30d, probe_count_30d, up_count_30d, avg_latency_ms, latency_p50_ms, resolution_count_24h) VALUES (?, 0, '', ?, ?, ?, 0, 0, 0) ON CONFLICT(server_id) DO UPDATE SET uptime_30d = ?, probe_count_30d = ?, up_count_30d = ?`, serverID, uptime, total, upCount, uptime, total, upCount)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}
