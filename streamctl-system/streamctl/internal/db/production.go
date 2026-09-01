package db

import (
	"fmt"
	"strings"
	"time"
)

// ProductionCut is one retained source range for a conference talk. Source is
// a Spaces object key; chunkedVideo points at the first file in the sequence.
type ProductionCut struct {
	TalkID     string    `json:"talkId"`
	Position   int       `json:"position"`
	Source     string    `json:"source"`
	SourceType string    `json:"sourceType"`
	InMS       int64     `json:"inMs"`
	OutMS      int64     `json:"outMs"`
	UpdatedAt  time.Time `json:"-"`
}

func (db *DB) ListProductionCuts(conference string) (map[string][]ProductionCut, error) {
	rows, err := db.Query(`
		SELECT talk_id, position, source_object_key, source_type, in_ms, out_ms, updated_at
		FROM production_cuts
		WHERE conference = ?
		ORDER BY talk_id, position
	`, strings.TrimSpace(conference))
	if err != nil {
		return nil, fmt.Errorf("list production cuts: %w", err)
	}
	defer rows.Close()
	cuts := make(map[string][]ProductionCut)
	for rows.Next() {
		var cut ProductionCut
		if err := rows.Scan(&cut.TalkID, &cut.Position, &cut.Source, &cut.SourceType, &cut.InMS, &cut.OutMS, &cut.UpdatedAt); err != nil {
			return nil, err
		}
		cuts[cut.TalkID] = append(cuts[cut.TalkID], cut)
	}
	return cuts, rows.Err()
}

// ReplaceProductionCuts atomically replaces every retained range for a talk.
func (db *DB) ReplaceProductionCuts(conference, talkID string, cuts []ProductionCut) error {
	conference = strings.TrimSpace(conference)
	talkID = strings.TrimSpace(talkID)
	if conference == "" || talkID == "" {
		return fmt.Errorf("conference and talk ID are required")
	}
	for i := range cuts {
		cuts[i].Source = strings.TrimSpace(cuts[i].Source)
		if cuts[i].SourceType == "" {
			cuts[i].SourceType = "video"
		}
		if !strings.HasPrefix(cuts[i].Source, conference+"/recordings/") {
			return fmt.Errorf("cut %d source must be inside %s/recordings", i+1, conference)
		}
		if cuts[i].SourceType != "video" && cuts[i].SourceType != "chunkedVideo" {
			return fmt.Errorf("cut %d source type is invalid", i+1)
		}
		if cuts[i].InMS < 0 || cuts[i].OutMS <= cuts[i].InMS {
			return fmt.Errorf("cut %d must have an out point later than its in point", i+1)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM production_cuts WHERE conference = ? AND talk_id = ?`, conference, talkID); err != nil {
		return fmt.Errorf("clear production cuts: %w", err)
	}
	for i, cut := range cuts {
		if _, err := tx.Exec(`
			INSERT INTO production_cuts (conference, talk_id, position, source_object_key, source_type, in_ms, out_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, conference, talkID, i, cut.Source, cut.SourceType, cut.InMS, cut.OutMS); err != nil {
			return fmt.Errorf("save production cut %d: %w", i+1, err)
		}
	}
	return tx.Commit()
}
