package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// Change is one committed write to a record. Seq numbers writes in commit
// order across all records.
type Change struct {
	Seq    int64     `json:"seq"`
	Action string    `json:"action"` // created or updated
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
	// Record is the record as this write left it, without its text.
	Record ChangedRecord `json:"record"`
}

// ChangedRecord identifies a changed record and says enough about it to
// decide whether to read the rest.
type ChangedRecord struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	ProjectID string `json:"project_id,omitempty"`
	Archived  bool   `json:"archived"`
	Version   int64  `json:"version"`
}

// ChangesOptions selects changes after a cursor.
type ChangesOptions struct {
	After     int64  // return changes numbered above this
	Kind      string // empty for every kind
	ProjectID string // empty for every project; otherwise that project's records only
	Limit     int    // default 100, 1–200
}

// ChangesResult is a page of changes. Next is the cursor to continue from.
type ChangesResult struct {
	Items []Change `json:"items"`
	Next  int64    `json:"next"`
}

// Changed returns a channel that is closed after the next committed record
// write. Take it before reading changes, so a write between the read and the
// wait is not missed.
func (s *Store) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Store) signalChange() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

// Changes lists writes numbered above o.After, oldest first. Next is the last
// change returned when the page is full; otherwise it is the newest change of
// any kind, so a filtered reader skips what can never match and a cursor ahead
// of the newest change, as after a restore, is moved back to it.
func (s *Store) Changes(ctx context.Context, o ChangesOptions) (ChangesResult, error) {
	if o.After < 0 {
		return ChangesResult{}, invalid("after must not be negative")
	}
	if o.Kind != "" && !namePattern.MatchString(o.Kind) {
		return ChangesResult{}, invalid("invalid kind filter")
	}
	if len(o.ProjectID) > 100 || !utf8.ValidString(o.ProjectID) {
		return ChangesResult{}, invalid("invalid project_id filter")
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
	if o.Limit < 1 || o.Limit > 200 {
		return ChangesResult{}, invalid("changes limit must be between 1 and 200")
	}
	// One read transaction, so the newest change and the page agree.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChangesResult{}, err
	}
	defer tx.Rollback()
	result := ChangesResult{Items: []Change{}}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM changes").Scan(&result.Next); err != nil {
		return ChangesResult{}, err
	}
	query := `SELECT c.seq, v.data FROM changes c JOIN revisions v ON v.record_id = c.record_id AND v.version = c.version WHERE c.seq > ?`
	args := []any{o.After}
	if o.Kind != "" {
		query += " AND json_extract(v.data, '$.record.kind') = ?"
		args = append(args, o.Kind)
	}
	if o.ProjectID != "" {
		query += " AND json_extract(v.data, '$.record.project_id') = ?"
		args = append(args, o.ProjectID)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY c.seq LIMIT ?", append(args, o.Limit)...)
	if err != nil {
		return ChangesResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Change
		var data string
		if err := rows.Scan(&c.Seq, &data); err != nil {
			return ChangesResult{}, err
		}
		var revision Revision
		if err := json.Unmarshal([]byte(data), &revision); err != nil {
			return ChangesResult{}, fmt.Errorf("decode revision: %w", err)
		}
		r := revision.Record
		c.Action, c.Actor, c.At = revision.Action, revision.Actor, revision.At
		c.Record = ChangedRecord{ID: r.ID, Kind: r.Kind, Title: r.Title, Status: r.Status, ProjectID: r.ProjectID, Archived: r.Archived, Version: r.Version}
		result.Items = append(result.Items, c)
	}
	if err := rows.Err(); err != nil {
		return ChangesResult{}, err
	}
	if len(result.Items) == o.Limit {
		result.Next = result.Items[len(result.Items)-1].Seq
	}
	return result, nil
}
