package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/moncho/herma/internal/store"
)

const kindUsage = "usage: herma [global flags] kind propose NAME --file PATH [--project ID] [--body TEXT] | kind change ID --file PATH | kind list"

func kindCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(kindUsage)
	}
	switch args[0] {
	case "propose":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New(kindUsage)
		}
		fs := flags("kind propose", stderr)
		file := fs.String("file", "", "JSON definition document (required)")
		project := fs.String("project", "", "limit the kind to this project")
		body := fs.String("body", "", "what the kind is for")
		requestID := fs.String("request-id", "", "idempotency key; defaults to a new random key")
		if err := parse(fs, args[2:]); err != nil {
			return err
		}
		definition, err := readDefinition(*file)
		if err != nil {
			return err
		}
		input := store.CreateInput{Kind: "kind", Title: args[1], Body: *body, ProjectID: *project, Fields: map[string]any{"definition": definition}}
		return cfg.request(ctx, stdout, http.MethodPost, "/v1/records", nil, input, *requestID)
	case "change":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New(kindUsage)
		}
		fs := flags("kind change", stderr)
		file := fs.String("file", "", "JSON definition document (required)")
		requestID := fs.String("request-id", "", "idempotency key; defaults to a new random key")
		if err := parse(fs, args[2:]); err != nil {
			return err
		}
		definition, err := readDefinition(*file)
		if err != nil {
			return err
		}
		c, err := cfg.client()
		if err != nil {
			return err
		}
		data, err := c.Do(ctx, http.MethodGet, "/v1/records/"+url.PathEscape(args[1]), nil, nil, "")
		if err != nil {
			return err
		}
		var current store.Record
		if err := json.Unmarshal(data, &current); err != nil || current.Kind != "kind" {
			return fmt.Errorf("%s is not a kind record", args[1])
		}
		input := store.UpdateInput{Version: current.Version, Fields: map[string]any{"pending": definition}}
		return cfg.request(ctx, stdout, http.MethodPatch, "/v1/records/"+url.PathEscape(args[1]), nil, input, *requestID)
	case "list":
		if err := noOptions("kind list", args[1:], stderr); err != nil {
			return err
		}
		c, err := cfg.client()
		if err != nil {
			return err
		}
		data, err := c.Do(ctx, http.MethodGet, "/v1/records", url.Values{"kind": {"kind"}, "limit": {"200"}}, nil, "")
		if err != nil {
			return err
		}
		var page store.ListResult
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		kinds := []map[string]any{}
		for _, r := range page.Items {
			_, pending := r.Fields["pending"]
			kinds = append(kinds, map[string]any{"id": r.ID, "name": r.Title, "status": r.Status, "project_id": r.ProjectID, "version": r.Version, "pending": pending})
		}
		return output(stdout, map[string]any{"kinds": kinds})
	default:
		return errors.New(kindUsage)
	}
}

func readDefinition(path string) (map[string]any, error) {
	if path == "" {
		return nil, errors.New("--file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read definition: %w", err)
	}
	var definition map[string]any
	if err := json.Unmarshal(data, &definition); err != nil || definition == nil {
		return nil, errors.New("definition file must hold one JSON object")
	}
	return definition, nil
}
