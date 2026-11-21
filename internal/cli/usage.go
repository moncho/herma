package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moncho/herma/internal/safetext"
	"github.com/moncho/herma/internal/store"
)

// usageCommand shows how often agents were given each accepted record, least
// used first, so the reviewer can see which records earn their place. Without
// --project it uses the checkout's binding, and without a binding it lists
// every project.
func usageCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("usage", stderr)
	projectID := fs.String("project", "", "project ID; defaults to the nearest .herma-project.json, else all projects")
	limit := fs.Int("limit", 100, "maximum records (1–200)")
	format := fs.String("format", "text", "text (readable) or json")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return errors.New("usage --format must be text or json")
	}
	if *projectID == "" {
		var err error
		if *projectID, err = boundProject(); err != nil {
			return err
		}
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *projectID != "" {
		q.Set("project_id", *projectID)
	}
	if *format == "json" {
		return cfg.request(ctx, stdout, http.MethodGet, "/v1/usage", q, nil, "")
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	data, err := c.Do(ctx, http.MethodGet, "/v1/usage", q, nil, "")
	if err != nil {
		return err
	}
	var result store.UsageResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	return usageText(stdout, result)
}

// usageText prints one line per record under a header: the two counts, the
// day it was last used and the day it was accepted, then what it is.
func usageText(w io.Writer, result store.UsageResult) error {
	if len(result.Items) == 0 {
		_, err := io.WriteString(w, "No accepted records.\n")
		return err
	}
	day := func(at *time.Time, none string) string {
		if at == nil {
			return none
		}
		return at.UTC().Format("2006-01-02")
	}
	kindWidth := len("KIND")
	for _, u := range result.Items {
		kindWidth = max(kindWidth, utf8.RuneCountInString(u.Kind))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%8s  %7s  %-10s  %-10s  %-36s  %-*s  %s\n", "RECALLED", "FETCHED", "LAST USED", "ACCEPTED", "ID", kindWidth, "KIND", "TITLE")
	for _, u := range result.Items {
		fmt.Fprintf(&b, "%8d  %7d  %-10s  %-10s  %-36s  %-*s  %s\n", u.Recalled, u.Fetched, day(u.LastUsedAt, "never"), day(u.ReviewedAt, "-"), u.ID, kindWidth, u.Kind, safetext.Line(u.Title))
	}
	if len(result.Items) < result.Total {
		fmt.Fprintf(&b, "%d of %d records; raise --limit (up to 200) to see more\n", len(result.Items), result.Total)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
