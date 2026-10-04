package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RecallOptions selects reviewed knowledge for relevance-ranked recall.
type RecallOptions struct {
	Query           string
	ProjectID       string // empty searches every project
	IncludeProposed bool
	Limit           int // default 20, 1–100
}

const maxRecallWords = 32

// recallQuery turns plain text into quoted FTS5 term lists joined with OR, so
// any word can match and syntax in the query is never interpreted. whole holds
// the whole-word terms; stems holds the four-rune stems of longer words, which
// find shorter inflections and are searched only after the whole-word terms.
// The word cap counts distinct words of two or more runes.
func recallQuery(query string) (whole, stems string, err error) {
	fields := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) })
	seenWord := map[string]bool{}
	var words []string
	for _, word := range fields {
		if utf8.RuneCountInString(word) >= 2 && !seenWord[word] {
			seenWord[word] = true
			words = append(words, word)
		}
	}
	if len(words) > maxRecallWords {
		return "", "", invalid(fmt.Sprintf("recall queries support at most %d words", maxRecallWords))
	}
	if len(words) == 0 {
		return "", "", invalid("recall query needs at least one word of two or more characters")
	}
	quote := func(word string, prefix bool) string {
		term := `"` + strings.ReplaceAll(word, `"`, `""`) + `"`
		if prefix {
			term += "*"
		}
		return term
	}
	var wholeTerms, stemTerms []string
	seenTerm := map[string]bool{}
	for _, word := range words {
		runes := []rune(word)
		term := quote(word, len(runes) >= 4)
		seenTerm[term] = true
		wholeTerms = append(wholeTerms, term)
	}
	for _, word := range words {
		runes := []rune(word)
		if len(runes) < 5 {
			continue
		}
		term := quote(string(runes[:4]), true)
		if !seenTerm[term] {
			seenTerm[term] = true
			stemTerms = append(stemTerms, term)
		}
	}
	return strings.Join(wholeTerms, " OR "), strings.Join(stemTerms, " OR "), nil
}

// Recalled is a recall result. Reviewed is true when a reviewer accepted it;
// records of kinds without review are returned unreviewed.
type Recalled struct {
	Record
	Reviewed bool
}

// Recall ranks accepted knowledge and principles, and records of other kinds
// with recall enabled, by relevance.
func (s *Store) Recall(ctx context.Context, o RecallOptions) ([]Recalled, error) {
	o.Query = strings.TrimSpace(o.Query)
	if len(o.Query) > 500 || !utf8.ValidString(o.Query) {
		return nil, invalid("recall query must be valid UTF-8 and at most 500 bytes")
	}
	if len(o.ProjectID) > 100 || !utf8.ValidString(o.ProjectID) {
		return nil, invalid("invalid project_id filter")
	}
	if o.Limit == 0 {
		o.Limit = 20
	}
	if o.Limit < 1 || o.Limit > 100 {
		return nil, invalid("recall limit must be between 1 and 100")
	}
	whole, stems, err := recallQuery(o.Query)
	if err != nil {
		return nil, err
	}
	kinds, err := kindsFrom(ctx, s.db)
	if err != nil {
		return nil, err
	}
	var reviewed, open []string
	for _, k := range kinds {
		if !k.Definition.Policy.Recall || k.Status == "proposed" {
			continue
		}
		if k.review() {
			reviewed = append(reviewed, k.Name)
		} else {
			open = append(open, k.Name)
		}
	}
	statuses := "'accepted'"
	if o.IncludeProposed {
		statuses = "'accepted', 'proposed'"
	}
	records := []Recalled{}
	seen := map[string]bool{}
	for _, match := range []string{whole, stems} {
		if match == "" || len(records) >= o.Limit {
			continue
		}
		found, err := s.recallPass(ctx, o, reviewed, open, statuses, match, seen, o.Limit-len(records))
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
	}
	return records, nil
}

// recallPass runs one ranked query, skipping ids already returned by an
// earlier pass and recording the ids it returns.
func (s *Store) recallPass(ctx context.Context, o RecallOptions, reviewed, open []string, statuses, match string, seen map[string]bool, limit int) ([]Recalled, error) {
	placeholders := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	condition := "r.kind IN (" + placeholders(len(reviewed)) + ") AND r.status IN (" + statuses + ")"
	if len(open) > 0 {
		condition = "((" + condition + ") OR r.kind IN (" + placeholders(len(open)) + "))"
	}
	query := `SELECT r.data FROM record_search JOIN records r ON r.search_rowid = record_search.rowid
WHERE record_search MATCH ? AND ` + condition + ` AND r.archived = 0`
	args := []any{match}
	for _, name := range reviewed {
		args = append(args, name)
	}
	for _, name := range open {
		args = append(args, name)
	}
	if o.ProjectID != "" {
		query += " AND (r.project_id = ? OR r.project_id IS NULL)"
		args = append(args, o.ProjectID)
	}
	if len(seen) > 0 {
		query += " AND r.id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(seen)), ",") + ")"
		for id := range seen {
			args = append(args, id)
		}
	}
	query += ` ORDER BY bm25(record_search, 5.0, 1.0, 0.5), CASE WHEN r.project_id = ? THEN 0 ELSE 1 END, r.priority DESC, r.updated_ns DESC, r.id LIMIT ?`
	args = append(args, o.ProjectID, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Recalled{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		r, err := decodeRecord(data)
		if err != nil {
			return nil, err
		}
		seen[r.ID] = true
		records = append(records, Recalled{Record: r, Reviewed: slices.Contains(reviewed, r.Kind) && r.Status == "accepted"})
	}
	return records, rows.Err()
}
