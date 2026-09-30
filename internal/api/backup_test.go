package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/moncho/herma/internal/backup"
)

func TestBackupStatusWhenDisabled(t *testing.T) {
	h, _ := roleTestHandler(t)
	response := apiTestRequest(h, http.MethodGet, "/v1/backup", "", "", "Bearer "+apiTestToken, "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"enabled":false}` {
		t.Fatalf("disabled status: %d %s", response.Code, response.Body.String())
	}
}

func TestBackupStatusForEveryRole(t *testing.T) {
	h, s := roleTestHandler(t)
	dir := t.TempDir()
	runner, err := backup.New(s, backup.Config{Dir: dir, Every: time.Hour, Keep: 3}, &bytes.Buffer{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.SetBackupReporter(runner)
	for _, test := range []struct {
		handler http.Handler
		token   string
	}{
		{h, apiTestToken},
		{h, readerToken},
		{h.Socket(), reviewerToken},
	} {
		response := apiTestRequest(test.handler, http.MethodGet, "/v1/backup", "", "", "Bearer "+test.token, "")
		var status backup.Status
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil {
			t.Fatalf("status: %d %s", response.Code, response.Body.String())
		}
		if !status.Enabled || status.Dir != dir || status.Keep != 3 || status.Stale || status.LastSuccess == nil {
			t.Fatalf("status: %+v", status)
		}
	}
	assertAPIError(t, apiTestRequest(h, http.MethodPost, "/v1/backup", "{}", "application/json", "Bearer "+apiTestToken, ""), http.StatusMethodNotAllowed)
}
