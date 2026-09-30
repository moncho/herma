package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// review lists proposed knowledge and principles, the reviewer's queue. It is
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
	return output(stdout, queue)
}
