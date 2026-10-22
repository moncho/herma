package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

// reviewItem is one entry of the interactive queue: a proposal, or an
// accepted or retired kind whose pending change awaits the reviewer.
type reviewItem struct {
	heading string
	record  store.Record
	pending bool
}

type reviewOutcome struct {
	record store.Record
	note   string
}

// reviewInteractive shows the review queue one item at a time and applies
// the reviewer's decision on each, at the version that was shown.
func reviewInteractive(ctx context.Context, c *client.Client, stdin io.Reader, stdout io.Writer) error {
	items, err := reviewItems(ctx, c)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		_, err := io.WriteString(stdout, "Nothing to review.\n")
		return err
	}
	names := projectNames(ctx, c)
	in := bufio.NewScanner(stdin)
	// ask prompts until one of the allowed answers arrives, or any non-empty
	// line when allowed is nil; ok is false once input ends.
	ask := func(prompt string, allowed ...string) (string, bool) {
		for {
			fmt.Fprint(stdout, prompt)
			if !in.Scan() {
				fmt.Fprintln(stdout)
				return "", false
			}
			answer := strings.TrimSpace(in.Text())
			if answer != "" && (allowed == nil || slices.Contains(allowed, answer)) {
				return answer, true
			}
		}
	}
	var accepted, rejected, skipped, failed []reviewOutcome
	reached := len(items)
	for i, item := range items {
		var b strings.Builder
		fmt.Fprintf(&b, "\n[%d/%d] %s\n", i+1, len(items), item.heading)
		writeProposal(&b, item.record, names(item.record.ProjectID))
		if item.pending {
			definition, _ := json.MarshalIndent(item.record.Fields["pending"], "  ", "  ")
			b.WriteString("  pending definition:\n  " + string(definition) + "\n")
		}
		if _, err := io.WriteString(stdout, b.String()); err != nil {
			return err
		}
		key, ok := ask("[a]ccept [r]eject [s]kip [q]uit: ", "a", "r", "s", "q")
		if !ok || key == "q" {
			reached = i
			break
		}
		if key == "s" {
			skipped = append(skipped, reviewOutcome{record: item.record})
			continue
		}
		input := store.UpdateInput{Version: item.record.Version}
		switch {
		case key == "a" && item.pending:
			input.AcceptPending = true
		case key == "a":
			input.Status = pointerTo("accepted")
		case item.pending:
			input.Fields = map[string]any{"pending": nil}
		default:
			reason, ok := ask("reason: ")
			if !ok {
				reached = i
				break
			}
			input.Status, input.ReviewReason = pointerTo("rejected"), &reason
		}
		if reached == i {
			break
		}
		if _, err := c.Do(ctx, http.MethodPatch, "/v1/records/"+url.PathEscape(item.record.ID), nil, input, ""); err != nil {
			failed = append(failed, reviewOutcome{record: item.record, note: decisionError(err, item.record.ID)})
			continue
		}
		if key == "a" {
			accepted = append(accepted, reviewOutcome{record: item.record})
		} else {
			rejected = append(rejected, reviewOutcome{record: item.record})
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\naccepted %d · rejected %d · skipped %d · failed %d", len(accepted), len(rejected), len(skipped), len(failed))
	if left := len(items) - reached; left > 0 {
		fmt.Fprintf(&b, " · left %d", left)
	}
	b.WriteString("\n")
	for _, group := range []struct {
		heading  string
		outcomes []reviewOutcome
	}{{"Accepted", accepted}, {"Rejected", rejected}, {"Skipped", skipped}, {"Failed", failed}} {
		if len(group.outcomes) == 0 {
			continue
		}
		b.WriteString(group.heading + "\n")
		for _, o := range group.outcomes {
			line := "  " + o.record.ID + " · " + o.record.Title
			if o.note != "" {
				line += ": " + o.note
			}
			b.WriteString(line + "\n")
		}
	}
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of the decisions could not be applied; see Failed", len(failed))
	}
	return nil
}

// reviewItems lists the whole queue in the order herma review prints it.
func reviewItems(ctx context.Context, c *client.Client) ([]reviewItem, error) {
	var items []reviewItem
	for _, section := range []struct{ kind, heading string }{{"principle", "Principle"}, {"knowledge", "Knowledge"}, {"kind", "Kind"}} {
		for offset := 0; ; offset += 200 {
			query := url.Values{"kind": {section.kind}, "status": {"proposed"}, "limit": {"200"}, "offset": {strconv.Itoa(offset)}}
			data, err := c.Do(ctx, http.MethodGet, "/v1/records", query, nil, "")
			if err != nil {
				return nil, err
			}
			var page store.ListResult
			if err := json.Unmarshal(data, &page); err != nil {
				return nil, err
			}
			for _, r := range page.Items {
				items = append(items, reviewItem{heading: section.heading, record: r})
			}
			if len(page.Items) < 200 {
				break
			}
		}
	}
	pending, err := pendingKindChanges(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, r := range pending {
		items = append(items, reviewItem{heading: "Pending kind change", record: r, pending: true})
	}
	return items, nil
}

// decisionError explains a failed decision in one line; a conflict means
// someone changed the record after it was shown.
func decisionError(err error, id string) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return "changed since it was shown"
	}
	return explain(err, id)
}

func pointerTo(s string) *string { return &s }
