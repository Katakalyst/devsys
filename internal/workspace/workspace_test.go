package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katakalyst/devsys/internal/workspace"
)

// mkGit creates a bare .git directory at dir/.git, making dir a discovered
// repo. It does not initialise a real git repo — discovery only cares that
// .git is a directory, not that it contains valid git state.
func mkGit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkGit %s: %v", dir, err)
	}
}

func TestDiscoverRepos_Empty(t *testing.T) {
	root := t.TempDir()
	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("want 0 repos, got %d: %v", len(repos), repos)
	}
}

func TestDiscoverRepos_RootOnly(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(repos))
	}
	if repos[0].RelPath != "." {
		t.Errorf("RelPath: want \".\", got %q", repos[0].RelPath)
	}
	if repos[0].AbsPath != root {
		t.Errorf("AbsPath: want %q, got %q", root, repos[0].AbsPath)
	}
	if repos[0].Depth() != 0 {
		t.Errorf("Depth: want 0, got %d", repos[0].Depth())
	}
}

// TestDiscoverRepos_SideBySide covers Scenario 10 from the spec: sibling
// directories each with their own .git, with no repo at the workspace root.
func TestDiscoverRepos_SideBySide(t *testing.T) {
	root := t.TempDir()
	mkGit(t, filepath.Join(root, "backend"))
	mkGit(t, filepath.Join(root, "frontend"))

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 repos, got %d", len(repos))
	}
	// Alphabetical within same depth.
	if repos[0].RelPath != "backend" || repos[1].RelPath != "frontend" {
		t.Errorf("unexpected order: %v", relPaths(repos))
	}
}

// TestDiscoverRepos_Nested covers Scenario 9: a nested repo inside another
// repo's directory tree.
func TestDiscoverRepos_Nested(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	mkGit(t, filepath.Join(root, "vendor", "lib"))

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 repos, got %d: %v", len(repos), relPaths(repos))
	}
	// Shallow before deep.
	if repos[0].RelPath != "." {
		t.Errorf("first repo: want \".\", got %q", repos[0].RelPath)
	}
	if repos[1].RelPath != filepath.Join("vendor", "lib") {
		t.Errorf("second repo: want %q, got %q", filepath.Join("vendor", "lib"), repos[1].RelPath)
	}
}

// TestDiscoverRepos_ShallowBeforeDeep verifies the ordering guarantee across
// multiple depths.
func TestDiscoverRepos_ShallowBeforeDeep(t *testing.T) {
	root := t.TempDir()
	mkGit(t, filepath.Join(root, "deep", "a", "b"))
	mkGit(t, filepath.Join(root, "mid"))
	mkGit(t, root)

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("want 3 repos, got %d: %v", len(repos), relPaths(repos))
	}

	wantOrder := []string{".", "mid", filepath.Join("deep", "a", "b")}
	for i, want := range wantOrder {
		if repos[i].RelPath != want {
			t.Errorf("repos[%d].RelPath: want %q, got %q", i, want, repos[i].RelPath)
		}
	}
}

// TestDiscoverRepos_AlphabeticalWithinDepth verifies that repos at the same
// depth are sorted alphabetically.
func TestDiscoverRepos_AlphabeticalWithinDepth(t *testing.T) {
	root := t.TempDir()
	mkGit(t, filepath.Join(root, "zebra"))
	mkGit(t, filepath.Join(root, "alpha"))
	mkGit(t, filepath.Join(root, "middle"))

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"alpha", "middle", "zebra"}
	got := relPaths(repos)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repos[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

// TestDiscoverRepos_DoesNotDescendIntoDotGit confirms that files inside .git
// directories are not mistaken for repos and that the walk correctly skips the
// .git directory contents.
func TestDiscoverRepos_DoesNotDescendIntoDotGit(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	// Create a .git inside the workspace root's .git — this must never be
	// reported as a separate repo.
	if err := os.MkdirAll(filepath.Join(root, ".git", "modules", "sub", ".git"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("want exactly 1 repo, got %d: %v", len(repos), relPaths(repos))
	}
}

// TestDiscoverRepos_NonexistentRoot returns an error rather than panicking.
func TestDiscoverRepos_NonexistentRoot(t *testing.T) {
	_, err := workspace.DiscoverRepos("/this/path/does/not/exist/devsys-test")
	if err == nil {
		t.Fatal("expected error for nonexistent root, got nil")
	}
}

// TestDiscoverRepos_Depth verifies the Depth() helper.
func TestDiscoverRepos_Depth(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	mkGit(t, filepath.Join(root, "a"))
	mkGit(t, filepath.Join(root, "a", "b"))

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantDepths := []int{0, 1, 2}
	for i, want := range wantDepths {
		if got := repos[i].Depth(); got != want {
			t.Errorf("repos[%d] (%s) Depth: want %d, got %d", i, repos[i].RelPath, want, got)
		}
	}
}

func relPaths(repos []workspace.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.RelPath
	}
	return out
}

// ---------------------------------------------------------------------------
// Submodules and linked worktrees — .git as a *file* (gitdir indirection),
// not a directory. Spec §5 Scenario 9 puts formal submodules in the same
// bucket as any other repo; linked worktrees share their main repo's remotes
// and are deliberately excluded (documents/TODO.md).
// ---------------------------------------------------------------------------

// mkGitFile writes dir/.git as a file pointing at target via "gitdir: ",
// the real layout git itself uses for both submodules and linked worktrees.
func mkGitFile(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkGitFile mkdir %s: %v", dir, err)
	}
	content := "gitdir: " + target + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644); err != nil {
		t.Fatalf("mkGitFile write %s: %v", dir, err)
	}
}

func TestDiscoverRepos_SubmoduleGitFileIsDiscovered(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	// A submodule's .git file points into its parent's .git/modules/<name> —
	// this is what a real `git submodule add` produces.
	mkGitFile(t, filepath.Join(root, "vendor", "lib"), "../../.git/modules/vendor/lib")

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{".", filepath.Join("vendor", "lib")}
	got := relPaths(repos)
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repos[%d]: want %q, got %q (full: %v)", i, want[i], got[i], got)
		}
	}
}

func TestDiscoverRepos_LinkedWorktreeIsExcluded(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	// A linked worktree's .git file points into its main repo's own
	// .git/worktrees/<name> — this is what a real `git worktree add` produces.
	// The path is intentionally absolute with forward slashes, matching what
	// git itself writes on Windows.
	mkGitFile(t, filepath.Join(root, "wt-branch"), filepath.ToSlash(filepath.Join(root, ".git", "worktrees", "wt-branch")))

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"."}
	got := relPaths(repos)
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("want only the root repo (worktree excluded), got %v", got)
	}
}

func TestDiscoverRepos_MalformedGitFileIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	mkGit(t, root)
	if err := os.MkdirAll(filepath.Join(root, "broken"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken", ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	repos, err := workspace.DiscoverRepos(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"."}
	got := relPaths(repos)
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("want only the root repo (malformed .git skipped, not fatal), got %v", got)
	}
}
