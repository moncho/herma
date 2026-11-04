package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

// duplicateThreshold is the share of title words two records must have in
// common (intersection over union) for one to count as a duplicate of the
// other.
const duplicateThreshold = 0.6

// similarAccepted finds an accepted record of the same kind whose title says
// nearly the same thing as r's, so the reviewer does not accept a duplicate.
// It searches with recall, which covers kinds with recall enabled, and
// returns nil when nothing is close enough.
func similarAccepted(ctx context.Context, c *client.Client, r store.Record) (*acceptedRecord, error) {
	words := titleWords(r.Title)
	if len(words) == 0 {
		return nil, nil
	}
	q := url.Values{"q": {r.Title}, "limit": {"20"}, "max_bytes": {"65536"}}
	if r.ProjectID != "" {
		q.Set("project_id", r.ProjectID)
	}
	data, err := c.Do(ctx, http.MethodGet, "/v1/recall", q, nil, "")
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
			return nil, nil // the title has no searchable words
		}
		return nil, err
	}
	var packet struct {
		Results []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &packet); err != nil {
		return nil, err
	}
	for _, hit := range packet.Results {
		if hit.ID == r.ID || hit.Kind != r.Kind || hit.Status != "accepted" {
			continue
		}
		if titleSimilarity(words, titleWords(hit.Title)) >= duplicateThreshold {
			return &acceptedRecord{ID: hit.ID, Kind: hit.Kind, Title: hit.Title}, nil
		}
	}
	return nil, nil
}

// titleWords lowercases a title and splits it into its distinct words.
func titleWords(title string) map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		words[word] = true
	}
	return words
}

// titleSimilarity is the share of words the two titles have in common.
func titleSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for word := range a {
		if b[word] {
			common++
		}
	}
	return float64(common) / float64(len(a)+len(b)-common)
}

// duplicateMessage says how to resolve a proposal that duplicates an
// accepted record.
func duplicateMessage(id string, similar acceptedRecord) string {
	return "looks like a duplicate of accepted " + similar.Kind + " " + similar.ID + " (" + similar.Title + "); accept " + id + " --replaces " + similar.ID + " to supersede it, or reject " + id
}
