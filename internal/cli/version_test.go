package cli

import (
	"strings"
	"testing"

	"github.com/moncho/herma/internal/version"
)

func TestVersionPrintsTheStampedVersion(t *testing.T) {
	cleanEnv(t)
	data, err := runCLI(t, "version")
	if err != nil || strings.TrimSpace(string(data)) != "herma "+version.Version {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := runCLI(t, "version", "--extra"); err == nil {
		t.Fatal("version takes no options")
	}
}
