package project

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linkWorktree lays out what `git worktree add` writes: the worktree's .git
// file names an admin directory inside the main repository, and that
// directory's gitdir file names the worktree's .git file back. relative
// writes both pointers the way git does with worktree.useRelativePaths.
func linkWorktree(t *testing.T, main, name string, relative bool) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(parent, name)
	admin := filepath.Join(main, ".git", "worktrees", name)
	for _, dir := range []string{worktree, admin} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	forward, back := admin, filepath.Join(worktree, ".git")
	if relative {
		if forward, err = filepath.Rel(worktree, admin); err != nil {
			t.Fatal(err)
		}
		if back, err = filepath.Rel(admin, back); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+forward+"\n")
	writeFile(t, filepath.Join(admin, "gitdir"), back+"\n")
	writeFile(t, filepath.Join(admin, "commondir"), "../..\n")
	return worktree
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertUnbound(t *testing.T, dir, why string) {
	t.Helper()
	binding, checkout, err := Discover(dir)
	if !errors.Is(err, ErrNoBinding) || checkout != "" || binding != (Binding{}) {
		t.Fatalf("%s: got %+v %q %v, want no binding", why, binding, checkout, err)
	}
}

func TestLinkedWorktreeInheritsTheMainCheckoutBinding(t *testing.T) {
	for _, relative := range []bool{false, true} {
		main := makeRepository(t)
		worktree := linkWorktree(t, main, "feature", relative)
		nested := filepath.Join(worktree, "src", "pkg")
		if err := os.MkdirAll(nested, 0755); err != nil {
			t.Fatal(err)
		}
		assertUnbound(t, nested, "main checkout has no binding")
		if _, err := Bind(main, Binding{ProjectID: projectA, MaxBytes: 4096}); err != nil {
			t.Fatal(err)
		}
		// The binding applies to the worktree, where the session's files are.
		binding, checkout, err := Discover(nested)
		if err != nil || checkout != worktree || binding != (Binding{ProjectID: projectA, MaxBytes: 4096}) {
			t.Fatalf("relative %t: inherited binding: %+v %q %v, want project A applying to %s", relative, binding, checkout, err, worktree)
		}
		// A binding of the worktree's own wins.
		if _, err := Bind(worktree, Binding{ProjectID: projectB}); err != nil {
			t.Fatal(err)
		}
		binding, checkout, err = Discover(nested)
		if err != nil || checkout != worktree || binding.ProjectID != projectB {
			t.Fatalf("relative %t: own binding: %+v %q %v, want project B", relative, binding, checkout, err)
		}
	}
}

// A folder can claim to be a worktree of any repository by writing a .git
// file. Only the repository's own record of the worktree is believed, and a
// folder cannot write that.
func TestWorktreeBindingNeedsTheRepositoryToNameTheWorktree(t *testing.T) {
	main := makeRepository(t)
	if _, err := Bind(main, Binding{ProjectID: projectA}); err != nil {
		t.Fatal(err)
	}
	real := linkWorktree(t, main, "feature", false)
	admin := filepath.Join(main, ".git", "worktrees", "feature")
	if _, _, err := Discover(real); err != nil {
		t.Fatalf("the registered worktree: %v", err)
	}

	// The same worktree with its .git file padded past the size limit is not read.
	padded := linkWorktree(t, main, "padded", false)
	writeFile(t, filepath.Join(padded, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "padded")+strings.Repeat("\n", 8192))
	assertUnbound(t, padded, "an oversized .git file")
	writeFile(t, filepath.Join(padded, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "padded")+strings.Repeat("\n", 16))
	if _, _, err := Discover(padded); err != nil {
		t.Fatalf("the same .git file within the size limit: %v", err)
	}

	impostor := t.TempDir()
	writeFile(t, filepath.Join(impostor, ".git"), "gitdir: "+admin+"\n")
	assertUnbound(t, impostor, "a folder naming another worktree's admin directory")

	// An admin directory the folder brought along proves nothing either: it
	// must sit in the repository whose binding is read.
	forged := t.TempDir()
	ownAdmin := filepath.Join(forged, "admin")
	if err := os.Mkdir(ownAdmin, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(forged, ".git"), "gitdir: "+ownAdmin+"\n")
	writeFile(t, filepath.Join(ownAdmin, "gitdir"), filepath.Join(forged, ".git")+"\n")
	writeFile(t, filepath.Join(ownAdmin, "commondir"), filepath.Join(main, ".git")+"\n")
	assertUnbound(t, forged, "a folder with its own admin directory pointing at the repository")

	// Nor does one that only appears to sit in a repository, through a symlink.
	staged, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	decoy, elsewhere, guest := filepath.Join(staged, "repo"), filepath.Join(staged, "elsewhere"), filepath.Join(staged, "guest")
	for _, dir := range []string{filepath.Join(decoy, ".git"), filepath.Join(elsewhere, "feature"), guest} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(decoy, ".git", "worktrees")); err != nil {
		t.Fatal(err)
	}
	if _, err := Bind(decoy, Binding{ProjectID: projectB}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(guest, ".git"), "gitdir: "+filepath.Join(decoy, ".git", "worktrees", "feature")+"\n")
	writeFile(t, filepath.Join(elsewhere, "feature", "gitdir"), filepath.Join(guest, ".git")+"\n")
	assertUnbound(t, guest, "an admin directory reached through a symlink")

	// Reaching the real admin directory through a symlinked .git is refused.
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	assertUnbound(t, linked, "a symlinked .git file")
}

func TestWorktreeBindingIgnoresOtherGitFiles(t *testing.T) {
	main := makeRepository(t)
	if _, err := Bind(main, Binding{ProjectID: projectA}); err != nil {
		t.Fatal(err)
	}
	// A submodule's .git file points into .git/modules, not .git/worktrees.
	module := filepath.Join(main, ".git", "modules", "lib")
	submodule := filepath.Join(main, "lib")
	for _, dir := range []string{module, submodule} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(submodule, ".git"), "gitdir: "+module+"\n")
	writeFile(t, filepath.Join(module, "gitdir"), filepath.Join(submodule, ".git")+"\n")
	assertUnbound(t, submodule, "a submodule")

	// A bare repository has no main checkout; the folder that holds it is not one.
	holder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bareAdmin, bareWorktree := filepath.Join(holder, "repo.git", "worktrees", "feature"), filepath.Join(holder, "feature")
	for _, dir := range []string{bareAdmin, bareWorktree} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Bind(filepath.Join(holder, "repo.git", "worktrees"), Binding{ProjectID: projectB}); err != nil {
		t.Fatal(err)
	}
	if _, err := Bind(holder, Binding{ProjectID: projectB}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(bareWorktree, ".git"), "gitdir: "+bareAdmin+"\n")
	writeFile(t, filepath.Join(bareAdmin, "gitdir"), filepath.Join(bareWorktree, ".git")+"\n")
	assertUnbound(t, bareWorktree, "a worktree of a bare repository")

	for name, content := range map[string]string{
		"empty": "", "no prefix": filepath.Join(main, ".git", "worktrees", "x") + "\n",
		"oversized": "gitdir: " + strings.Repeat("x", 8192) + "\n",
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".git"), content)
		assertUnbound(t, dir, name+" .git file")
	}
}

func TestInvalidMainBindingStopsWorktreeDiscovery(t *testing.T) {
	main := makeRepository(t)
	worktree := linkWorktree(t, main, "feature", false)
	writeFile(t, filepath.Join(main, FileName), `{"project_id":"not-a-record"}`)
	binding, checkout, err := Discover(worktree)
	if err == nil || errors.Is(err, ErrNoBinding) || binding != (Binding{}) || checkout != "" {
		t.Fatalf("invalid main binding: %+v %q %v, want an error", binding, checkout, err)
	}
}

// The layout above is what this package assumes git writes; this checks the
// assumption against the installed git.
func TestWorktreeBindingWithRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, worktree := filepath.Join(root, "main"), filepath.Join(root, "feature")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.Mkdir(main, 0755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q")
	git(main, "commit", "-q", "--allow-empty", "-m", "first")
	git(main, "worktree", "add", "-q", worktree)
	if _, err := Bind(main, Binding{ProjectID: projectA}); err != nil {
		t.Fatal(err)
	}
	binding, checkout, err := Discover(worktree)
	if err != nil || checkout != worktree || binding.ProjectID != projectA {
		t.Fatalf("git worktree: %+v %q %v, want project A applying to %s", binding, checkout, err, worktree)
	}
	// Once git forgets the worktree, the folder no longer inherits.
	git(main, "worktree", "remove", "--force", worktree)
	if err := os.Mkdir(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "feature")+"\n")
	assertUnbound(t, worktree, "a worktree git has removed")
}
