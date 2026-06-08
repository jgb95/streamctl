package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	return &DB{conn}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS endpoints (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	endpoint_type TEXT NOT NULL DEFAULT 'rtmp',
	rtmp_url TEXT NOT NULL,
	stream_key TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS nostr_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	pubkey TEXT NOT NULL,
	npub TEXT NOT NULL,
	secret_path TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS nostr_relays (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	url TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS streams (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	schedule_type TEXT NOT NULL,         -- 'once' or 'recurring'
	on_calendar TEXT NOT NULL,           -- systemd OnCalendar expression
	nostr_enabled INTEGER NOT NULL DEFAULT 0,
	nostr_key_id INTEGER,
	nostr_title TEXT NOT NULL DEFAULT '',
	nostr_summary TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (nostr_key_id) REFERENCES nostr_keys(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS stream_endpoints (
	stream_id INTEGER NOT NULL,
	endpoint_id INTEGER NOT NULL,
	PRIMARY KEY (stream_id, endpoint_id),
	FOREIGN KEY (stream_id) REFERENCES streams(id) ON DELETE CASCADE,
	FOREIGN KEY (endpoint_id) REFERENCES endpoints(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS stream_videos (
	stream_id INTEGER NOT NULL,
	position INTEGER NOT NULL,
	video_file TEXT NOT NULL,
	PRIMARY KEY (stream_id, position),
	FOREIGN KEY (stream_id) REFERENCES streams(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS gpu_job_logs (
	unit_name TEXT PRIMARY KEY,
	raw_path TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	active_state TEXT NOT NULL DEFAULT '',
	sub_state TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL DEFAULT '',
	journal TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS gpu_job_queue (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	raw_path TEXT NOT NULL UNIQUE,
	unit_name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'queued',
	attempt_count INTEGER NOT NULL DEFAULT 0,
	last_attempt_at DATETIME,
	last_error TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	started_at DATETIME,
	finished_at DATETIME
);
`

func (db *DB) Migrate() error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	if err := db.migrateLegacyVideoFile(); err != nil {
		return err
	}
	if err := db.migrateEndpointType(); err != nil {
		return err
	}
	if err := db.migrateNostrStreamColumns(); err != nil {
		return err
	}
	if err := db.migrateGPUQueueColumns(); err != nil {
		return err
	}
	return db.seedDefaultNostrRelays()
}

// migrateLegacyVideoFile copies the old streams.video_file column into
// stream_videos (position 0) and drops the column. Idempotent: a no-op once
// the column is gone.
func (db *DB) migrateLegacyVideoFile() error {
	hasCol, err := db.hasColumn("streams", "video_file")
	if err != nil {
		return err
	}
	if !hasCol {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO stream_videos (stream_id, position, video_file)
		SELECT id, 0, video_file FROM streams
		WHERE video_file IS NOT NULL AND video_file <> ''
	`); err != nil {
		return fmt.Errorf("backfilling stream_videos: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE streams DROP COLUMN video_file`); err != nil {
		return fmt.Errorf("dropping legacy column: %w", err)
	}
	return tx.Commit()
}

func (db *DB) migrateEndpointType() error {
	hasCol, err := db.hasColumn("endpoints", "endpoint_type")
	if err != nil {
		return err
	}
	if hasCol {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE endpoints ADD COLUMN endpoint_type TEXT NOT NULL DEFAULT 'rtmp'`)
	return err
}

func (db *DB) migrateNostrStreamColumns() error {
	columns := []struct {
		name string
		sql  string
	}{
		{"nostr_enabled", `ALTER TABLE streams ADD COLUMN nostr_enabled INTEGER NOT NULL DEFAULT 0`},
		{"nostr_key_id", `ALTER TABLE streams ADD COLUMN nostr_key_id INTEGER`},
		{"nostr_title", `ALTER TABLE streams ADD COLUMN nostr_title TEXT NOT NULL DEFAULT ''`},
		{"nostr_summary", `ALTER TABLE streams ADD COLUMN nostr_summary TEXT NOT NULL DEFAULT ''`},
	}
	for _, col := range columns {
		hasCol, err := db.hasColumn("streams", col.name)
		if err != nil {
			return err
		}
		if hasCol {
			continue
		}
		if _, err := db.Exec(col.sql); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) migrateGPUQueueColumns() error {
	columns := []struct {
		name string
		sql  string
	}{
		{"attempt_count", `ALTER TABLE gpu_job_queue ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0`},
		{"last_attempt_at", `ALTER TABLE gpu_job_queue ADD COLUMN last_attempt_at DATETIME`},
		{"last_error", `ALTER TABLE gpu_job_queue ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`},
	}
	for _, col := range columns {
		hasCol, err := db.hasColumn("gpu_job_queue", col.name)
		if err != nil {
			return err
		}
		if hasCol {
			continue
		}
		if _, err := db.Exec(col.sql); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) seedDefaultNostrRelays() error {
	defaults := []string{
		"wss://relay.damus.io",
		"wss://nos.lol",
		"wss://relay.primal.net",
		"wss://relay.nostr.band",
	}
	for _, url := range defaults {
		if _, err := db.Exec(`INSERT OR IGNORE INTO nostr_relays (url, enabled) VALUES (?, 1)`, url); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) hasColumn(table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

type Endpoint struct {
	ID        int64
	Type      string
	Name      string
	RtmpURL   string
	StreamKey string
	Enabled   bool
	CreatedAt time.Time
}

type Stream struct {
	ID           int64
	Name         string
	Videos       []string // ordered playlist; populated on read
	ScheduleType string
	OnCalendar   string
	NostrEnabled bool
	NostrKeyID   int64
	NostrTitle   string
	NostrSummary string
	Enabled      bool
	CreatedAt    time.Time
	Endpoints    []Endpoint // populated on read
	NostrKey     *NostrKey
	NostrRelays  []NostrRelay
}

type NostrKey struct {
	ID         int64
	Name       string
	PubKey     string
	Npub       string
	SecretPath string
	Enabled    bool
	CreatedAt  time.Time
}

type NostrRelay struct {
	ID        int64
	URL       string
	Enabled   bool
	CreatedAt time.Time
}

type GPUJobLog struct {
	UnitName    string
	RawPath     string
	Host        string
	Description string
	ActiveState string
	SubState    string
	Result      string
	Journal     string
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type GPUJobQueueItem struct {
	ID            int64
	RawPath       string
	UnitName      string
	Status        string
	AttemptCount  int
	LastAttemptAt *time.Time
	LastError     string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// ---------- Endpoint queries ----------

func (db *DB) ListEndpoints() ([]Endpoint, error) {
	rows, err := db.Query(`SELECT id, endpoint_type, name, rtmp_url, stream_key, enabled, created_at FROM endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var enabled int
		if err := rows.Scan(&e.ID, &e.Type, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Enabled = enabled == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) GetEndpoint(id int64) (*Endpoint, error) {
	var e Endpoint
	var enabled int
	err := db.QueryRow(
		`SELECT id, endpoint_type, name, rtmp_url, stream_key, enabled, created_at FROM endpoints WHERE id = ?`, id,
	).Scan(&e.ID, &e.Type, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.Enabled = enabled == 1
	return &e, nil
}

func (db *DB) CreateEndpoint(e *Endpoint) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO endpoints (endpoint_type, name, rtmp_url, stream_key, enabled) VALUES (?, ?, ?, ?, ?)`,
		e.Type, e.Name, e.RtmpURL, e.StreamKey, boolInt(e.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateEndpoint(e *Endpoint) error {
	_, err := db.Exec(
		`UPDATE endpoints SET endpoint_type = ?, name = ?, rtmp_url = ?, stream_key = ?, enabled = ? WHERE id = ?`,
		e.Type, e.Name, e.RtmpURL, e.StreamKey, boolInt(e.Enabled), e.ID,
	)
	return err
}

func (db *DB) DeleteEndpoint(id int64) error {
	_, err := db.Exec(`DELETE FROM endpoints WHERE id = ?`, id)
	return err
}

// ---------- Stream queries ----------

func (db *DB) ListStreams() ([]Stream, error) {
	rows, err := db.Query(
		`SELECT id, name, schedule_type, on_calendar, nostr_enabled, nostr_key_id, nostr_title, nostr_summary, enabled, created_at FROM streams ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		var enabled, nostrEnabled int
		var nostrKeyID sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Name, &s.ScheduleType, &s.OnCalendar, &nostrEnabled, &nostrKeyID, &s.NostrTitle, &s.NostrSummary, &enabled, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled == 1
		s.NostrEnabled = nostrEnabled == 1
		if nostrKeyID.Valid {
			s.NostrKeyID = nostrKeyID.Int64
		}
		streams = append(streams, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range streams {
		eps, err := db.endpointsForStream(streams[i].ID)
		if err != nil {
			return nil, err
		}
		streams[i].Endpoints = eps

		vids, err := db.videosForStream(streams[i].ID)
		if err != nil {
			return nil, err
		}
		streams[i].Videos = vids
		if err := db.populateNostr(&streams[i]); err != nil {
			return nil, err
		}
	}
	return streams, nil
}

func (db *DB) GetStream(id int64) (*Stream, error) {
	var s Stream
	var enabled, nostrEnabled int
	var nostrKeyID sql.NullInt64
	err := db.QueryRow(
		`SELECT id, name, schedule_type, on_calendar, nostr_enabled, nostr_key_id, nostr_title, nostr_summary, enabled, created_at FROM streams WHERE id = ?`, id,
	).Scan(&s.ID, &s.Name, &s.ScheduleType, &s.OnCalendar, &nostrEnabled, &nostrKeyID, &s.NostrTitle, &s.NostrSummary, &enabled, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled == 1
	s.NostrEnabled = nostrEnabled == 1
	if nostrKeyID.Valid {
		s.NostrKeyID = nostrKeyID.Int64
	}

	eps, err := db.endpointsForStream(s.ID)
	if err != nil {
		return nil, err
	}
	s.Endpoints = eps

	vids, err := db.videosForStream(s.ID)
	if err != nil {
		return nil, err
	}
	s.Videos = vids
	if err := db.populateNostr(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (db *DB) populateNostr(s *Stream) error {
	if s.NostrKeyID != 0 {
		key, err := db.GetNostrKey(s.NostrKeyID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			s.NostrKey = key
		}
	}
	relays, err := db.EnabledNostrRelays()
	if err != nil {
		return err
	}
	s.NostrRelays = relays
	return nil
}

func (db *DB) videosForStream(streamID int64) ([]string, error) {
	rows, err := db.Query(
		`SELECT video_file FROM stream_videos WHERE stream_id = ? ORDER BY position`, streamID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (db *DB) endpointsForStream(streamID int64) ([]Endpoint, error) {
	rows, err := db.Query(`
		SELECT e.id, e.endpoint_type, e.name, e.rtmp_url, e.stream_key, e.enabled, e.created_at
		FROM endpoints e
		JOIN stream_endpoints se ON se.endpoint_id = e.id
		WHERE se.stream_id = ?
		ORDER BY e.name
	`, streamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var enabled int
		if err := rows.Scan(&e.ID, &e.Type, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Enabled = enabled == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) CreateStream(s *Stream, endpointIDs []int64, videos []string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO streams (name, schedule_type, on_calendar, nostr_enabled, nostr_key_id, nostr_title, nostr_summary, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Name, s.ScheduleType, s.OnCalendar, boolInt(s.NostrEnabled), nullInt64(s.NostrKeyID), s.NostrTitle, s.NostrSummary, boolInt(s.Enabled),
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, epID := range endpointIDs {
		if _, err := tx.Exec(`INSERT INTO stream_endpoints (stream_id, endpoint_id) VALUES (?, ?)`, id, epID); err != nil {
			return 0, fmt.Errorf("linking endpoint %d: %w", epID, err)
		}
	}
	for i, v := range videos {
		if _, err := tx.Exec(`INSERT INTO stream_videos (stream_id, position, video_file) VALUES (?, ?, ?)`, id, i, v); err != nil {
			return 0, fmt.Errorf("inserting video %d (%s): %w", i, v, err)
		}
	}

	return id, tx.Commit()
}

func (db *DB) UpdateStream(s *Stream, endpointIDs []int64, videos []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE streams SET name = ?, schedule_type = ?, on_calendar = ?, nostr_enabled = ?, nostr_key_id = ?, nostr_title = ?, nostr_summary = ?, enabled = ? WHERE id = ?`,
		s.Name, s.ScheduleType, s.OnCalendar, boolInt(s.NostrEnabled), nullInt64(s.NostrKeyID), s.NostrTitle, s.NostrSummary, boolInt(s.Enabled), s.ID,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM stream_endpoints WHERE stream_id = ?`, s.ID); err != nil {
		return err
	}
	for _, epID := range endpointIDs {
		if _, err := tx.Exec(`INSERT INTO stream_endpoints (stream_id, endpoint_id) VALUES (?, ?)`, s.ID, epID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DELETE FROM stream_videos WHERE stream_id = ?`, s.ID); err != nil {
		return err
	}
	for i, v := range videos {
		if _, err := tx.Exec(`INSERT INTO stream_videos (stream_id, position, video_file) VALUES (?, ?, ?)`, s.ID, i, v); err != nil {
			return fmt.Errorf("inserting video %d (%s): %w", i, v, err)
		}
	}
	return tx.Commit()
}

func (db *DB) DeleteStream(id int64) error {
	_, err := db.Exec(`DELETE FROM streams WHERE id = ?`, id)
	return err
}

// ---------- GPU job log queries ----------

func (db *DB) UpsertGPUJobLog(job GPUJobLog) error {
	_, err := db.Exec(`
		INSERT INTO gpu_job_logs (
			unit_name, raw_path, host, description, active_state, sub_state, result, journal, error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(unit_name) DO UPDATE SET
			raw_path = excluded.raw_path,
			host = excluded.host,
			description = excluded.description,
			active_state = excluded.active_state,
			sub_state = excluded.sub_state,
			result = excluded.result,
			journal = excluded.journal,
			error = excluded.error,
			updated_at = CURRENT_TIMESTAMP
	`, job.UnitName, job.RawPath, job.Host, job.Description, job.ActiveState, job.SubState, job.Result, job.Journal, job.Error)
	return err
}

func (db *DB) GetGPUJobLog(unitName string) (*GPUJobLog, error) {
	var job GPUJobLog
	err := db.QueryRow(`
		SELECT unit_name, raw_path, host, description, active_state, sub_state, result, journal, error, created_at, updated_at
		FROM gpu_job_logs WHERE unit_name = ?
	`, unitName).Scan(
		&job.UnitName, &job.RawPath, &job.Host, &job.Description, &job.ActiveState, &job.SubState, &job.Result, &job.Journal, &job.Error, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (db *DB) ListGPUJobLogs(limit int) ([]GPUJobLog, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`
		SELECT unit_name, raw_path, host, description, active_state, sub_state, result, journal, error, created_at, updated_at
		FROM gpu_job_logs ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUJobLog
	for rows.Next() {
		var job GPUJobLog
		if err := rows.Scan(&job.UnitName, &job.RawPath, &job.Host, &job.Description, &job.ActiveState, &job.SubState, &job.Result, &job.Journal, &job.Error, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// ---------- GPU queue queries ----------

func (db *DB) EnqueueGPUJob(rawPath string) (*GPUJobQueueItem, error) {
	_, err := db.Exec(`
		INSERT INTO gpu_job_queue (raw_path, status)
		VALUES (?, 'queued')
		ON CONFLICT(raw_path) DO UPDATE SET
			status = CASE
				WHEN gpu_job_queue.status IN ('finished', 'failed', 'cancelled') THEN 'queued'
				ELSE gpu_job_queue.status
			END,
			unit_name = CASE
				WHEN gpu_job_queue.status IN ('finished', 'failed', 'cancelled') THEN ''
				ELSE gpu_job_queue.unit_name
			END,
			started_at = CASE
				WHEN gpu_job_queue.status IN ('finished', 'failed', 'cancelled') THEN NULL
				ELSE gpu_job_queue.started_at
			END,
			finished_at = CASE
				WHEN gpu_job_queue.status IN ('finished', 'failed', 'cancelled') THEN NULL
				ELSE gpu_job_queue.finished_at
			END
	`, rawPath)
	if err != nil {
		return nil, err
	}
	return db.GetGPUQueueItemByRawPath(rawPath)
}

func (db *DB) GetGPUQueueItemByRawPath(rawPath string) (*GPUJobQueueItem, error) {
	var item GPUJobQueueItem
	var lastAttemptAt, startedAt, finishedAt sql.NullTime
	err := db.QueryRow(`
		SELECT id, raw_path, unit_name, status, attempt_count, last_attempt_at, last_error, created_at, started_at, finished_at
		FROM gpu_job_queue WHERE raw_path = ?
	`, rawPath).Scan(&item.ID, &item.RawPath, &item.UnitName, &item.Status, &item.AttemptCount, &lastAttemptAt, &item.LastError, &item.CreatedAt, &startedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	item.LastAttemptAt = nullTimePtr(lastAttemptAt)
	item.StartedAt = nullTimePtr(startedAt)
	item.FinishedAt = nullTimePtr(finishedAt)
	return &item, nil
}

func (db *DB) GetGPUQueueItemByUnitName(unitName string) (*GPUJobQueueItem, error) {
	var item GPUJobQueueItem
	var lastAttemptAt, startedAt, finishedAt sql.NullTime
	err := db.QueryRow(`
		SELECT id, raw_path, unit_name, status, attempt_count, last_attempt_at, last_error, created_at, started_at, finished_at
		FROM gpu_job_queue WHERE unit_name = ?
	`, unitName).Scan(&item.ID, &item.RawPath, &item.UnitName, &item.Status, &item.AttemptCount, &lastAttemptAt, &item.LastError, &item.CreatedAt, &startedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	item.LastAttemptAt = nullTimePtr(lastAttemptAt)
	item.StartedAt = nullTimePtr(startedAt)
	item.FinishedAt = nullTimePtr(finishedAt)
	return &item, nil
}

func (db *DB) NextQueuedGPUJob() (*GPUJobQueueItem, error) {
	var item GPUJobQueueItem
	var lastAttemptAt, startedAt, finishedAt sql.NullTime
	err := db.QueryRow(`
		SELECT id, raw_path, unit_name, status, attempt_count, last_attempt_at, last_error, created_at, started_at, finished_at
		FROM gpu_job_queue WHERE status = 'queued'
		ORDER BY created_at, id LIMIT 1
	`).Scan(&item.ID, &item.RawPath, &item.UnitName, &item.Status, &item.AttemptCount, &lastAttemptAt, &item.LastError, &item.CreatedAt, &startedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	item.LastAttemptAt = nullTimePtr(lastAttemptAt)
	item.StartedAt = nullTimePtr(startedAt)
	item.FinishedAt = nullTimePtr(finishedAt)
	return &item, nil
}

func (db *DB) ListOpenGPUQueueItems(limit int) ([]GPUJobQueueItem, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`
		SELECT id, raw_path, unit_name, status, attempt_count, last_attempt_at, last_error, created_at, started_at, finished_at
		FROM gpu_job_queue
		WHERE status IN ('queued', 'running')
		ORDER BY created_at, id LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUJobQueueItem
	for rows.Next() {
		var item GPUJobQueueItem
		var lastAttemptAt, startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.RawPath, &item.UnitName, &item.Status, &item.AttemptCount, &lastAttemptAt, &item.LastError, &item.CreatedAt, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		item.LastAttemptAt = nullTimePtr(lastAttemptAt)
		item.StartedAt = nullTimePtr(startedAt)
		item.FinishedAt = nullTimePtr(finishedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (db *DB) ListRecentGPUQueueItems(limit int) ([]GPUJobQueueItem, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := db.Query(`
		SELECT id, raw_path, unit_name, status, attempt_count, last_attempt_at, last_error, created_at, started_at, finished_at
		FROM gpu_job_queue
		ORDER BY
			CASE status
				WHEN 'running' THEN 0
				WHEN 'queued' THEN 1
				WHEN 'failed' THEN 2
				WHEN 'cancelled' THEN 3
				ELSE 4
			END,
			COALESCE(last_attempt_at, started_at, finished_at, created_at) DESC,
			id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUJobQueueItem
	for rows.Next() {
		var item GPUJobQueueItem
		var lastAttemptAt, startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.RawPath, &item.UnitName, &item.Status, &item.AttemptCount, &lastAttemptAt, &item.LastError, &item.CreatedAt, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		item.LastAttemptAt = nullTimePtr(lastAttemptAt)
		item.StartedAt = nullTimePtr(startedAt)
		item.FinishedAt = nullTimePtr(finishedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (db *DB) GPUQueueStatusCounts() (map[string]int, error) {
	rows, err := db.Query(`SELECT status, count(*) FROM gpu_job_queue GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

func (db *DB) MarkGPUQueueRunning(id int64, unitName string) error {
	_, err := db.Exec(`
		UPDATE gpu_job_queue
		SET status = 'running',
			unit_name = ?,
			attempt_count = attempt_count + 1,
			last_attempt_at = CURRENT_TIMESTAMP,
			last_error = '',
			started_at = CURRENT_TIMESTAMP,
			finished_at = NULL
		WHERE id = ?
	`, unitName, id)
	return err
}

func (db *DB) RequeueRunningGPUJob(rawPath, lastError string) error {
	result, err := db.Exec(`
		UPDATE gpu_job_queue
		SET status = 'queued', unit_name = '', last_error = ?, started_at = NULL, finished_at = NULL
		WHERE raw_path = ? AND status = 'running'
	`, lastError, rawPath)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) MarkGPUQueueFinished(rawPath, status, lastError string) error {
	if status == "" {
		status = "finished"
	}
	result, err := db.Exec(`
		UPDATE gpu_job_queue
		SET status = ?, last_error = ?, finished_at = CURRENT_TIMESTAMP
		WHERE raw_path = ? AND status IN ('queued', 'running')
	`, status, lastError, rawPath)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// ---------- Nostr queries ----------

func (db *DB) ListNostrKeys() ([]NostrKey, error) {
	rows, err := db.Query(`SELECT id, name, pubkey, npub, secret_path, enabled, created_at FROM nostr_keys ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NostrKey
	for rows.Next() {
		var k NostrKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.PubKey, &k.Npub, &k.SecretPath, &enabled, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		out = append(out, k)
	}
	return out, rows.Err()
}

func (db *DB) GetNostrKey(id int64) (*NostrKey, error) {
	var k NostrKey
	var enabled int
	err := db.QueryRow(
		`SELECT id, name, pubkey, npub, secret_path, enabled, created_at FROM nostr_keys WHERE id = ?`, id,
	).Scan(&k.ID, &k.Name, &k.PubKey, &k.Npub, &k.SecretPath, &enabled, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	return &k, nil
}

func (db *DB) CreateNostrKey(k *NostrKey) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO nostr_keys (name, pubkey, npub, secret_path, enabled) VALUES (?, ?, ?, ?, ?)`,
		k.Name, k.PubKey, k.Npub, k.SecretPath, boolInt(k.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateNostrKey(k *NostrKey) error {
	_, err := db.Exec(
		`UPDATE nostr_keys SET name = ?, enabled = ? WHERE id = ?`,
		k.Name, boolInt(k.Enabled), k.ID,
	)
	return err
}

func (db *DB) DeleteNostrKey(id int64) error {
	_, err := db.Exec(`DELETE FROM nostr_keys WHERE id = ?`, id)
	return err
}

func (db *DB) ListNostrRelays() ([]NostrRelay, error) {
	rows, err := db.Query(`SELECT id, url, enabled, created_at FROM nostr_relays ORDER BY url`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NostrRelay
	for rows.Next() {
		var relay NostrRelay
		var enabled int
		if err := rows.Scan(&relay.ID, &relay.URL, &enabled, &relay.CreatedAt); err != nil {
			return nil, err
		}
		relay.Enabled = enabled == 1
		out = append(out, relay)
	}
	return out, rows.Err()
}

func (db *DB) EnabledNostrRelays() ([]NostrRelay, error) {
	rows, err := db.Query(`SELECT id, url, enabled, created_at FROM nostr_relays WHERE enabled = 1 ORDER BY url`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NostrRelay
	for rows.Next() {
		var relay NostrRelay
		var enabled int
		if err := rows.Scan(&relay.ID, &relay.URL, &enabled, &relay.CreatedAt); err != nil {
			return nil, err
		}
		relay.Enabled = enabled == 1
		out = append(out, relay)
	}
	return out, rows.Err()
}

func (db *DB) CreateNostrRelay(relay *NostrRelay) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO nostr_relays (url, enabled) VALUES (?, ?)`,
		relay.URL, boolInt(relay.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateNostrRelay(relay *NostrRelay) error {
	_, err := db.Exec(
		`UPDATE nostr_relays SET url = ?, enabled = ? WHERE id = ?`,
		relay.URL, boolInt(relay.Enabled), relay.ID,
	)
	return err
}

func (db *DB) DeleteNostrRelay(id int64) error {
	_, err := db.Exec(`DELETE FROM nostr_relays WHERE id = ?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
