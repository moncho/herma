package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/moncho/herma/internal/store"
)

// counted reports whether a read by this author says an agent used a record.
// The reviewer reads every record while judging it, so those reads would
// make each record look used.
func counted(author store.Author) bool { return author.Role != store.RoleReviewer }

// countRecalled notes the records a recall response gave to an agent. The
// count is a statistic: failing to write it must not fail the read it
// describes, so the error is dropped.
func (h *Handler) countRecalled(ctx context.Context, author store.Author, records []store.Recalled) {
	if !counted(author) || len(records) == 0 {
		return
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	_ = h.store.CountRecalled(ctx, ids)
}

// countFetched notes a read of one record by ID, under the same rules.
func (h *Handler) countFetched(ctx context.Context, author store.Author, id string) {
	if counted(author) {
		_ = h.store.CountFetched(ctx, id)
	}
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := validateQuery(q, "project_id", "limit"); err != nil {
		badRequest(w, err)
		return
	}
	limit, err := queryInt(q, "limit", 100)
	if err != nil || limit < 1 || limit > 200 {
		badRequest(w, errors.New("limit must be an integer between 1 and 200"))
		return
	}
	result, err := h.store.Usage(r.Context(), store.UsageOptions{ProjectID: q.Get("project_id"), Limit: limit})
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
