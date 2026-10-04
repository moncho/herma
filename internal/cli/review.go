package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/moncho/herma/internal/store"
)

// review lists proposed knowledge, principles and kinds, and pending kind
// changes, the reviewer's queue. It is
// read-only, so agents and read-only identities can see what awaits review.
func review(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("review", stderr)
	limit := fs.Int("limit", 50, "maximum records per kind (1–200)")
	offset := fs.Int("offset", 0, "number of records to skip per kind")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *limit < 1 || *limit > 200 || *offset < 0 {
		return errors.New("--limit must be 1–200 and --offset must be nonnegative")
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	queue := map[string]json.RawMessage{}
	for key, kind := range map[string]string{"knowledge": "knowledge", "principles": "principle"} {
		query := url.Values{"kind": {kind}, "status": {"proposed"}, "limit": {strconv.Itoa(*limit)}, "offset": {strconv.Itoa(*offset)}}
		data, err := c.Do(ctx, http.MethodGet, "/v1/records", query, nil, "")
		if err != nil {
			return err
		}
		queue[key] = data
	}
	kinds, err := c.Do(ctx, http.MethodGet, "/v1/records", url.Values{"kind": {"kind"}, "status": {"proposed"}, "limit": {strconv.Itoa(*limit)}, "offset": {strconv.Itoa(*offset)}}, nil, "")
	if err != nil {
		return err
	}
	queue["kinds"] = kinds
	// accept_pending applies to accepted and retired kinds alike.
	pending := []store.Record{}
	for _, status := range []string{"accepted", "retired"} {
		data, err := c.Do(ctx, http.MethodGet, "/v1/records", url.Values{"kind": {"kind"}, "status": {status}, "limit": {"200"}}, nil, "")
		if err != nil {
			return err
		}
		var page store.ListResult
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		for _, r := range page.Items {
			if _, ok := r.Fields["pending"]; ok {
				pending = append(pending, r)
			}
		}
	}
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	queue["pending_kind_changes"] = data
	return output(stdout, queue)
}
