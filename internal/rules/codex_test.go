package rules

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func codexHomeFor(t *testing.T, agents string) string {
	t.Helper()
	home := t.TempDir()
	if agents != "" {
		if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(agents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func readAgents(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const block1 = Header + " -->\n# Global principles (reviewed in herma)\n\n## One\n"
const block2 = Header + " -->\n# Global principles (reviewed in herma)\n\n## Two\n"

func TestCodexBlockAppendsReplacesAndRemoves(t *testing.T) {
	home := codexHomeFor(t, "# Mine\n\nKeep this.\n")
	if changed, err := SyncCodexBlock(home, []byte(block1)); err != nil || !changed {
		t.Fatalf("append: %v %v", changed, err)
	}
	want := "# Mine\n\nKeep this.\n\n" + CodexStart + "\n" + block1 + CodexEnd + "\n"
	if got := readAgents(t, home); got != want {
		t.Fatalf("after append:\n%q\nwant:\n%q", got, want)
	}
	if changed, err := SyncCodexBlock(home, []byte(block1)); err != nil || changed {
		t.Fatalf("unchanged: %v %v", changed, err)
	}
	if _, err := SyncCodexBlock(home, []byte(block2)); err != nil {
		t.Fatal(err)
	}
	want2 := "# Mine\n\nKeep this.\n\n" + CodexStart + "\n" + block2 + CodexEnd + "\n"
	if got := readAgents(t, home); got != want2 {
		t.Fatalf("after replace:\n%q", got)
	}
	if changed, err := SyncCodexBlock(home, nil); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if got := readAgents(t, home); got != "# Mine\n\nKeep this.\n" {
		t.Fatalf("after remove: %q", got)
	}
}

func TestCodexBlockKeepsSurroundingBytesExactly(t *testing.T) {
	before := "# Mine\r\n\r\nA"
	after := "\r\nTail without newline"
	home := codexHomeFor(t, before+"\n\n"+CodexStart+"\nold\n"+CodexEnd+"\n"+after)
	if _, err := SyncCodexBlock(home, []byte(block1)); err != nil {
		t.Fatal(err)
	}
	want := before + "\n\n" + CodexStart + "\n" + block1 + CodexEnd + "\n" + after
	if got := readAgents(t, home); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCodexBlockCreatesAMissingFile(t *testing.T) {
	home := codexHomeFor(t, "")
	if _, err := SyncCodexBlock(home, []byte(block1)); err != nil {
		t.Fatal(err)
	}
	if got := readAgents(t, home); got != CodexStart+"\n"+block1+CodexEnd+"\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCodexBlockSkipsWithoutCodexHome(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if changed, err := SyncCodexBlock(missing, []byte(block1)); err != nil || changed {
		t.Fatalf("got %v %v", changed, err)
	}
}

func TestCodexBlockRefusesMalformedMarkers(t *testing.T) {
	for name, text := range map[string]string{
		"start only": "x\n" + CodexStart + "\nold\n",
		"end first":  CodexEnd + "\n" + CodexStart + "\n",
		"two blocks": CodexStart + "\na\n" + CodexEnd + "\n" + CodexStart + "\nb\n" + CodexEnd + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := codexHomeFor(t, text)
			if _, err := SyncCodexBlock(home, []byte(block1)); !errors.Is(err, ErrMalformedBlock) {
				t.Fatalf("err = %v", err)
			}
			if got := readAgents(t, home); got != text {
				t.Fatal("file changed")
			}
		})
	}
}

func TestCodexBlockRefusesASymlinkedFile(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "dotfiles-AGENTS.md")
	if err := os.WriteFile(target, []byte("# Mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncCodexBlock(home, []byte(block1)); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "# Mine\n" {
		t.Fatal("target changed")
	}
	if info, _ := os.Lstat(filepath.Join(home, "AGENTS.md")); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
}

func TestCodexBlockKeepsTheFileMode(t *testing.T) {
	home := codexHomeFor(t, "# Mine\n")
	if err := os.Chmod(filepath.Join(home, "AGENTS.md"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncCodexBlock(home, []byte(block1)); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(home, "AGENTS.md")); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}
