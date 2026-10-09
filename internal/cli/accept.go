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

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

type acceptedRecord struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type acceptFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// accept is the reviewer's shortcut for `update ID --version N --status
// accepted`: it reads each record's current version itself, accepts several
// records at once, and with --replaces supersedes the old record only after
// the new one is accepted.
func accept(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	var ids []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ids, args = append(ids, args[0]), args[1:]
	}
	fs := flags("accept", stderr)
	replaces := fs.String("replaces", "", "record to mark superseded once the single new record is accepted")
	all := fs.Bool("all", false, "accept every proposed principle, knowledge record and kind")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case *all && (len(ids) > 0 || *replaces != ""):
		return errors.New("accept --all takes no IDs and no --replaces")
	case !*all && len(ids) == 0:
		return errors.New("usage: herma [global flags] accept ID [ID ...] | accept NEW --replaces OLD | accept --all")
	case *replaces != "" && len(ids) != 1:
		return errors.New("accept --replaces takes exactly one new record ID")
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	if *all {
		if ids, err = proposedIDs(ctx, c); err != nil {
			return err
		}
	}
	result := struct {
		Accepted   []acceptedRecord `json:"accepted"`
		Superseded []acceptedRecord `json:"superseded,omitempty"`
		Failed     []acceptFailure  `json:"failed,omitempty"`
	}{Accepted: []acceptedRecord{}}
	for _, id := range ids {
		r, err := setStatus(ctx, c, id, "accepted")
		if err != nil {
			result.Failed = append(result.Failed, acceptFailure{ID: id, Error: explain(err, id)})
			continue
		}
		result.Accepted = append(result.Accepted, r)
	}
	if *replaces != "" && len(result.Failed) == 0 {
		r, err := setStatus(ctx, c, *replaces, "superseded")
		if err != nil {
			result.Failed = append(result.Failed, acceptFailure{ID: *replaces, Error: explain(err, *replaces)})
		} else {
			result.Superseded = append(result.Superseded, r)
		}
	}
	if err := output(stdout, result); err != nil {
		return err
	}
	if len(result.Failed) > 0 {
		return fmt.Errorf("%d of the records could not be changed; see failed", len(result.Failed))
	}
	return nil
}

// setStatus moves a record to status at its current version.
func setStatus(ctx context.Context, c *client.Client, id, status string) (acceptedRecord, error) {
	path := "/v1/records/" + url.PathEscape(id)
	data, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return acceptedRecord{}, err
	}
	var current store.Record
	if err := json.Unmarshal(data, &current); err != nil {
		return acceptedRecord{}, err
	}
	requestID, err := randomID()
	if err != nil {
		return acceptedRecord{}, err
	}
	data, err = c.Do(ctx, http.MethodPatch, path, nil, store.UpdateInput{Version: current.Version, Status: &status}, requestID)
	if err != nil {
		return acceptedRecord{}, err
	}
	var updated store.Record
	if err := json.Unmarshal(data, &updated); err != nil {
		return acceptedRecord{}, err
	}
	return acceptedRecord{ID: updated.ID, Kind: updated.Kind, Title: updated.Title}, nil
}

// explain turns a failed write into one line, with the fix for the common
// case of a proposal that has no sources.
func explain(err error, id string) string {
	message := err.Error()
	if strings.Contains(message, "requires at least one source") {
		message += "; add them with: herma update " + id + " --version N --sources SOURCE"
	}
	return message
}

// proposedIDs lists the review queue that --all accepts: proposed knowledge,
// principles and kinds (pending kind changes are not a status change).
func proposedIDs(ctx context.Context, c *client.Client) ([]string, error) {
	var ids []string
	for _, kind := range []string{"principle", "knowledge", "kind"} {
		for offset := 0; ; offset += 200 {
			query := url.Values{"kind": {kind}, "status": {"proposed"}, "limit": {"200"}, "offset": {strconv.Itoa(offset)}}
			data, err := c.Do(ctx, http.MethodGet, "/v1/records", query, nil, "")
			if err != nil {
				return nil, err
			}
			var page store.ListResult
			if err := json.Unmarshal(data, &page); err != nil {
				return nil, err
			}
			for _, r := range page.Items {
				ids = append(ids, r.ID)
			}
			if len(page.Items) < 200 {
				break
			}
		}
	}
	return ids, nil
}
