package rules

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSyncCreatesSelfIgnoringDirectoryAndReportsChanges(t *testing.T) {
	root := t.TempDir()
	changed, err := Sync(root, []byte(Header+"v1\n"))
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	if got := read(t, filepath.Join(root, Path)); got != Header+"v1\n" {
		t.Fatalf("file = %q", got)
	}
	if got := read(t, filepath.Join(root, ".claude", "rules", "herma", ".gitignore")); got != "*\n" {
		t.Fatalf(".gitignore = %q", got)
	}
	before, _ := os.Stat(filepath.Join(root, Path))
	if changed, err := Sync(root, []byte(Header+"v1\n")); err != nil || changed {
		t.Fatalf("identical sync: changed=%v err=%v", changed, err)
	}
	after, _ := os.Stat(filepath.Join(root, Path))
	if !os.SameFile(before, after) {
		t.Fatal("identical content rewrote the file")
	}
	if changed, err := Sync(root, []byte(Header+"v2\n")); err != nil || !changed || read(t, filepath.Join(root, Path)) != Header+"v2\n" {
		t.Fatalf("updated sync: changed=%v err=%v", changed, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".claude", "rules", "herma"))
	if len(entries) != 2 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestSyncAcceptsAnExistingIgnoreAllGitignore(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "rules", "herma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(root, []byte(Header+"\n")); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := read(t, filepath.Join(dir, ".gitignore")); got != "*\n" {
		t.Fatalf(".gitignore rewritten: %q", got)
	}
}

func TestSyncRefusesAGitignoreThatDoesNotIgnoreEverything(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"custom":    func(t *testing.T, path string) { writeFile(t, path, "custom\n") },
		"empty":     func(t *testing.T, path string) { writeFile(t, path, "") },
		"negated":   func(t *testing.T, path string) { writeFile(t, path, "*\n!principles.md\n") },
		"symlinked": func(t *testing.T, path string) { linkTo(t, path, "*\n") },
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".claude", "rules", "herma")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".gitignore")
			plant(t, path)
			before, _ := os.Lstat(path)
			beforeContent := read(t, path)
			if _, err := Sync(root, []byte(Header+"\n")); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
			after, err := os.Lstat(path)
			if err != nil || after.Mode() != before.Mode() || !os.SameFile(before, after) || read(t, path) != beforeContent {
				t.Fatalf(".gitignore changed: %v %v", after, err)
			}
			if _, err := os.Lstat(filepath.Join(root, Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("principles.md written: %v", err)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linkTo(t *testing.T, path, content string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "gitignore")
	writeFile(t, target, content)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestSyncEmptyContentRemovesOnlyThePrinciplesFile(t *testing.T) {
	root := t.TempDir()
	if changed, err := Sync(root, nil); err != nil || changed {
		t.Fatalf("empty sync on a bare checkout: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty sync created directories")
	}
	if _, err := Sync(root, []byte(Header+"\n")); err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(root, nil); err != nil || !changed {
		t.Fatalf("empty sync after content: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(filepath.Join(root, Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("principles file still present")
	}
	if read(t, filepath.Join(root, ".claude", "rules", "herma", ".gitignore")) != "*\n" {
		t.Fatal(".gitignore removed")
	}
}

func TestSyncRefusesSymlinkedDirectories(t *testing.T) {
	for _, linked := range []string{".claude", filepath.Join(".claude", "rules"), filepath.Join(".claude", "rules", "herma")} {
		t.Run(linked, func(t *testing.T) {
			root, elsewhere := t.TempDir(), t.TempDir()
			if parent := filepath.Dir(filepath.Join(root, linked)); parent != root {
				if err := os.MkdirAll(parent, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(elsewhere, filepath.Join(root, linked)); err != nil {
				t.Fatal(err)
			}
			if _, err := Sync(root, []byte(Header+"\n")); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
			if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
				t.Fatalf("wrote through the symlink: %v", entries)
			}
		})
	}
}

func TestSyncRefusesASymlinkedPrinciplesFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "rules", "herma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim.md")
	if err := os.WriteFile(target, []byte(Header+"keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "principles.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"write": []byte(Header + "\n"), "remove": nil} {
		if _, err := Sync(root, content); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("%s: err = %v, want ErrUnsafePath", name, err)
		}
		if read(t, target) != Header+"keep me\n" {
			t.Fatalf("%s: changed the symlink target", name)
		}
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: symlink replaced or removed: %v %v", name, info, err)
		}
		if dest, err := os.Readlink(link); err != nil || dest != target {
			t.Fatalf("%s: symlink retargeted: %q %v", name, dest, err)
		}
	}
}

func TestSyncLeavesAPrinciplesFileHermaDidNotWrite(t *testing.T) {
	for name, content := range map[string][]byte{"write": []byte(Header + "\n"), "remove": nil} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".claude", "rules", "herma")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, ".gitignore"), "*\n")
			writeFile(t, filepath.Join(root, Path), "# my own rules\n")
			if _, err := Sync(root, content); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
			if got := read(t, filepath.Join(root, Path)); got != "# my own rules\n" {
				t.Fatalf("file changed: %q", got)
			}
		})
	}
}

func TestSyncReplacesAndRemovesAHermaGeneratedFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "rules", "herma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".gitignore"), "*\n")
	writeFile(t, filepath.Join(root, Path), Header+"old\n")
	if changed, err := Sync(root, []byte(Header+"new\n")); err != nil || !changed {
		t.Fatalf("replace: changed=%v err=%v", changed, err)
	}
	if got := read(t, filepath.Join(root, Path)); got != Header+"new\n" {
		t.Fatalf("file = %q", got)
	}
	if changed, err := Sync(root, nil); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(filepath.Join(root, Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present: %v", err)
	}
}

func TestSyncRefusesNonRegularPrinciplesFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(root, []byte(Header+"\n")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("write over a directory: %v", err)
	}
	if _, err := Sync(root, nil); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("remove a directory: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(root, Path)); err != nil || !info.IsDir() {
		t.Fatal("directory was removed or replaced")
	}
}

func TestSyncReportsWriteFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "rules", "herma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := Sync(root, []byte(Header+"\n")); err == nil || errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want a write error", err)
	}
}

func TestSyncGlobalWritesReplacesAndRemoves(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(Header + " -->\n# Global principles (reviewed in herma)\n\n## One\n")
	changed, err := SyncGlobal(home, content)
	if err != nil || !changed {
		t.Fatalf("first sync: %v %v", changed, err)
	}
	got, err := os.ReadFile(filepath.Join(home, GlobalPath))
	if err != nil || string(got) != string(content) {
		t.Fatalf("file %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "rules", "herma", ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("global sync must not write a .gitignore: %v", err)
	}
	if changed, err := SyncGlobal(home, content); err != nil || changed {
		t.Fatalf("same content: %v %v", changed, err)
	}
	if changed, err := SyncGlobal(home, nil); err != nil || !changed {
		t.Fatalf("removal: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(home, GlobalPath)); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
}

func TestSyncGlobalSkipsWithoutClaudeHome(t *testing.T) {
	home := t.TempDir()
	if changed, err := SyncGlobal(home, []byte(Header+" -->\n")); err != nil || changed {
		t.Fatalf("got %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("created ~/.claude")
	}
}

func TestSyncGlobalRefusesSymlinkedRulesDir(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".claude", "rules")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncGlobal(home, []byte(Header+" -->\n")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("wrote through the symlink")
	}
}

func TestSyncGlobalLeavesAHandWrittenFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "rules", "herma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "global-principles.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncGlobal(home, []byte(Header+" -->\n")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v", err)
	}
}
