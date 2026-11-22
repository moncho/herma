package cli

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A running server writes the cause of an internal error to its stderr, which
// the service setup sends to server.log, and tells the caller nothing of it.
func TestServeLogsTheCauseOfInternalErrors(t *testing.T) {
	var serverLog safeBuffer
	credentials, _, _ := startServeTo(t, &serverLog, nil)
	t.Chdir(t.TempDir())
	task := createAs(t, "local-agent", "--kind", "task", "--title", "A task")

	// Break the database behind the server's back.
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(credentials), "herma.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE usage"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// The read still answers; only the count behind it failed.
	if _, err := runCLI(t, "get", task.ID); err != nil {
		t.Fatalf("get after the usage table was dropped: %v", err)
	}
	_, err = runCLI(t, "usage", "--format", "json")
	if err == nil || !strings.Contains(err.Error(), "internal_error") || strings.Contains(err.Error(), "no such table") {
		t.Fatalf("usage after the table was dropped: %v, want only the generic internal error", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	want := []*regexp.Regexp{
		regexp.MustCompile(`usage count not recorded: GET /v1/records/` + task.ID + ` \(local-agent\): .*no such table: usage`),
		regexp.MustCompile(`internal error: GET /v1/usage \(local-agent\): .*no such table: usage`),
	}
	for {
		logged := serverLog.String()
		if want[0].MatchString(logged) && want[1].MatchString(logged) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server log lacks the causes:\n%s", logged)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
