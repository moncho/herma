// Package api exposes the shared knowledge store over an authenticated JSON API.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moncho/herma/internal/store"
)

const maxBody = 128 << 10
const maxExportBytes = 16 << 20
const exportBuildTimeout = 20 * time.Second

type identity struct {
	actor  string
	digest [32]byte
}

type handler struct {
	store      *store.Store
	identities []identity
	// Aggregated context and exports must not mix revisions from concurrent API
	// writes. One service process owns the database for this first version.
	mu sync.RWMutex
}

// NewHandler copies actor->token credentials. Ambiguous credentials fail closed.
func NewHandler(s *store.Store, identities map[string]string) http.Handler {
	h := &handler{store: s}
	seen := map[[32]byte]bool{}
	for actor, token := range identities {
		digest := sha256.Sum256([]byte(token))
		if strings.TrimSpace(actor) == "" || strings.TrimSpace(token) == "" || seen[digest] {
			h.identities = nil
			return h
		}
		seen[digest] = true
		h.identities = append(h.identities, identity{actor: actor, digest: digest})
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	actor, ok := h.authenticate(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="knowledge-base"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
		return
	}
	// URL.Query silently drops malformed parameters. Reject them so a broken
	// project filter can never turn into a broader query.
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		badRequest(w, errors.New("malformed query parameters"))
		return
	}
	switch r.URL.Path {
	case "/v1/schema":
		if method(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, schemaDocument())
		}
	case "/v1/records":
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			var input store.CreateInput
			if !decodeBody(w, r, &input) {
				return
			}
			// Client-controlled body reads and response writes must never hold
			// the snapshot lock. Only the committed mutation needs exclusion.
			h.mu.Lock()
			record, replay, err := h.store.Create(r.Context(), actor, r.Header.Get("Idempotency-Key"), input)
			h.mu.Unlock()
			if err != nil {
				storeError(w, err)
				return
			}
			status := http.StatusCreated
			if replay {
				status = http.StatusOK
				w.Header().Set("Idempotency-Replayed", "true")
			}
			w.Header().Set("Location", "/v1/records/"+record.ID)
			writeJSON(w, status, record)
		default:
			method(w, r, http.MethodGet, http.MethodPost)
		}
	case "/v1/context":
		if method(w, r, http.MethodGet) {
			h.projectContext(w, r)
		}
	case "/v1/export":
		if method(w, r, http.MethodGet) {
			h.export(w, r)
		}
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "records" && validID(parts[2]) {
			if len(parts) == 3 {
				h.record(w, r, parts[2], actor)
				return
			}
			if len(parts) == 4 && parts[3] == "history" {
				if !method(w, r, http.MethodGet) {
					return
				}
				revisions, err := h.store.History(r.Context(), parts[2])
				if err != nil {
					storeError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"items": revisions})
				return
			}
		}
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (h *handler) authenticate(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	digest := sha256.Sum256([]byte(parts[1]))
	actor := ""
	for _, item := range h.identities {
		if subtle.ConstantTimeCompare(digest[:], item.digest[:]) == 1 {
			actor = item.actor
		}
	}
	return actor, actor != ""
}

func (h *handler) record(w http.ResponseWriter, r *http.Request, id, actor string) {
	switch r.Method {
	case http.MethodGet:
		record, err := h.store.Get(r.Context(), id)
		if err != nil {
			storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, record)
	case http.MethodPatch:
		var input store.UpdateInput
		if !decodeBody(w, r, &input) {
			return
		}
		h.mu.Lock()
		record, replay, err := h.store.Update(r.Context(), id, actor, r.Header.Get("Idempotency-Key"), input)
		h.mu.Unlock()
		if err != nil {
			storeError(w, err)
			return
		}
		if replay {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeJSON(w, http.StatusOK, record)
	default:
		method(w, r, http.MethodGet, http.MethodPatch)
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := validateQuery(q, "kind", "project_id", "global", "status", "owner", "tag", "q", "include_archived", "limit", "offset"); err != nil {
		badRequest(w, err)
		return
	}
	options := store.ListOptions{Kind: q.Get("kind"), ProjectID: q.Get("project_id"), Status: q.Get("status"), Owner: q.Get("owner"), Tag: q.Get("tag"), Query: q.Get("q")}
	var err error
	if options.Global, err = queryBool(q, "global"); err != nil {
		badRequest(w, err)
		return
	}
	if options.Archived, err = queryBool(q, "include_archived"); err != nil {
		badRequest(w, err)
		return
	}
	if options.Limit, err = queryInt(q, "limit", 50); err != nil {
		badRequest(w, err)
		return
	}
	if options.Limit < 1 || options.Limit > 200 {
		badRequest(w, errors.New("limit must be between 1 and 200"))
		return
	}
	if options.Offset, err = queryInt(q, "offset", 0); err != nil {
		badRequest(w, err)
		return
	}
	result, err := h.store.List(r.Context(), options)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) export(w http.ResponseWriter, r *http.Request) {
	if err := validateQuery(r.URL.Query()); err != nil {
		badRequest(w, err)
		return
	}
	// Leave time to send a structured failure before the server's 30-second
	// write deadline. Streaming and efficient large-history snapshots are future work.
	ctx, cancel := context.WithTimeout(r.Context(), exportBuildTimeout)
	defer cancel()
	snapshot, err := h.exportSnapshot(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, "export_timeout", "export took too long to prepare; use a stopped-service database backup for large histories")
		} else {
			storeError(w, err)
		}
		return
	}
	// Serialize before committing success. Content-Length also makes an
	// interrupted network transfer detectable by HTTP clients.
	data, err := json.Marshal(snapshot)
	if err != nil {
		storeError(w, err)
		return
	}
	if len(data)+1 > maxExportBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "export_too_large", "export exceeds 16 MiB; use a stopped-service database backup for large histories")
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "export_timeout", "export took too long to prepare; use a stopped-service database backup for large histories")
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

type exportSnapshot struct {
	FormatVersion int                         `json:"format_version"`
	ExportedAt    time.Time                   `json:"exported_at"`
	Records       []store.Record              `json:"records"`
	History       map[string][]store.Revision `json:"history"`
}

func (h *handler) exportSnapshot(ctx context.Context) (exportSnapshot, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := exportSnapshot{FormatVersion: 1, Records: []store.Record{}, History: map[string][]store.Revision{}}
	for offset := 0; ; offset += 200 {
		page, err := h.store.List(ctx, store.ListOptions{Archived: true, Limit: 200, Offset: offset})
		if err != nil {
			return exportSnapshot{}, err
		}
		result.Records = append(result.Records, page.Items...)
		if len(result.Records) >= page.Total {
			break
		}
	}
	for _, record := range result.Records {
		items, err := h.store.History(ctx, record.ID)
		if err != nil {
			return exportSnapshot{}, err
		}
		result.History[record.ID] = items
	}
	result.ExportedAt = time.Now().UTC()
	return result, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 128 KiB")
		} else {
			badRequest(w, errors.New("could not read request body"))
		}
		return false
	}
	// Object-only input prevents null from turning into an empty request. Reject
	// duplicate keys so a client cannot sign one interpretation and write another.
	if err = validateJSONObject(data); err != nil {
		badRequest(w, err)
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(dst); err != nil {
		badRequest(w, fmt.Errorf("invalid JSON: %w", err))
		return false
	}
	return true
}

func validateJSONObject(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("request body must be valid UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("request body must be a JSON object")
	}
	keys := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return errors.New("invalid JSON object")
		}
		key, ok := token.(string)
		if !ok || keys[key] {
			return errors.New("duplicate or invalid JSON field")
		}
		for _, char := range key {
			if !(char >= 'a' && char <= 'z' || char == '_') {
				return errors.New("JSON field names must use their documented ASCII lowercase spelling")
			}
		}
		keys[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return errors.New("invalid JSON field value")
		}
		if string(value) == "null" {
			return fmt.Errorf("field %q cannot be null; omit it or supply a value", key)
		}
	}
	if _, err = d.Token(); err != nil {
		return errors.New("invalid JSON object")
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, value := range allowed {
		if r.Method == value {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this endpoint")
	return false
}

func validID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "rec_") {
		return false
	}
	for _, r := range id[4:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validateQuery(q url.Values, allowed ...string) error {
	for key, values := range q {
		found := false
		for _, value := range allowed {
			if key == value {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown query parameter %q", key)
		}
		if len(values) != 1 {
			return fmt.Errorf("query parameter %q must appear once", key)
		}
	}
	return nil
}

func queryBool(q url.Values, key string) (bool, error) {
	if !q.Has(key) {
		return false, nil
	}
	if q.Get(key) == "true" {
		return true, nil
	}
	if q.Get(key) == "false" {
		return false, nil
	}
	return false, fmt.Errorf("%s must be true or false", key)
}
func queryInt(q url.Values, key string, fallback int) (int, error) {
	if !q.Has(key) {
		return fallback, nil
	}
	n, err := strconv.Atoi(q.Get(key))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}

func badRequest(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
}
func storeError(w http.ResponseWriter, err error) {
	var invalid *store.ValidationError
	switch {
	case errors.As(err, &invalid):
		badRequest(w, err)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "version_conflict", "record changed; fetch the latest version, reconcile your edit, and retry")
	case errors.Is(err, store.ErrIdempotency):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
