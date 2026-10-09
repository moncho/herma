package cli

import (
	"context"
	"io"
	"net/url"
)

// principlesCommand prints accepted principles as markdown: global ones by
// default, or one project's with --project, for clients without a hook.
func principlesCommand(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	fs := flags("principles", stderr)
	projectID := fs.String("project", "", "project ID; omitted prints the global principles")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	query := url.Values{}
	if *projectID != "" {
		query.Set("project_id", *projectID)
	}
	data, err := c.Text(ctx, "/v1/principles", query)
	if err != nil {
		return err
	}
	_, err = stdout.Write(data)
	return err
}
