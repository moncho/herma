package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Store keeps record changes, revision history, and replay receipts atomic.
// A single connection serializes writes and also makes :memory: useful in tests.
type Store struct{ db *sql.DB }

// schemaVersion is the PRAGMA user_version written by the newest migration.
var schemaVersion = len(migrations)

// migrations[i] upgrades a database from user_version i to i+1. Each runs in
// one transaction with its version bump. Append new steps; never edit old ones.
var migrations = []string{
	// 1: the original unversioned schema. IF NOT EXISTS adopts databases
	// created before versioning without changing them.
	`CREATE TABLE IF NOT EXISTS records (
 id TEXT PRIMARY KEY,
 kind TEXT NOT NULL,
 project_id TEXT REFERENCES records(id),
 status TEXT NOT NULL,
 priority INTEGER NOT NULL,
 owner TEXT NOT NULL,
 archived INTEGER NOT NULL,
 updated_ns INTEGER NOT NULL,
 data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS records_project ON records(project_id);
CREATE INDEX IF NOT EXISTS records_order ON records(priority DESC, updated_ns DESC, id);
CREATE INDEX IF NOT EXISTS records_project_recent ON records(project_id, kind, archived, updated_ns DESC, id);
CREATE VIRTUAL TABLE IF NOT EXISTS record_search USING fts5(id UNINDEXED, title, body);
CREATE TABLE IF NOT EXISTS revisions (
 record_id TEXT NOT NULL REFERENCES records(id),
 version INTEGER NOT NULL,
 data TEXT NOT NULL,
 PRIMARY KEY(record_id, version)
);
CREATE TABLE IF NOT EXISTS idempotency (
 actor TEXT NOT NULL,
 key TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 data TEXT NOT NULL,
 PRIMARY KEY(actor, key)
);`,
	// 2: key search rows by FTS rowid. Deleting by the UNINDEXED id column
	// scanned the whole index on every write. The rowid is stored in records
	// because implicit table rowids may change during VACUUM.
	`ALTER TABLE records ADD COLUMN search_rowid INTEGER;
UPDATE records SET search_rowid = rowid;
CREATE UNIQUE INDEX records_search_rowid ON records(search_rowid);
DROP TABLE record_search;
CREATE VIRTUAL TABLE record_search USING fts5(title, body);
INSERT INTO record_search(rowid, title, body)
 SELECT search_rowid, json_extract(data, '$.title'), json_extract(data, '$.body') FROM records;`,
	// 3: receipts reference the revision written in the same transaction
	// instead of storing another complete record copy.
	`CREATE TABLE idempotency_compact (
 actor TEXT NOT NULL,
 key TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 record_id TEXT NOT NULL,
 version INTEGER NOT NULL,
 PRIMARY KEY(actor, key),
 FOREIGN KEY(record_id, version) REFERENCES revisions(record_id, version)
);
INSERT INTO idempotency_compact(actor, key, fingerprint, record_id, version)
 SELECT actor, key, fingerprint, json_extract(data, '$.id'), json_extract(data, '$.version') FROM idempotency;
DROP TABLE idempotency;
ALTER TABLE idempotency_compact RENAME TO idempotency;`,
}

// Open opens a local SQLite database. The parent directory must already exist.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, invalid("database path is required")
	}
	dsn := path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("database path: %w", err)
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute}).String()
	}
	// Connection-level pragmas also apply if database/sql replaces a connection.
	dsn += "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this herma supports (%d); use a newer herma build", version, len(migrations))
	}
	for ; version < len(migrations); version++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate schema to version %d: %w", version+1, err)
		}
		// PRAGMA values cannot be bound; version is a trusted integer.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate schema to version %d: %w", version+1, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Create(ctx context.Context, author Author, key string, input CreateInput) (Record, bool, error) {
	if err := validateAuthor(author, key); err != nil {
		return Record{}, false, err
	}
	fingerprint, err := requestFingerprint("create", "", input)
	if err != nil {
		return Record{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	if record, replay, err := replayRecord(ctx, tx, author.Name, key, fingerprint); replay || err != nil {
		return record, replay, err
	}
	r := Record{
		Kind: input.Kind, Title: input.Title, Body: input.Body, ProjectID: input.ProjectID,
		Status: input.Status, Priority: input.Priority, Owner: input.Owner,
		Tags: input.Tags, Links: input.Links, Sources: input.Sources,
		CreatedBy: author.Name, UpdatedBy: author.Name, Version: 1,
	}
	if r.Status == "" {
		r.Status = defaultStatus[r.Kind]
	}
	if err := normalizeRecord(&r); err != nil {
		return Record{}, false, err
	}
	if err := authorizeCreate(author, r); err != nil {
		return Record{}, false, err
	}
	if err := validateReferences(ctx, tx, r, ""); err != nil {
		return Record{}, false, err
	}
	randomID := make([]byte, 16)
	if _, err := rand.Read(randomID); err != nil {
		return Record{}, false, fmt.Errorf("generate record ID: %w", err)
	}
	r.ID = "rec_" + hex.EncodeToString(randomID)
	r.CreatedAt = time.Now().UTC()
	r.UpdatedAt = r.CreatedAt
	markReview(author, "proposed", &r, r.CreatedAt)
	if err := saveRecord(ctx, tx, r, "created", author.Name, key, fingerprint); err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return r, false, nil
}

func (s *Store) Update(ctx context.Context, id string, author Author, key string, input UpdateInput) (Record, bool, error) {
	if err := validateAuthor(author, key); err != nil {
		return Record{}, false, err
	}
	fingerprint, err := requestFingerprint("update", id, input)
	if err != nil {
		return Record{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	if record, replay, err := replayRecord(ctx, tx, author.Name, key, fingerprint); replay || err != nil {
		return record, replay, err
	}
	if input.Version <= 0 {
		return Record{}, false, invalid("version must be positive")
	}
	if !hasContentChanges(input) && input.Archived == nil {
		return Record{}, false, invalid("at least one field must be provided")
	}
	r, err := getRecord(ctx, tx, id)
	if err != nil {
		return Record{}, false, err
	}
	if r.Version != input.Version {
		return Record{}, false, ErrConflict
	}
	if r.Kind == "note" && hasContentChanges(input) {
		return Record{}, false, invalid("notes are append-only; only archived may be changed")
	}
	old := r
	oldProject := r.ProjectID
	if input.Title != nil {
		r.Title = *input.Title
	}
	if input.Body != nil {
		r.Body = *input.Body
	}
	if input.ProjectID != nil {
		r.ProjectID = *input.ProjectID
	}
	if input.Status != nil {
		r.Status = *input.Status
	}
	if input.Priority != nil {
		r.Priority = *input.Priority
	}
	if input.Owner != nil {
		r.Owner = *input.Owner
	}
	if input.Tags != nil {
		r.Tags = *input.Tags
	}
	if input.Links != nil {
		r.Links = *input.Links
	}
	if input.Sources != nil {
		r.Sources = *input.Sources
	}
	if input.Archived != nil {
		r.Archived = *input.Archived
	}
	if err := normalizeRecord(&r); err != nil {
		return Record{}, false, err
	}
	if err := authorizeUpdate(author, old, r); err != nil {
		return Record{}, false, err
	}
	if err := validateReferences(ctx, tx, r, oldProject); err != nil {
		return Record{}, false, err
	}
	r.Version++
	r.UpdatedBy = author.Name
	r.UpdatedAt = time.Now().UTC()
	markReview(author, old.Status, &r, r.UpdatedAt)
	if err := saveRecord(ctx, tx, r, "updated", author.Name, key, fingerprint); err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return r, false, nil
}

func (s *Store) Get(ctx context.Context, id string) (Record, error) { return getRecord(ctx, s.db, id) }

func (s *Store) List(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := validateList(&options); err != nil {
		return ListResult{}, err
	}
	conditions, args := []string{"1=1"}, []any{}
	add := func(condition string, value any) {
		conditions = append(conditions, condition)
		args = append(args, value)
	}
	if options.Kind != "" {
		add("r.kind = ?", options.Kind)
	}
	if options.ProjectID != "" {
		add("r.project_id = ?", options.ProjectID)
	}
	if options.Global {
		conditions = append(conditions, "r.project_id IS NULL")
	}
	if options.Status != "" {
		add("r.status = ?", options.Status)
	}
	if options.Owner != "" {
		add("r.owner = ?", options.Owner)
	}
	if options.Tag != "" {
		add("EXISTS (SELECT 1 FROM json_each(r.data, '$.tags') WHERE value = ?)", options.Tag)
	}
	if !options.Archived {
		conditions = append(conditions, "r.archived = 0")
	}
	if options.Query != "" {
		query := plainTextQuery(options.Query)
		if query == "" {
			conditions = append(conditions, "0=1")
		} else {
			add("r.search_rowid IN (SELECT rowid FROM record_search WHERE record_search MATCH ?)", query)
		}
	}
	where := " FROM records r WHERE " + strings.Join(conditions, " AND ")
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ListResult{}, err
	}
	defer tx.Rollback()
	result := ListResult{Items: []Record{}, Limit: options.Limit, Offset: options.Offset}
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&result.Total); err != nil {
		return ListResult{}, err
	}
	pageArgs := append(append([]any{}, args...), options.Limit, options.Offset)
	order := "r.priority DESC, r.updated_ns DESC, r.id ASC"
	if options.RecentFirst {
		order = "r.updated_ns DESC, r.id ASC"
	}
	rows, err := tx.QueryContext(ctx, "SELECT r.data"+where+" ORDER BY "+order+" LIMIT ? OFFSET ?", pageArgs...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return ListResult{}, err
		}
		var r Record
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			return ListResult{}, fmt.Errorf("decode record: %w", err)
		}
		result.Items = append(result.Items, r)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	if err := rows.Close(); err != nil {
		return ListResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ListResult{}, err
	}
	return result, nil
}

func (s *Store) History(ctx context.Context, id string) ([]Revision, error) {
	// Records are never deleted, so existence remains true across these reads.
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT data FROM revisions WHERE record_id = ? ORDER BY version ASC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var revision Revision
		if err := json.Unmarshal([]byte(data), &revision); err != nil {
			return nil, fmt.Errorf("decode revision: %w", err)
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getRecord(ctx context.Context, db rowQueryer, id string) (Record, error) {
	var data string
	err := db.QueryRowContext(ctx, "SELECT data FROM records WHERE id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return Record{}, fmt.Errorf("decode record: %w", err)
	}
	return r, nil
}

func saveRecord(ctx context.Context, tx *sql.Tx, r Record, action, actor, key, fingerprint string) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var project any
	if r.ProjectID != "" {
		project = r.ProjectID
	}
	// Each record owns one search row, addressed by its stored FTS rowid.
	var searchRowID int64
	err = tx.QueryRowContext(ctx, "SELECT search_rowid FROM records WHERE id = ?", r.ID).Scan(&searchRowID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		result, err := tx.ExecContext(ctx, "INSERT INTO record_search(title, body) VALUES (?, ?)", r.Title, r.Body)
		if err != nil {
			return err
		}
		if searchRowID, err = result.LastInsertId(); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.ExecContext(ctx, "UPDATE record_search SET title = ?, body = ? WHERE rowid = ?", r.Title, r.Body, searchRowID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO records(id, kind, project_id, status, priority, owner, archived, updated_ns, data, search_rowid)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id, status=excluded.status, priority=excluded.priority,
owner=excluded.owner, archived=excluded.archived, updated_ns=excluded.updated_ns, data=excluded.data`,
		r.ID, r.Kind, project, r.Status, r.Priority, r.Owner, r.Archived, r.UpdatedAt.UnixNano(), string(data), searchRowID)
	if err != nil {
		return err
	}
	revision, err := json.Marshal(Revision{Version: r.Version, Actor: actor, Action: action, At: r.UpdatedAt, Record: r})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO revisions(record_id, version, data) VALUES (?, ?, ?)", r.ID, r.Version, string(revision)); err != nil {
		return err
	}
	if key != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO idempotency(actor, key, fingerprint, record_id, version) VALUES (?, ?, ?, ?, ?)", actor, key, fingerprint, r.ID, r.Version); err != nil {
			return err
		}
	}
	return nil
}

func requestFingerprint(operation, id string, input any) (string, error) {
	data, err := json.Marshal(struct {
		Operation string
		ID        string
		Input     any
	}{operation, id, input})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func replayRecord(ctx context.Context, tx *sql.Tx, actor, key, fingerprint string) (Record, bool, error) {
	if key == "" {
		return Record{}, false, nil
	}
	// The revision holds the exact record snapshot this key originally returned.
	var previous, data string
	err := tx.QueryRowContext(ctx, `SELECT i.fingerprint, r.data FROM idempotency i
JOIN revisions r ON r.record_id = i.record_id AND r.version = i.version
WHERE i.actor = ? AND i.key = ?`, actor, key).Scan(&previous, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if previous != fingerprint {
		return Record{}, false, ErrIdempotency
	}
	var revision Revision
	if err := json.Unmarshal([]byte(data), &revision); err != nil {
		return Record{}, false, fmt.Errorf("decode replay: %w", err)
	}
	return revision.Record, true, nil
}

var defaultStatus = map[string]string{
	"knowledge": "proposed", "principle": "proposed", "project": "planned",
	"task": "open", "note": "published", "feedback": "open",
}

var statuses = map[string][]string{
	"knowledge": {"proposed", "accepted", "rejected", "superseded"},
	"principle": {"proposed", "accepted", "rejected", "superseded"},
	"project":   {"planned", "active", "paused", "completed"},
	"task":      {"open", "in_progress", "blocked", "done"},
	"note":      {"published"},
	"feedback":  {"open", "triaged", "resolved"},
}

func invalid(message string) error { return &ValidationError{Message: message} }

func forbidden(message string) error { return fmt.Errorf("%w: %s", ErrForbidden, message) }

func validateAuthor(author Author, key string) error {
	actor := author.Name
	if strings.TrimSpace(actor) == "" || len(actor) > 200 || !utf8.ValidString(actor) {
		return invalid("actor must be nonblank and at most 200 bytes")
	}
	if !author.Role.Valid() {
		return invalid("author role must be reviewer, agent, or read-only")
	}
	if author.Role == RoleReadOnly {
		return forbidden("read-only identities cannot write")
	}
	if len(key) > 200 || !utf8.ValidString(key) || (key != "" && strings.TrimSpace(key) == "") {
		return invalid("idempotency key must be nonblank and at most 200 bytes when supplied")
	}
	return nil
}

// durable kinds hold knowledge meant to outlive sessions and need review.
func durable(kind string) bool { return kind == "knowledge" || kind == "principle" }

func authorizeCreate(author Author, r Record) error {
	if !durable(r.Kind) || author.Role == RoleReviewer || r.Status == "proposed" {
		return nil
	}
	return forbidden(fmt.Sprintf("only a reviewer can create %s with status %s", r.Kind, r.Status))
}

// authorizeUpdate runs inside the write transaction against the committed
// record, so a concurrent status change cannot slip past it.
func authorizeUpdate(author Author, old, next Record) error {
	if !durable(old.Kind) || author.Role == RoleReviewer {
		return nil
	}
	if old.Status != "proposed" {
		return forbidden(fmt.Sprintf("only a reviewer can change %s %s", old.Status, old.Kind))
	}
	if next.Status != old.Status {
		return forbidden(fmt.Sprintf("only a reviewer can change the status of %s", old.Kind))
	}
	return nil
}

// markReview records who last judged a durable record. Only reviewers can
// reach a status other than proposed, so author is a reviewer when it applies.
func markReview(author Author, previousStatus string, r *Record, at time.Time) {
	if !durable(r.Kind) || r.Status == previousStatus {
		return
	}
	if r.Status == "proposed" {
		r.ReviewedBy, r.ReviewedAt = "", nil
		return
	}
	r.ReviewedBy, r.ReviewedAt = author.Name, &at
}

func validStatus(kind, status string) bool {
	for k, values := range statuses {
		if kind != "" && kind != k {
			continue
		}
		for _, value := range values {
			if value == status {
				return true
			}
		}
	}
	return false
}

func normalizeRecord(r *Record) error {
	if _, ok := defaultStatus[r.Kind]; !ok {
		return invalid("kind must be knowledge, principle, project, task, note, or feedback")
	}
	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" || !utf8.ValidString(r.Title) || utf8.RuneCountInString(r.Title) > 300 {
		return invalid("title must contain 1 to 300 characters")
	}
	if len(r.Body) > 64*1024 || !utf8.ValidString(r.Body) {
		return invalid("body must be valid UTF-8 and at most 64 KiB")
	}
	if !validStatus(r.Kind, r.Status) {
		return invalid("status is invalid for this record kind")
	}
	if r.Priority < 0 || r.Priority > 5 {
		return invalid("priority must be between 0 and 5")
	}
	r.Owner = strings.TrimSpace(r.Owner)
	if len(r.Owner) > 200 || !utf8.ValidString(r.Owner) {
		return invalid("owner must be valid UTF-8 and at most 200 bytes")
	}
	if len(r.ProjectID) > 100 || !utf8.ValidString(r.ProjectID) {
		return invalid("project_id must be at most 100 bytes")
	}
	if r.Kind == "project" && r.ProjectID != "" {
		return invalid("a project cannot belong to another project")
	}
	var err error
	if r.Tags, err = normalizeStrings(r.Tags, "tags", 32, 64, true); err != nil {
		return err
	}
	if r.Links, err = normalizeStrings(r.Links, "links", 100, 100, false); err != nil {
		return err
	}
	if r.Sources, err = normalizeStrings(r.Sources, "sources", 50, 2048, false); err != nil {
		return err
	}
	if durable(r.Kind) && r.Status == "accepted" && len(r.Sources) == 0 {
		return invalid(fmt.Sprintf("accepted %s requires at least one source", r.Kind))
	}
	return nil
}

func normalizeStrings(values []string, field string, maxItems, maxBytes int, lower bool) ([]string, error) {
	if len(values) > maxItems {
		return nil, invalid(fmt.Sprintf("%s supports at most %d items", field, maxItems))
	}
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
			return nil, invalid(fmt.Sprintf("%s entries must be nonblank valid UTF-8 and at most %d bytes", field, maxBytes))
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, nil
}

func validateReferences(ctx context.Context, tx *sql.Tx, r Record, oldProject string) error {
	if r.ProjectID != "" && r.ProjectID != oldProject {
		project, err := getRecord(ctx, tx, r.ProjectID)
		if errors.Is(err, ErrNotFound) {
			return invalid("project_id must reference an existing project")
		}
		if err != nil {
			return err
		}
		if project.Kind != "project" || project.Archived {
			return invalid("project_id must reference an unarchived project")
		}
	}
	for _, link := range r.Links {
		if link == r.ID {
			return invalid("links must not reference the record itself")
		}
		if _, err := getRecord(ctx, tx, link); err != nil {
			if errors.Is(err, ErrNotFound) {
				return invalid("links must reference existing records")
			}
			return err
		}
	}
	return nil
}

func hasContentChanges(input UpdateInput) bool {
	return input.Title != nil || input.Body != nil || input.ProjectID != nil || input.Status != nil || input.Priority != nil || input.Owner != nil || input.Tags != nil || input.Links != nil || input.Sources != nil
}

func validateList(o *ListOptions) error {
	if o.Kind != "" {
		if _, ok := defaultStatus[o.Kind]; !ok {
			return invalid("invalid kind filter")
		}
	}
	if o.Status != "" && !validStatus(o.Kind, o.Status) {
		return invalid("invalid status filter")
	}
	if o.Global && o.ProjectID != "" {
		return invalid("global and project_id filters cannot be combined")
	}
	if len(o.ProjectID) > 100 || !utf8.ValidString(o.ProjectID) {
		return invalid("invalid project_id filter")
	}
	o.Owner = strings.TrimSpace(o.Owner)
	if len(o.Owner) > 200 || !utf8.ValidString(o.Owner) {
		return invalid("invalid owner filter")
	}
	o.Tag = strings.ToLower(strings.TrimSpace(o.Tag))
	if len(o.Tag) > 64 || !utf8.ValidString(o.Tag) {
		return invalid("invalid tag filter")
	}
	o.Query = strings.TrimSpace(o.Query)
	if len(o.Query) > 500 || !utf8.ValidString(o.Query) {
		return invalid("query must be valid UTF-8 and at most 500 bytes")
	}
	if o.Limit == 0 {
		o.Limit = 50
	}
	if o.Limit < 1 || o.Limit > 200 {
		return invalid("limit must be between 1 and 200")
	}
	if o.Offset < 0 {
		return invalid("offset cannot be negative")
	}
	return nil
}

// Split punctuation so FTS operators, quotes, and column selectors are always
// ordinary search terms. Matching requires every term in either title or body.
func plainTextQuery(query string) string {
	tokens := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) })
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		quoted = append(quoted, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " AND ")
}
