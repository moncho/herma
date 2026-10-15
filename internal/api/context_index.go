package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/moncho/herma/internal/store"
)

// indexLimit caps the session-start index in bytes.
const indexLimit = 4000

// contextIndex answers format=index: titles of open work and accepted
// knowledge, so a session knows what herma holds without loading bodies.
func (h *Handler) contextIndex(w http.ResponseWriter, r *http.Request, projectID string) {
	snapshot, err := h.projectSnapshot(r.Context(), projectID, true)
	if err != nil {
		storeError(w, err)
		return
	}
	data := []byte(renderIndex(snapshot))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// renderIndex lists open tasks and accepted knowledge, newest first, within
// indexLimit bytes; other sections appear only as counts.
func renderIndex(s contextSnapshot) string {
	var work, knowledge []store.Record
	var workTotal, knowledgeTotal int
	var rest []snapshotSection
	for _, sec := range s.Sections {
		switch sec.Kind {
		case "task":
			work, workTotal = sec.Records, sec.Total
		case "knowledge":
			knowledge, knowledgeTotal = sec.Records, sec.Total
		default:
			rest = append(rest, sec)
		}
	}
	newest := func(rs []store.Record, projectFirst bool) []store.Record {
		out := append([]store.Record{}, rs...)
		sort.SliceStable(out, func(i, j int) bool {
			if projectFirst && (out[i].ProjectID == "") != (out[j].ProjectID == "") {
				return out[i].ProjectID != ""
			}
			if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
				return out[i].UpdatedAt.After(out[j].UpdatedAt)
			}
			return out[i].ID < out[j].ID
		})
		return out
	}
	work, knowledge = newest(work, false), newest(knowledge, true)
	parts := summaryParts(rest)
	if len(work) == 0 && len(knowledge) == 0 && len(parts) == 0 {
		return ""
	}
	header := "herma index · " + cleanTitle(s.Project.Title) + " · read one with mcp__herma__get or: herma get ID\n"
	tail := ""
	if len(parts) > 0 {
		tail = "Also in herma: " + strings.Join(parts, " · ") + " — read them with: herma context\n"
	}
	workLine := func(r store.Record) string {
		return "- " + r.ID + " · " + r.Status + " · " + r.UpdatedAt.UTC().Format("2006-01-02") + " · " + cleanTitle(r.Title) + "\n"
	}
	knowledgeLine := func(r store.Record) string { return "- " + r.ID + " · " + cleanTitle(r.Title) + "\n" }
	render := func(nw, nk int) string {
		var b strings.Builder
		b.WriteString(header)
		if nw > 0 {
			b.WriteString("Open work (unreviewed, kept by agents):\n")
			for _, r := range work[:nw] {
				b.WriteString(workLine(r))
			}
		}
		if nk > 0 {
			b.WriteString("Knowledge (reviewed):\n")
			for _, r := range knowledge[:nk] {
				b.WriteString(knowledgeLine(r))
			}
		}
		b.WriteString(tail)
		var hidden []string
		if n := workTotal - nw; n > 0 {
			hidden = append(hidden, fmt.Sprintf("%d open work", n))
		}
		if n := knowledgeTotal - nk; n > 0 {
			hidden = append(hidden, fmt.Sprintf("%d knowledge", n))
		}
		if len(hidden) > 0 {
			b.WriteString("Not shown: " + strings.Join(hidden, ", ") + " — list them with: herma list --kind task / herma list --kind knowledge\n")
		}
		return b.String()
	}
	nw, nk := len(work), len(knowledge)
	out := render(nw, nk)
	for len(out) > indexLimit && (nw > 0 || nk > 0) {
		if nk > 0 {
			nk--
		} else {
			nw--
		}
		out = render(nw, nk)
	}
	return out
}

// cleanTitle keeps a title on one line without control characters.
func cleanTitle(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
}
