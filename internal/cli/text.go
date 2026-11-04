package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moncho/herma/internal/store"
)

// listText prints one line per record: id, kind, status, date and title,
// with the kind and status columns as wide as the page needs. Archived
// records are marked. A final line says how to see the rest when the page
// is not the whole result.
func listText(w io.Writer, page store.ListResult, offset int) error {
	if len(page.Items) == 0 {
		_, err := io.WriteString(w, "No records.\n")
		return err
	}
	kindWidth, statusWidth := 0, 0
	for _, r := range page.Items {
		kindWidth = max(kindWidth, utf8.RuneCountInString(r.Kind))
		statusWidth = max(statusWidth, utf8.RuneCountInString(r.Status))
	}
	var b strings.Builder
	for _, r := range page.Items {
		fmt.Fprintf(&b, "%s  %-*s  %-*s  %s  %s", r.ID, kindWidth, r.Kind, statusWidth, r.Status, r.UpdatedAt.UTC().Format("2006-01-02"), r.Title)
		if r.Archived {
			b.WriteString("  [archived]")
		}
		b.WriteString("\n")
	}
	if shown := offset + len(page.Items); shown < page.Total {
		fmt.Fprintf(&b, "%d of %d records; add --offset %d for the next page\n", len(page.Items), page.Total, shown)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// recordText prints one record for a person: a header line, who touched
// it and when, its lists and typed fields, then the title and body.
func recordText(w io.Writer, r store.Record, project string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · %s · v%d · %s", r.ID, r.Kind, r.Status, r.Version, project)
	if r.Archived {
		b.WriteString(" · archived")
	}
	b.WriteString("\n")
	day := func(at time.Time) string { return at.UTC().Format("2006-01-02") }
	fmt.Fprintf(&b, "created by %s %s · updated by %s %s", r.CreatedBy, day(r.CreatedAt), r.UpdatedBy, day(r.UpdatedAt))
	if r.ReviewedAt != nil {
		fmt.Fprintf(&b, " · reviewed by %s %s", r.ReviewedBy, day(*r.ReviewedAt))
	}
	b.WriteString("\n")
	if r.ReviewReason != "" {
		b.WriteString("reason: " + r.ReviewReason + "\n")
	}
	if r.Priority != 0 {
		b.WriteString("priority: " + strconv.Itoa(r.Priority) + "\n")
	}
	if r.Owner != "" {
		b.WriteString("owner: " + r.Owner + "\n")
	}
	for _, list := range []struct {
		name  string
		items []string
	}{{"tags", r.Tags}, {"links", r.Links}, {"sources", r.Sources}} {
		if len(list.items) > 0 {
			b.WriteString(list.name + ": " + strings.Join(list.items, ", ") + "\n")
		}
	}
	if len(r.Fields) > 0 {
		names := make([]string, 0, len(r.Fields))
		for name := range r.Fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value, _ := json.Marshal(r.Fields[name])
			b.WriteString(name + ": " + string(value) + "\n")
		}
	}
	b.WriteString("\n" + r.Title + "\n")
	writeBody(&b, r.Body)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeBody indents a body by two spaces, keeping blank lines blank.
func writeBody(b *strings.Builder, body string) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return
	}
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + line + "\n")
	}
}
