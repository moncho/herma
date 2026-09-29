package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIncompleteExportDoesNotReachStdout(t *testing.T) {
	t.Setenv("HERMA_TOKEN", "export-test-token")
	t.Setenv("HERMA_IDENTITY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/export" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{"format_version":1,"records":[`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"--url", server.URL, "export"}, &stdout, &stderr)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected a truncated-transfer error, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("partial export was printed: %q", stdout.String())
	}
}
