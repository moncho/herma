package cli

import (
	"strings"
	"testing"

	"github.com/moncho/herma/internal/store"
)

func TestPrinciplesCommandPrintsGlobalOrProject(t *testing.T) {
	db, p, _ := claudeHookFixture(t, 0, nil)
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Everywhere"})
	reviewed(t, db, store.CreateInput{Kind: "principle", Title: "Here only", ProjectID: p.ID})
	globalData, err := runCLI(t, "principles")
	if err != nil {
		t.Fatal(err)
	}
	global := string(globalData)
	if !strings.Contains(global, "## Everywhere") || strings.Contains(global, "Here only") {
		t.Fatalf("global:\n%s", global)
	}
	projectData, err := runCLI(t, "principles", "--project", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	project := string(projectData)
	if !strings.Contains(project, "## Here only") || strings.Contains(project, "Everywhere") {
		t.Fatalf("project:\n%s", project)
	}
}
