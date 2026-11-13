package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// reject is the reviewer's shortcut for `update ID --version N --status
// rejected [--reason TEXT]`: it reads each record's current version itself and
// rejects several records at once, all with the same reason when one is given.
func reject(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) error {
	var ids []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ids, args = append(ids, args[0]), args[1:]
	}
	fs := flags("reject", stderr)
	reason := fs.String("reason", "", "one line saying why, stored as review_reason on every rejected record")
	if err := parse(fs, args); err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("usage: herma [global flags] reject ID [ID ...] [--reason TEXT]")
	}
	var reviewReason *string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "reason" {
			reviewReason = reason
		}
	})
	if reviewReason != nil && strings.TrimSpace(*reviewReason) == "" {
		return errors.New("reject --reason must be one line of 1 to 300 characters")
	}
	c, err := cfg.client()
	if err != nil {
		return err
	}
	result := struct {
		Rejected []acceptedRecord `json:"rejected"`
		Failed   []acceptFailure  `json:"failed,omitempty"`
	}{Rejected: []acceptedRecord{}}
	for _, id := range ids {
		r, err := setCurrentStatus(ctx, c, id, "rejected", reviewReason)
		if err != nil {
			result.Failed = append(result.Failed, acceptFailure{ID: id, Error: explain(err, id)})
			continue
		}
		result.Rejected = append(result.Rejected, r)
	}
	if err := output(stdout, result); err != nil {
		return err
	}
	if len(result.Failed) > 0 {
		return fmt.Errorf("%d of the records could not be changed; see failed", len(result.Failed))
	}
	return nil
}
