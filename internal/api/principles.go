package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/moncho/herma/internal/rules"
	"github.com/moncho/herma/internal/store"
)

// principlesFileHeader starts a project rules file and globalPrinciplesFileHeader
// the global one. Claude Code strips the HTML comment, so it costs no context.
const (
	principlesFileHeader       = rules.Header + " Edits are overwritten; change them in herma. -->\n# Project principles (reviewed in herma)\n"
	globalPrinciplesFileHeader = rules.Header + " Edits are overwritten; change them in herma. -->\n# Global principles (reviewed in herma)\n"
)

// projectUnavailableCode marks a principles request whose project is gone or
// archived, so the hook knows to remove the generated rules file.
const projectUnavailableCode = "project_unavailable"

// errProjectArchived reports a principles request for an archived project.
var errProjectArchived = errors.New("project is archived")

func (h *Handler) principles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := validateQuery(q, "project_id"); err != nil {
		badRequest(w, err)
		return
	}
	projectID := q.Get("project_id")
	var records []store.Record
	var total int
	var err error
	if projectID == "" {
		records, total, err = h.globalPrinciples(r.Context())
	} else {
		records, total, err = h.principleSnapshot(r.Context(), projectID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, projectUnavailableCode, "project not found")
		return
	case errors.Is(err, errProjectArchived):
		writeError(w, http.StatusGone, projectUnavailableCode, "project is archived")
		return
	case err != nil:
		storeError(w, err)
		return
	}
	data := renderPrinciples(projectID, records, total)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) principleSnapshot(ctx context.Context, projectID string) ([]store.Record, int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	project, err := h.store.Get(ctx, projectID)
	if err != nil {
		return nil, 0, err
	}
	if project.Kind != "project" {
		return nil, 0, &store.ValidationError{Message: "principles require an unarchived project"}
	}
	if project.Archived {
		return nil, 0, errProjectArchived
	}
	return h.contextRecords(ctx, projectID, "principle", []string{"accepted"}, false, false, contextLimit)
}

// globalPrinciples returns accepted principles that belong to no project, in
// context order.
func (h *Handler) globalPrinciples(ctx context.Context) ([]store.Record, int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result, err := h.store.List(ctx, store.ListOptions{Kind: "principle", Status: "accepted", Global: true, Limit: contextLimit})
	if err != nil {
		return nil, 0, err
	}
	items := result.Items
	sort.Slice(items, func(i, j int) bool { return contextRecordLess(items[i], items[j], false) })
	return items, result.Total, nil
}

// renderPrinciples writes the rules file in context order with bodies
// verbatim. An empty result means no file should exist.
func renderPrinciples(projectID string, records []store.Record, total int) []byte {
	if total == 0 {
		return nil
	}
	var b strings.Builder
	list := "list them all with: herma list --project " + projectID + " --kind principle --status accepted --limit 200"
	if projectID == "" {
		b.WriteString(globalPrinciplesFileHeader)
		list = "list them all with: herma list --global --kind principle --status accepted --limit 200"
	} else {
		b.WriteString(principlesFileHeader)
	}
	for _, r := range records {
		scope := "project"
		if r.ProjectID == "" {
			scope = "global"
		}
		fmt.Fprintf(&b, "\n## %s\n", oneLine(r.Title))
		if body := strings.TrimRight(r.Body, "\n"); body != "" {
			b.WriteString(body + "\n")
		}
		fmt.Fprintf(&b, "\nherma: %s · %s\n", r.ID, scope)
	}
	if hidden := total - len(records); hidden > 0 {
		if hidden == 1 {
			fmt.Fprintf(&b, "\n1 more accepted principle (the lowest-priority one) is not shown; %s\n", list)
		} else {
			fmt.Fprintf(&b, "\n%d more accepted principles (the lowest-priority ones) are not shown; %s\n", hidden, list)
		}
	}
	return []byte(b.String())
}
