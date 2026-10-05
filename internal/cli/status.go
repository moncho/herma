package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

type statusReview struct {
	Knowledge          int `json:"knowledge"`
	Principles         int `json:"principles"`
	Kinds              int `json:"kinds"`
	PendingKindChanges int `json:"pending_kind_changes"`
	Total              int `json:"total"`
}

type statusBackup struct {
	Enabled       bool       `json:"enabled"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	AgeSeconds    *int64     `json:"age_seconds,omitempty"`
	Stale         *bool      `json:"stale,omitempty"`
}

// statusCommand answers in one call what a status line shows: the binding,
// the identity, what awaits review and how old the last backup is. Outside a
// bound checkout it reports {"bound": false} without contacting the server.
func statusCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if err := noOptions("status", args, stderr); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	binding, _, err := project.Discover(cwd)
	if errors.Is(err, project.ErrNoBinding) {
		return output(stdout, map[string]bool{"bound": false})
	}
	if err != nil {
		return err
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	var who struct {
		Identity string `json:"identity"`
		Role     string `json:"role"`
	}
	if err := getJSON(ctx, c, "/v1/whoami", nil, &who); err != nil {
		return err
	}
	review := statusReview{}
	for _, count := range []struct {
		kind string
		into *int
	}{{"knowledge", &review.Knowledge}, {"principle", &review.Principles}, {"kind", &review.Kinds}} {
		var page store.ListResult
		if err := getJSON(ctx, c, "/v1/records", url.Values{"kind": {count.kind}, "status": {"proposed"}, "limit": {"1"}}, &page); err != nil {
			return err
		}
		*count.into = page.Total
	}
	pending, err := pendingKindChanges(ctx, c)
	if err != nil {
		return err
	}
	review.PendingKindChanges = len(pending)
	review.Total = review.Knowledge + review.Principles + review.Kinds + review.PendingKindChanges
	var backup struct {
		Enabled     bool `json:"enabled"`
		Stale       bool `json:"stale"`
		LastSuccess *struct {
			At time.Time `json:"at"`
		} `json:"last_success"`
	}
	if err := getJSON(ctx, c, "/v1/backup", nil, &backup); err != nil {
		return err
	}
	b := statusBackup{Enabled: backup.Enabled}
	if backup.Enabled {
		b.Stale = &backup.Stale
	}
	if backup.Enabled && backup.LastSuccess != nil {
		at := backup.LastSuccess.At.UTC()
		age := int64(time.Since(at).Seconds())
		b.LastSuccessAt, b.AgeSeconds = &at, &age
	}
	return output(stdout, map[string]any{
		"bound": true, "project_id": binding.ProjectID, "server": "ok",
		"identity": who.Identity, "role": who.Role, "review": review, "backup": b,
	})
}

func getJSON(ctx context.Context, c *client.Client, path string, query url.Values, into any) error {
	data, err := c.Do(ctx, http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}
