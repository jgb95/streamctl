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
	rtmp_url TEXT NOT NULL,
	stream_key TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS streams (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	schedule_type TEXT NOT NULL,         -- 'once' or 'recurring'
	on_calendar TEXT NOT NULL,           -- systemd OnCalendar expression
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
`

func (db *DB) Migrate() error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	return db.migrateLegacyVideoFile()
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
	Enabled      bool
	CreatedAt    time.Time
	Endpoints    []Endpoint // populated on read
}

// ---------- Endpoint queries ----------

func (db *DB) ListEndpoints() ([]Endpoint, error) {
	rows, err := db.Query(`SELECT id, name, rtmp_url, stream_key, enabled, created_at FROM endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var enabled int
		if err := rows.Scan(&e.ID, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt); err != nil {
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
		`SELECT id, name, rtmp_url, stream_key, enabled, created_at FROM endpoints WHERE id = ?`, id,
	).Scan(&e.ID, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.Enabled = enabled == 1
	return &e, nil
}

func (db *DB) CreateEndpoint(e *Endpoint) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO endpoints (name, rtmp_url, stream_key, enabled) VALUES (?, ?, ?, ?)`,
		e.Name, e.RtmpURL, e.StreamKey, boolInt(e.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateEndpoint(e *Endpoint) error {
	_, err := db.Exec(
		`UPDATE endpoints SET name = ?, rtmp_url = ?, stream_key = ?, enabled = ? WHERE id = ?`,
		e.Name, e.RtmpURL, e.StreamKey, boolInt(e.Enabled), e.ID,
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
		`SELECT id, name, schedule_type, on_calendar, enabled, created_at FROM streams ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		var enabled int
		if err := rows.Scan(&s.ID, &s.Name, &s.ScheduleType, &s.OnCalendar, &enabled, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled == 1
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
	}
	return streams, nil
}

func (db *DB) GetStream(id int64) (*Stream, error) {
	var s Stream
	var enabled int
	err := db.QueryRow(
		`SELECT id, name, schedule_type, on_calendar, enabled, created_at FROM streams WHERE id = ?`, id,
	).Scan(&s.ID, &s.Name, &s.ScheduleType, &s.OnCalendar, &enabled, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled == 1

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
	return &s, nil
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
		SELECT e.id, e.name, e.rtmp_url, e.stream_key, e.enabled, e.created_at
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
		if err := rows.Scan(&e.ID, &e.Name, &e.RtmpURL, &e.StreamKey, &enabled, &e.CreatedAt); err != nil {
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
		`INSERT INTO streams (name, schedule_type, on_calendar, enabled) VALUES (?, ?, ?, ?)`,
		s.Name, s.ScheduleType, s.OnCalendar, boolInt(s.Enabled),
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
		`UPDATE streams SET name = ?, schedule_type = ?, on_calendar = ?, enabled = ? WHERE id = ?`,
		s.Name, s.ScheduleType, s.OnCalendar, boolInt(s.Enabled), s.ID,
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
