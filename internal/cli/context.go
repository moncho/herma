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
	"os"
	"strconv"

	"github.com/moncho/herma/internal/project"
	"github.com/moncho/herma/internal/store"
)

func contextBudget(n int) error {
	if n < project.MinMaxBytes || n > project.MaxMaxBytes {
		return fmt.Errorf("max-bytes must be between %d and %d", project.MinMaxBytes, project.MaxMaxBytes)
	}
	return nil
}

func bindProject(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "bind" {
		return errors.New("usage: herma project bind --project ID [--dir PATH] [--max-bytes N]")
	}
	fs := flags("project bind", stderr)
	id := fs.String("project", "", "existing project record ID (required)")
	dir := fs.String("dir", ".", "repository directory to bind")
	budget := fs.Int("max-bytes", project.DefaultMaxBytes, "maximum context JSON bytes, including metadata")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("project bind requires --project ID")
	}
	if err := contextBudget(*budget); err != nil {
		return err
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	data, err := c.Do(ctx, http.MethodGet, "/v1/records/"+url.PathEscape(*id), nil, nil, "")
	if err != nil {
		return err
	}
	var record store.Record
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.ID != *id || record.Kind != "project" || record.Archived {
		return errors.New("binding requires an existing unarchived project")
	}
	path, err := project.Bind(*dir, project.Binding{ProjectID: *id, MaxBytes: *budget})
	if err != nil {
		return err
	}
	return output(stdout, map[string]any{"project_id": *id, "max_bytes": *budget, "path": path})
}

func projectContextCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("context", stderr)
	id := fs.String("project", "", "project ID; defaults to the nearest .herma-project.json")
	budget := fs.Int("max-bytes", project.DefaultMaxBytes, "maximum context JSON bytes, including metadata")
	durable := fs.Bool("include-durable", false, "also include accepted knowledge")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *id == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		binding, _, err := project.Discover(cwd)
		if errors.Is(err, project.ErrNoBinding) {
			return errors.New("context requires --project ID or a project binding; run herma project bind --project ID")
		}
		if err != nil {
			return err
		}
		*id = binding.ProjectID
		if !supplied(fs)["max-bytes"] {
			*budget = binding.MaxBytes
		}
	}
	data, err := cfg.contextData(ctx, *id, *budget, *durable)
	if err != nil {
		return err
	}
	_, err = stdout.Write(data)
	return err
}

// Keep context compact on stdout as well as over HTTP. Indentation would add
// unbudgeted text after the server has measured the response.
func (cfg config) contextData(ctx context.Context, id string, budget int, durable bool) ([]byte, error) {
	if err := contextBudget(budget); err != nil {
		return nil, err
	}
	c, err := cfg.client()
	if err != nil {
		return nil, err
	}
	q := url.Values{"project_id": {id}, "max_bytes": {strconv.Itoa(budget)}, "include_durable": {strconv.FormatBool(durable)}}
	data, err := c.Do(ctx, http.MethodGet, "/v1/context", q, nil, "")
	if err != nil {
		return nil, err
	}
	var packet struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
		MaxBytes int `json:"max_bytes"`
	}
	if err := json.Unmarshal(data, &packet); err != nil || packet.Project.ID != id || packet.MaxBytes != budget {
		return nil, errors.New("server returned an incompatible context response; update the herma service")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, err
	}
	compact.WriteByte('\n')
	if compact.Len() > budget {
		return nil, errors.New("server context exceeds the requested byte budget")
	}
	return compact.Bytes(), nil
}
