package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// contextSummary answers format=summary: one line of counts that tells a
// session coordination exists without loading any record text.
func (h *Handler) contextSummary(w http.ResponseWriter, r *http.Request, projectID string) {
	snapshot, err := h.projectSnapshot(r.Context(), projectID, false)
	if err != nil {
		storeError(w, err)
		return
	}
	data := []byte(renderSummary(snapshot.Sections))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// renderSummary lists each section that has records, by tier then heading.
func renderSummary(sections []snapshotSection) string {
	sorted := append([]snapshotSection{}, sections...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Tier != sorted[j].Tier {
			return sorted[i].Tier < sorted[j].Tier
		}
		return sorted[i].Heading < sorted[j].Heading
	})
	var parts []string
	for _, s := range sorted {
		if len(s.Records) == 0 {
			continue
		}
		part := s.Heading + " " + strconv.Itoa(len(s.Records))
		if s.Total > len(s.Records) {
			part += "+"
		}
		if s.RecentFirst {
			part += " (latest " + s.Records[0].UpdatedAt.UTC().Format("2006-01-02") + ")"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return "herma coordination: " + strings.Join(parts, " · ") + " — read them with: herma context\n"
}
