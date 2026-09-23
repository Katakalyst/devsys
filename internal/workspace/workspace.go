// Package workspace implements discovery of git repositories within a devsys
// project workspace. It is used by both `devsys init` and `devsys auth` to
// find repos to configure credentials for.
package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Repo represents a git repository discovered inside a project workspace.
type Repo struct {
	// AbsPath is the absolute path to the repository root — the directory
	// that directly contains the .git entry.
	AbsPath string

	// RelPath is the path relative to the workspace root. It is "." when the
	// workspace root itself is a repository.
	RelPath string
}

// Depth returns how many directory levels below the workspace root this repo
// sits. The workspace root (RelPath == ".") has depth 0; a direct child
// (e.g. "frontend") has depth 1; "subdir/lib" has depth 2; and so on.
func (r Repo) Depth() int {
	if r.RelPath == "." {
		return 0
	}
	return strings.Count(r.RelPath, string(os.PathSeparator)) + 1
}

// DiscoverRepos walks workspaceRoot recursively and returns every git
// repository found, ordered shallow-to-deep. Within the same depth, repos
// are ordered alphabetically by RelPath for deterministic output.
//
// Both nested repos (a repo inside another repo's tree) and side-by-side
// repos (sibling directories each with their own .git) are returned. The spec
// treats them identically — if a .git is there, it was put there deliberately,
// and difficulty of discovery is not a reason to support it less (Git Remote &
// Credential Spec §7).
//
// A repo root is marked by a ".git" entry that is either a directory (an
// ordinary repo) or a file (a formal submodule or a linked worktree, both of
// which point elsewhere via a "gitdir: <path>" line). Submodules are real
// repos — their own remote, their own history (Spec §5 Scenario 9) — and are
// included exactly like any other. Worktrees are excluded: a linked worktree
// shares its main repo's remotes and has none of its own, so including it
// would surface a permanently-unconfigurable "no remote" row, not a real gap
// to fill. This distinction is read from git's own fixed internal layout (a
// worktree's gitdir always lives under <main-repo>/.git/worktrees/<name>, a
// submodule's under <parent>/.git/modules/<name>), not guessed — so it isn't
// the kind of heuristic filtering the rule below rejects.
//
// Noise such as a vendored .git inside node_modules is handled by ordering,
// not filtering: it sorts deep, after the repos the user actually cares about.
// Nothing else is excluded — no skip-list (spec §7 explicitly rejects
// heuristic filtering).
//
// The walk is always fresh; nothing is cached or read from a config file (R15).
// Permission errors on individual directories are skipped silently so an
// unreadable vendor tree doesn't abort discovery of everything else.
func DiscoverRepos(workspaceRoot string) ([]Repo, error) {
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve workspace path: %w", err)
	}

	var repos []Repo

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				// Root itself is inaccessible — propagate so the caller gets
				// a real error rather than an empty result.
				return err
			}
			// Skip unreadable subdirectories (permission denied inside vendor
			// dirs, etc.) rather than aborting the entire walk.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() != ".git" {
			return nil
		}

		if d.IsDir() {
			// path is the .git directory itself; its parent is the repo root.
			repoRoot := filepath.Dir(path)
			rel, relErr := filepath.Rel(abs, repoRoot)
			if relErr != nil {
				// Should not happen since repoRoot is always under abs.
				return fmt.Errorf("cannot compute relative path for %s: %w", repoRoot, relErr)
			}
			repos = append(repos, Repo{AbsPath: repoRoot, RelPath: rel})
			// Do not descend into .git itself — there are no repos inside there.
			return fs.SkipDir
		}

		// A ".git" file (submodule or worktree) — never a directory to
		// descend into, so no fs.SkipDir needed either way below.
		isWorktree, gitFileErr := gitFileIsWorktree(path)
		if gitFileErr != nil || isWorktree {
			return nil
		}
		repoRoot := filepath.Dir(path)
		rel, relErr := filepath.Rel(abs, repoRoot)
		if relErr != nil {
			return fmt.Errorf("cannot compute relative path for %s: %w", repoRoot, relErr)
		}
		repos = append(repos, Repo{AbsPath: repoRoot, RelPath: rel})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("cannot walk workspace: %w", walkErr)
	}

	sort.SliceStable(repos, func(i, j int) bool {
		di, dj := repos[i].Depth(), repos[j].Depth()
		if di != dj {
			return di < dj
		}
		return repos[i].RelPath < repos[j].RelPath
	})

	return repos, nil
}

// gitFileIsWorktree reads a ".git" file's "gitdir: <path>" pointer and
// reports whether that path lives under a .git/worktrees/ directory — git's
// own fixed layout for a linked worktree, as opposed to .git/modules/ for a
// formal submodule. A malformed or unreadable file is reported as an error;
// the caller treats that the same as "skip this entry," consistent with
// DiscoverRepos's existing "skip what can't be read" behavior.
func gitFileIsWorktree(gitFilePath string) (bool, error) {
	data, err := os.ReadFile(gitFilePath)
	if err != nil {
		return false, err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return false, fmt.Errorf("unrecognized .git file format in %s", gitFilePath)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	normalized := filepath.ToSlash(target)
	return strings.Contains(normalized, "/.git/worktrees/"), nil
}
