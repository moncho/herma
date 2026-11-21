package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

// Usage is how often agents were given one record that recall can return.
type Usage struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	ProjectID  string     `json:"project_id,omitempty"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	// Recalled counts recall responses that included the record; Fetched
	// counts reads of the record by ID.
	Recalled   int64      `json:"recalled"`
	Fetched    int64      `json:"fetched"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// UsageOptions selects the records whose usage is listed.
type UsageOptions struct {
	ProjectID string // empty lists every project; otherwise that project and global records
	Limit     int    // default 100, 1–200
}

type UsageResult struct {
	Items []Usage `json:"items"`
	Total int     `json:"total"`
}

// CountRecalled notes that a recall response included these records.
func (s *Store) CountRecalled(ctx context.Context, ids []string) error {
	return s.countUse(ctx, ids, 1, 0)
}

// CountFetched notes that the record was read by ID.
func (s *Store) CountFetched(ctx context.Context, id string) error {
	return s.countUse(ctx, []string{id}, 0, 1)
}

func (s *Store) countUse(ctx context.Context, ids []string, recalled, fetched int) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixNano()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage(record_id, recalled, fetched, last_used_ns) VALUES(?, ?, ?, ?)
ON CONFLICT(record_id) DO UPDATE SET recalled = recalled + excluded.recalled, fetched = fetched + excluded.fetched, last_used_ns = excluded.last_used_ns`,
			id, recalled, fetched, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Usage lists the records recall can return with their counts, least used
// first and, among equally used records, the longest unchanged first.
func (s *Store) Usage(ctx context.Context, o UsageOptions) (UsageResult, error) {
	if len(o.ProjectID) > 100 || !utf8.ValidString(o.ProjectID) {
		return UsageResult{}, invalid("invalid project_id filter")
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
	if o.Limit < 1 || o.Limit > 200 {
		return UsageResult{}, invalid("usage limit must be between 1 and 200")
	}
	reviewed, open, err := s.recallKinds(ctx)
	if err != nil {
		return UsageResult{}, err
	}
	placeholders := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	where := "r.kind IN (" + placeholders(len(reviewed)) + ") AND r.status = 'accepted'"
	if len(open) > 0 {
		where = "((" + where + ") OR r.kind IN (" + placeholders(len(open)) + "))"
	}
	where += " AND r.archived = 0"
	args := []any{}
	for _, name := range reviewed {
		args = append(args, name)
	}
	for _, name := range open {
		args = append(args, name)
	}
	if o.ProjectID != "" {
		where += " AND (r.project_id = ? OR r.project_id IS NULL)"
		args = append(args, o.ProjectID)
	}
	result := UsageResult{Items: []Usage{}}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM records r WHERE "+where, args...).Scan(&result.Total); err != nil {
		return UsageResult{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.data, COALESCE(u.recalled, 0), COALESCE(u.fetched, 0), u.last_used_ns
FROM records r LEFT JOIN usage u ON u.record_id = r.id WHERE `+where+`
ORDER BY COALESCE(u.recalled, 0) + COALESCE(u.fetched, 0), r.updated_ns, r.id LIMIT ?`, append(args, o.Limit)...)
	if err != nil {
		return UsageResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		var u Usage
		var last sql.NullInt64
		if err := rows.Scan(&data, &u.Recalled, &u.Fetched, &last); err != nil {
			return UsageResult{}, err
		}
		r, err := decodeRecord(data)
		if err != nil {
			return UsageResult{}, err
		}
		u.ID, u.Kind, u.Title, u.ProjectID, u.ReviewedAt = r.ID, r.Kind, r.Title, r.ProjectID, r.ReviewedAt
		if last.Valid {
			at := time.Unix(0, last.Int64).UTC()
			u.LastUsedAt = &at
		}
		result.Items = append(result.Items, u)
	}
	return result, rows.Err()
}
