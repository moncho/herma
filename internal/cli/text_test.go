package cli

import (
	"strings"
	"testing"
)

func TestListTextPrintsOneLinePerRecord(t *testing.T) {
	startServe(t)
	a := createAs(t, "local-agent", "--kind", "task", "--title", "Wire the hook", "--body", "long body text").ID
	b := createAs(t, "local-agent", "--kind", "knowledge", "--title", "SQLite runs in WAL mode", "--sources", "https://sqlite.org/wal.html").ID
	data, err := runCLI(t, "list", "--format", "text")
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	out := string(data)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per record:\n%s", out)
	}
	for _, want := range []string{a + "  task       open      ", b + "  knowledge  proposed  ", "  Wire the hook", "  SQLite runs in WAL mode"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "long body text") {
		t.Fatalf("list text should not print bodies:\n%s", out)
	}
}

func TestListTextSaysHowToSeeTheRest(t *testing.T) {
	startServe(t)
	for _, title := range []string{"One", "Two", "Three"} {
		createAs(t, "local-agent", "--kind", "task", "--title", title)
	}
	data, err := runCLI(t, "list", "--format", "text", "--limit", "2")
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if !strings.Contains(string(data), "2 of 3 records; add --offset 2 for the next page") {
		t.Fatalf("output:\n%s", data)
	}
	data, err = runCLI(t, "list", "--format", "text", "--kind", "note")
	if err != nil || strings.TrimSpace(string(data)) != "No records." {
		t.Fatalf("%v: %q", err, data)
	}
}

func TestGetTextShowsTheWholeRecord(t *testing.T) {
	startServe(t)
	id := createAs(t, "owner", "--kind", "principle", "--title", "Keep it short", "--body", "First line.\n\nSecond paragraph.", "--status", "accepted", "--sources", "https://example.com/a,https://example.com/b", "--tags", "style").ID
	data, err := runCLI(t, "get", id, "--format", "text")
	if err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	out := string(data)
	for _, want := range []string{
		id + " · principle · accepted · v1 · global",
		"created by owner",
		"reviewed by owner",
		"tags: style",
		"sources: https://example.com/a, https://example.com/b",
		"\nKeep it short\n",
		"  First line.\n\n  Second paragraph.\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := runCLI(t, "get", id, "--format", "yaml"); err == nil {
		t.Fatal("expected a usage error for an unknown format")
	}
	if _, err := runCLI(t, "history", id, "--format", "text"); err == nil {
		t.Fatal("history takes no --format")
	}
}
