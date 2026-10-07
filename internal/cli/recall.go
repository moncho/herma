package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/moncho/herma/internal/project"
)

const defaultRecallBytes = 8192

// recallCommand searches reviewed knowledge. Without --project it uses the
// checkout's binding, and without a binding it searches every project.
func recallCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(`usage: herma [global flags] recall "words" [--project ID] [--include-proposed] [--limit N] [--max-bytes N]`)
	}
	query := args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		words := []string{query}
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				break
			}
			words = append(words, a)
		}
		return errors.New(`quote multi-word queries: herma recall "` + strings.Join(words, " ") + `"`)
	}
	fs := flags("recall", stderr)
	projectID := fs.String("project", "", "project ID; defaults to the nearest .herma-project.json, else all projects")
	proposed := fs.Bool("include-proposed", false, "also return proposed (unreviewed) knowledge and principles")
	limit := fs.Int("limit", 20, "maximum results (1–100)")
	budget := fs.Int("max-bytes", defaultRecallBytes, "maximum response bytes, including metadata")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	if *limit < 1 || *limit > 100 {
		return errors.New("--limit must be between 1 and 100")
	}
	if err := contextBudget(*budget); err != nil {
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
	q := url.Values{"q": {query}, "limit": {strconv.Itoa(*limit)}, "max_bytes": {strconv.Itoa(*budget)}, "include_proposed": {strconv.FormatBool(*proposed)}}
	if *projectID != "" {
		q.Set("project_id", *projectID)
	}
	data, err := c.Do(ctx, http.MethodGet, "/v1/recall", q, nil, "")
	if err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return err
	}
	compact.WriteByte('\n')
	if compact.Len() > *budget {
		return errors.New("server recall response exceeds the requested byte budget")
	}
	_, err = stdout.Write(compact.Bytes())
	return err
}

// boundProject returns the project ID of the nearest binding above the working
// directory, or "" when there is none.
func boundProject() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	binding, _, err := project.Discover(cwd)
	if errors.Is(err, project.ErrNoBinding) {
		return "", nil
	}
	return binding.ProjectID, err
}
