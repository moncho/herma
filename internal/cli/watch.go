package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/moncho/herma/internal/client"
)

// watchWait is how long the server holds each request; it stays under the
// client's 30-second request timeout.
const watchWait = 25

// watchRetry is the first pause after a failed request; it doubles up to
// watchRetryMax while the server stays unreachable.
var watchRetry = time.Second

const watchRetryMax = 30 * time.Second

// watchCommand prints record changes as they are committed, one JSON object
// per line, until it is interrupted. It only reports: what to do about a
// change is up to whatever reads the lines. Without --after it starts from
// now. Without --project it uses the checkout's binding, and without a
// binding it follows every project.
func watchCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("watch", stderr)
	projectID := fs.String("project", "", "project ID; defaults to the nearest .herma-project.json, else all projects")
	kind := fs.String("kind", "", "only changes to records of this kind")
	after := fs.Int64("after", 0, "start after this change number (0 replays everything); default: start from now")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *projectID == "" {
		var err error
		if *projectID, err = boundProject(); err != nil {
			return err
		}
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	q := url.Values{"wait": {strconv.Itoa(watchWait)}}
	if supplied(fs)["after"] {
		q.Set("after", strconv.FormatInt(*after, 10))
	}
	if *kind != "" {
		q.Set("kind", *kind)
	}
	if *projectID != "" {
		q.Set("project_id", *projectID)
	}
	pause := watchRetry
	for ctx.Err() == nil {
		data, err := c.Do(ctx, http.MethodGet, "/v1/changes", q, nil, "")
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			// A refusal will not get better by asking again; an unreachable
			// or failing server may come back.
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status < http.StatusInternalServerError {
				return err
			}
			fmt.Fprintf(stderr, "herma watch: %v; retrying in %v\n", err, pause)
			select {
			case <-ctx.Done():
			case <-time.After(pause):
			}
			pause = min(2*pause, watchRetryMax)
			continue
		}
		pause = watchRetry
		var page struct {
			Items []json.RawMessage `json:"items"`
			Next  int64             `json:"next"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		for _, item := range page.Items {
			var line bytes.Buffer
			if err := json.Compact(&line, item); err != nil {
				return err
			}
			line.WriteByte('\n')
			if _, err := stdout.Write(line.Bytes()); err != nil {
				return err
			}
		}
		q.Set("after", strconv.FormatInt(page.Next, 10))
	}
	return nil
}
