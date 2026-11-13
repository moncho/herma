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

// recordRef names a record at one version, the form accept takes and herma
// review prints.
func recordRef(id string, version int64) string {
	return id + "@" + strconv.FormatInt(version, 10)
}

// parseRecordRef splits ID@VERSION. A bare ID is refused: accepting applies
// to the version the reviewer read, never to whatever is current.
func parseRecordRef(ref string) (string, int64, error) {
	id, text, found := strings.Cut(ref, "@")
	version, err := strconv.ParseInt(text, 10, 64)
	if !found || id == "" || err != nil || version < 1 {
		return "", 0, fmt.Errorf("accept needs ID@VERSION, the version you read, not %q; herma review prints the command", ref)
	}
	return id, version, nil
}

// accept is the reviewer's shortcut for `update ID --version N --status
// accepted`: it accepts several records at once, each at the version the
// reviewer names, and with --replaces supersedes the old record only after
// the new one is accepted.
func accept(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	var refs []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		refs, args = append(refs, args[0]), args[1:]
	}
	fs := flags("accept", stderr)
	replaces := fs.String("replaces", "", "record to mark superseded once the single new record is accepted")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case len(refs) == 0:
		return errors.New("usage: herma [global flags] accept ID@VERSION [ID@VERSION ...] | accept NEW@VERSION --replaces OLD")
	case *replaces != "" && len(refs) != 1:
		return errors.New("accept --replaces takes exactly one new record")
	}
	type target struct {
		id      string
		version int64
	}
	targets := make([]target, 0, len(refs))
	for _, ref := range refs {
		id, version, err := parseRecordRef(ref)
		if err != nil {
			return err
		}
		targets = append(targets, target{id, version})
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	result := struct {
		Accepted   []acceptedRecord `json:"accepted"`
		Superseded []acceptedRecord `json:"superseded,omitempty"`
		Failed     []acceptFailure  `json:"failed,omitempty"`
	}{Accepted: []acceptedRecord{}}
	for _, t := range targets {
		id := t.id
		current, err := getRecord(ctx, c, id)
		if err == nil && current.Version != t.version {
			err = fmt.Errorf("changed since version %d was shown (now version %d); read it again with herma review", t.version, current.Version)
		}
		if err == nil && current.Status != "accepted" {
			var similar *acceptedRecord
			if similar, err = similarAccepted(ctx, c, current); err == nil && similar != nil && similar.ID != *replaces {
				err = errors.New(duplicateMessage(id, t.version, *similar))
			}
		}
		if err != nil {
			result.Failed = append(result.Failed, acceptFailure{ID: id, Error: explain(err, id)})
			continue
		}
		// The server checks the version again inside the write, so an edit
		// that lands after the read above is a conflict, not an acceptance.
		r, err := setStatus(ctx, c, id, t.version, "accepted", nil)
		if err != nil {
			result.Failed = append(result.Failed, acceptFailure{ID: id, Error: explain(err, id)})
			continue
		}
		result.Accepted = append(result.Accepted, r)
	}
	if *replaces != "" && len(result.Failed) == 0 {
		r, err := setCurrentStatus(ctx, c, *replaces, "superseded", nil)
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

// getRecord reads a record by ID.
func getRecord(ctx context.Context, c *client.Client, id string) (store.Record, error) {
	data, err := c.Do(ctx, http.MethodGet, "/v1/records/"+url.PathEscape(id), nil, nil, "")
	if err != nil {
		return store.Record{}, err
	}
	var current store.Record
	if err := json.Unmarshal(data, &current); err != nil {
		return store.Record{}, err
	}
	return current, nil
}

// setCurrentStatus moves a record to status at whatever version it has now.
// It suits changes where an unseen edit cannot matter: rejecting, and
// superseding an accepted record, which agents cannot edit.
func setCurrentStatus(ctx context.Context, c *client.Client, id, status string, reason *string) (acceptedRecord, error) {
	current, err := getRecord(ctx, c, id)
	if err != nil {
		return acceptedRecord{}, err
	}
	return setStatus(ctx, c, id, current.Version, status, reason)
}

// setStatus moves a record to status at the given version; reason, when
// given, is stored as the review reason of a rejection.
func setStatus(ctx context.Context, c *client.Client, id string, version int64, status string, reason *string) (acceptedRecord, error) {
	requestID, err := randomID()
	if err != nil {
		return acceptedRecord{}, err
	}
	data, err := c.Do(ctx, http.MethodPatch, "/v1/records/"+url.PathEscape(id), nil, store.UpdateInput{Version: version, Status: &status, ReviewReason: reason}, requestID)
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
