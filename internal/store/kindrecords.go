package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// lookupKind finds a built-in kind or the unarchived kind record with that name.
func lookupKind(ctx context.Context, q queryer, name string) (Kind, bool, error) {
	if k, ok := builtinKinds[name]; ok {
		return k, true, nil
	}
	var data string
	err := q.QueryRowContext(ctx, `SELECT data FROM records WHERE kind = 'kind' AND archived = 0 AND json_extract(data, '$.title') = ?`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Kind{}, false, nil
	}
	if err != nil {
		return Kind{}, false, err
	}
	r, err := decodeRecord(data)
	if err != nil {
		return Kind{}, false, err
	}
	k, err := kindFromRecord(r)
	return k, err == nil, err
}

// resolveKind returns the kind a write uses. New records need an accepted
// kind; existing records of a retired kind stay editable.
func resolveKind(ctx context.Context, q queryer, name string, creating bool) (Kind, error) {
	k, found, err := lookupKind(ctx, q, name)
	if err != nil {
		return Kind{}, err
	}
	if !found {
		return Kind{}, invalid(fmt.Sprintf("unknown kind %q", name))
	}
	switch {
	case k.Status == "proposed":
		return Kind{}, invalid(fmt.Sprintf("kind %q is proposed, not yet accepted", name))
	case k.Status == "retired" && creating:
		return Kind{}, invalid(fmt.Sprintf("kind %q is retired", name))
	}
	return k, nil
}

func kindFromRecord(r Record) (Kind, error) {
	d, err := definitionFrom("fields.definition", r.Fields["definition"])
	if err != nil {
		return Kind{}, fmt.Errorf("kind record %s: %w", r.ID, err)
	}
	_, pending := r.Fields["pending"]
	return Kind{
		Name: r.Title, Status: r.Status, ProjectID: r.ProjectID, RecordID: r.ID, Body: r.Body,
		Definition: d, Pending: pending, Heading: r.Title, ContextGlobal: true,
	}, nil
}

// kindsFrom lists the built-in kinds in their fixed order, then every
// unarchived kind record by name.
func kindsFrom(ctx context.Context, q queryer) ([]Kind, error) {
	kinds := make([]Kind, 0, len(builtinOrder))
	for _, name := range builtinOrder {
		kinds = append(kinds, builtinKinds[name])
	}
	rows, err := q.QueryContext(ctx, `SELECT data FROM records WHERE kind = 'kind' AND archived = 0 ORDER BY json_extract(data, '$.title')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		r, err := decodeRecord(data)
		if err != nil {
			return nil, err
		}
		k, err := kindFromRecord(r)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, k)
	}
	return kinds, rows.Err()
}

// Kinds lists every built-in kind and every unarchived kind record.
func (s *Store) Kinds(ctx context.Context) ([]Kind, error) { return kindsFrom(ctx, s.db) }

// normalizeKindFields validates a kind record's definition and pending
// documents and stores them in canonical form.
func normalizeKindFields(fields map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if name != "definition" && name != "pending" {
			return nil, invalid("fields." + name + ": unknown field")
		}
		d, err := definitionFrom("fields."+name, fields[name])
		if err != nil {
			return nil, err
		}
		out[name] = definitionValue(d)
	}
	if _, ok := out["definition"]; !ok {
		return nil, invalid("fields.definition: required")
	}
	return out, nil
}

func validateKindName(r Record) error {
	if !namePattern.MatchString(r.Title) {
		return invalid(fmt.Sprintf("a kind name must match %s", namePattern))
	}
	if _, ok := builtinKinds[r.Title]; ok {
		return invalid(fmt.Sprintf("%q is a built-in kind", r.Title))
	}
	// The text context labels the summed omissions of custom kinds "custom".
	if r.Title == "custom" {
		return invalid(`"custom" is a reserved kind name`)
	}
	return nil
}

// checkKindNameFree runs inside the write transaction for unarchived kind records.
func checkKindNameFree(ctx context.Context, tx *sql.Tx, r Record) error {
	var other string
	err := tx.QueryRowContext(ctx, `SELECT id FROM records WHERE kind = 'kind' AND archived = 0 AND id <> ? AND json_extract(data, '$.title') = ?`, r.ID, r.Title).Scan(&other)
	if err == nil {
		return invalid(fmt.Sprintf("kind %q already exists as %s", r.Title, other))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

var kindTransitions = map[string][]string{"proposed": {"accepted"}, "accepted": {"retired"}, "retired": {"accepted"}}

// authorizeKindUpdate applies the kind lifecycle. acceptingPending is true
// only for accept_pending by a reviewer (Update passes it only for one),
// which replaces the definition.
func authorizeKindUpdate(author Author, old, next Record, acceptingPending bool) error {
	if next.Title != old.Title || next.ProjectID != old.ProjectID {
		return invalid("a kind's name and project cannot change")
	}
	if next.Archived && !old.Archived && old.Status != "proposed" {
		return invalid("only a proposed kind can be archived; retire it instead")
	}
	if next.Archived && !old.Archived && next.Status != old.Status {
		return invalid("archive a proposed kind without changing its status")
	}
	if next.Status != old.Status {
		if author.Role != RoleReviewer {
			return forbidden("only a reviewer can change the status of a kind")
		}
		if !slices.Contains(kindTransitions[old.Status], next.Status) {
			return invalid(fmt.Sprintf("a kind cannot move from %s to %s", old.Status, next.Status))
		}
	}
	_, hasPending := next.Fields["pending"]
	if old.Status == "proposed" {
		if hasPending {
			return invalid("fields.pending: edit a proposed kind's definition directly")
		}
		return nil
	}
	if !acceptingPending && !reflect.DeepEqual(old.Fields["definition"], next.Fields["definition"]) {
		return invalid("fields.definition: change an accepted kind through fields.pending and accept_pending")
	}
	if author.Role != RoleReviewer && !sameExceptPending(old, next) {
		return forbidden("agents can change only fields.pending on an accepted kind")
	}
	return nil
}

func sameExceptPending(a, b Record) bool {
	strip := func(r Record) Record {
		r.Fields = maps.Clone(r.Fields)
		delete(r.Fields, "pending")
		return r
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// acceptPending replaces an accepted kind's definition with its pending
// change once every live record of the kind satisfies the new definition.
func acceptPending(ctx context.Context, tx *sql.Tx, author Author, r *Record) error {
	if r.Kind != "kind" {
		return invalid("accept_pending applies only to kind records")
	}
	if author.Role != RoleReviewer {
		return forbidden("only a reviewer can accept a pending kind change")
	}
	if r.Status == "proposed" {
		return invalid("a proposed kind has no pending change; accept the kind instead")
	}
	pending, ok := r.Fields["pending"]
	if !ok {
		return invalid("kind has no pending change")
	}
	oldDef, err := definitionFrom("fields.definition", r.Fields["definition"])
	if err != nil {
		return err
	}
	newDef, err := definitionFrom("fields.pending", pending)
	if err != nil {
		return err
	}
	if err := checkKindChange(ctx, tx, *r, oldDef, newDef); err != nil {
		return err
	}
	r.Fields = maps.Clone(r.Fields)
	r.Fields["definition"] = pending
	delete(r.Fields, "pending")
	return refreshFieldSearch(ctx, tx, Kind{Name: r.Title, Definition: newDef})
}

// refreshFieldSearch re-indexes the typed fields of every live record of a
// kind under its new definition, without rewriting the records.
func refreshFieldSearch(ctx context.Context, tx *sql.Tx, k Kind) error {
	rows, err := tx.QueryContext(ctx, `SELECT data, search_rowid FROM records WHERE kind = ? AND archived = 0`, k.Name)
	if err != nil {
		return err
	}
	type entry struct {
		rowid int64
		text  string
	}
	var entries []entry
	for rows.Next() {
		var data string
		var rowid int64
		if err := rows.Scan(&data, &rowid); err != nil {
			rows.Close()
			return err
		}
		rec, err := decodeRecord(data)
		if err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, entry{rowid, searchText(k, rec.Fields)})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `UPDATE record_search SET fields = ? WHERE rowid = ?`, e.text, e.rowid); err != nil {
			return err
		}
	}
	return nil
}

// checkKindChange refuses a definition that a live record of the kind would
// violate, naming up to five of them.
func checkKindChange(ctx context.Context, tx *sql.Tx, kindRecord Record, oldDef, newDef Definition) error {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM records WHERE kind = ? AND archived = 0 ORDER BY id`, kindRecord.Title)
	if err != nil {
		return err
	}
	defer rows.Close()
	candidate := Kind{Name: kindRecord.Title, Definition: newDef}
	var failures []string
	seen := map[string]map[string]string{} // field -> value -> first record ID
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		rec, err := decodeRecord(data)
		if err != nil {
			return err
		}
		fields, err := normalizeFields(newDef, rec.Fields)
		switch {
		case err != nil:
			failures = append(failures, rec.ID+" "+err.Error())
			continue
		case !candidate.allows(rec.Status):
			failures = append(failures, fmt.Sprintf("%s status %s is no longer allowed", rec.ID, rec.Status))
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(newDef.Schema)) {
			value, ok := fields[name]
			if !newDef.Schema[name].Unique || !ok {
				continue
			}
			key := fmt.Sprint(value)
			if seen[name] == nil {
				seen[name] = map[string]string{}
			}
			if first, dup := seen[name][key]; dup {
				failures = append(failures, fmt.Sprintf("%s fields.%s duplicates %s", rec.ID, name, first))
			} else {
				seen[name][key] = rec.ID
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if oldDef.Policy.Review != newDef.Policy.Review {
		// Archived records keep their statuses and can be restored, so they count here.
		var total int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE kind = ?`, kindRecord.Title).Scan(&total); err != nil {
			return err
		}
		if total > 0 {
			return invalid(fmt.Sprintf("review cannot change while records of kind %q exist", kindRecord.Title))
		}
	}
	if len(failures) > 0 {
		shown := failures[:min(5, len(failures))]
		return invalid(fmt.Sprintf("change breaks %d records (showing %d): %s", len(failures), len(shown), strings.Join(shown, "; ")))
	}
	return nil
}

// checkUnique runs inside the write transaction, which serializes it against
// concurrent writes to the same kind.
func checkUnique(ctx context.Context, tx *sql.Tx, k Kind, r Record) error {
	if r.Archived {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(k.Definition.Schema)) {
		value, ok := r.Fields[name]
		if !k.Definition.Schema[name].Unique || !ok {
			continue
		}
		if n, isNumber := value.(float64); isNumber {
			value = int64(n) // unique numbers are integers; JSON stores them without a fraction
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM records WHERE kind = ? AND archived = 0 AND id <> ? AND json_extract(data, ?) = ? ORDER BY id LIMIT 1`,
			r.Kind, r.ID, "$.fields."+name, value).Scan(&existing)
		if err == nil {
			return &DuplicateError{Field: name, ExistingID: existing}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}
