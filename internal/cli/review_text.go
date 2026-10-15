package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

// reviewText prints the review queue for a person: one block per proposal
// with its project by name, author, date, title, body and sources.
func reviewText(ctx context.Context, c *client.Client, w io.Writer, queue map[string]json.RawMessage, pending []store.Record, offset int) error {
	sections := []struct{ key, heading, noun string }{
		{"principles", "Principles", "principles"},
		{"knowledge", "Knowledge", "knowledge"},
		{"kinds", "Kinds", "kinds"},
	}
	pages := map[string]store.ListResult{}
	total := len(pending)
	for _, s := range sections {
		var page store.ListResult
		if err := json.Unmarshal(queue[s.key], &page); err != nil {
			return err
		}
		pages[s.key] = page
		total += page.Total
	}
	if total == 0 {
		_, err := io.WriteString(w, "Nothing to review.\n")
		return err
	}
	names := projectNames(ctx, c)
	var b strings.Builder
	noun := "proposals"
	if total == 1 {
		noun = "proposal"
	}
	fmt.Fprintf(&b, "herma review · %d %s\n", total, noun)
	for _, s := range sections {
		page := pages[s.key]
		if len(page.Items) == 0 && page.Total == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s\n", s.heading)
		for i, r := range page.Items {
			if i > 0 {
				b.WriteString("\n")
			}
			writeProposal(&b, r, names(r.ProjectID))
		}
		if more := page.Total - offset - len(page.Items); more > 0 {
			fmt.Fprintf(&b, "\n%d more %s not shown: herma review --offset %d\n", more, s.noun, offset+len(page.Items))
		}
	}
	if len(pending) > 0 {
		b.WriteString("\nPending kind changes\n")
		for i, r := range pending {
			if i > 0 {
				b.WriteString("\n")
			}
			writeProposal(&b, r, names(r.ProjectID))
		}
	}
	b.WriteString("\nAccept on the server: herma --identity owner accept ID [ID ...]\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeProposal(b *strings.Builder, r store.Record, project string) {
	fmt.Fprintf(b, "%s · %s · by %s · %s\n%s\n", r.ID, project, r.CreatedBy, r.UpdatedAt.UTC().Format("2006-01-02"), r.Title)
	if body := strings.TrimRight(r.Body, "\n"); body != "" {
		for _, line := range strings.Split(body, "\n") {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("  " + line + "\n")
		}
	}
	if len(r.Sources) > 0 {
		b.WriteString("  sources: " + strings.Join(r.Sources, ", ") + "\n")
	}
}

// projectNames returns a lookup from project ID to title, fetching each
// project once; "" is global, and a project that cannot be read shows its ID.
func projectNames(ctx context.Context, c *client.Client) func(string) string {
	cache := map[string]string{}
	return func(id string) string {
		if id == "" {
			return "global"
		}
		if name, ok := cache[id]; ok {
			return name
		}
		name := id
		if data, err := c.Do(ctx, http.MethodGet, "/v1/records/"+url.PathEscape(id), nil, nil, ""); err == nil {
			var p store.Record
			if json.Unmarshal(data, &p) == nil && p.Title != "" {
				name = p.Title
			}
		}
		cache[id] = name
		return name
	}
}
