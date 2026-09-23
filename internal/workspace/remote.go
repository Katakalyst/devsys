package workspace

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
)

// ErrNoOrigin is returned by OriginURL when the repository has no remote
// named "origin". The caller should treat this as the "no remote yet" branch
// from the spec (Git Remote & Credential Spec §8).
var ErrNoOrigin = errors.New("no origin remote configured")

// OriginURL reads the first URL of the "origin" remote from the repository's
// .git/config. The read is purely local — no network call is made (R3).
//
// Returns ErrNoOrigin when origin does not exist. Returns an error when the
// directory is not a git repository or the config cannot be read.
func (r Repo) OriginURL() (string, error) {
	repo, err := gogit.PlainOpen(r.AbsPath)
	if err != nil {
		return "", fmt.Errorf("cannot open git repository at %s: %w", r.AbsPath, err)
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		if errors.Is(err, gogit.ErrRemoteNotFound) {
			return "", ErrNoOrigin
		}
		return "", fmt.Errorf("cannot read origin remote: %w", err)
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return "", ErrNoOrigin
	}
	return urls[0], nil
}

// Remote is one named remote on a repo, as read from .git/config.
type Remote struct {
	Name string
	URL  string
}

// Remotes reads every remote configured on the repo — not just "origin" —
// sorted by name for deterministic listing/iteration order. Used by
// discovery/auth to enumerate all of a repo's remotes (Git Remote &
// Credential Spec §7's multi-remote decision), where OriginURL alone only
// ever covers the single-remote case.
//
// A remote with no URLs configured is skipped (nothing meaningful to derive
// a platform/repo-id from). Like OriginURL, this is a purely local read —
// no network call (R3).
func (r Repo) Remotes() ([]Remote, error) {
	repo, err := gogit.PlainOpen(r.AbsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open git repository at %s: %w", r.AbsPath, err)
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return nil, fmt.Errorf("cannot read remotes: %w", err)
	}
	var result []Remote
	for _, rem := range remotes {
		urls := rem.Config().URLs
		if len(urls) == 0 {
			continue
		}
		result = append(result, Remote{Name: rem.Config().Name, URL: urls[0]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// PlatformFromURL derives the platform name ("github" or "gitlab") from a
// remote URL. The derivation is from the URL alone — nothing is cached or
// stored separately (R3).
//
// Rule: if the URL's host is github.com, the platform is "github"; everything
// else is treated as "gitlab" (covers gitlab.com and self-hosted GitLab
// instances). GitHub Enterprise is not yet supported (see documents/TODO.md).
func PlatformFromURL(remoteURL string) (string, error) {
	host, err := hostFromRemoteURL(remoteURL)
	if err != nil {
		return "", fmt.Errorf("cannot determine platform: %w", err)
	}
	if host == "github.com" {
		return "github", nil
	}
	return "gitlab", nil
}

// PathFromURL derives the un-flattened owner/namespace + project path from a
// remote URL (e.g. "owner/project", or "group/subgroup/project" for a nested
// GitLab group) — the real platform path, still containing "/". Used
// anywhere that needs to display or reconstruct an actual platform path (API
// calls, PAT-scope prompts), as opposed to RepoIDFromURL's Podman-safe,
// flattened form.
//
// Handles all common URL forms:
//   - HTTPS: https://gitlab.com/owner/project.git
//   - HTTPS with embedded credentials: https://oauth2:TOKEN@gitlab.com/owner/project.git
//   - SCP-style SSH: git@github.com:owner/repo.git
//   - SSH URL: ssh://git@gitlab.com/owner/project.git
func PathFromURL(remoteURL string) (string, error) {
	var path string

	if strings.HasPrefix(remoteURL, "git@") {
		// SCP-style SSH: git@host:path[.git]
		// Split on the first ":" only — path may contain colons on exotic hosts.
		colon := strings.Index(remoteURL, ":")
		if colon < 0 {
			return "", fmt.Errorf("cannot parse SCP-style SSH URL: %q", remoteURL)
		}
		path = remoteURL[colon+1:]
	} else {
		// HTTPS or ssh:// — net/url handles embedded userinfo (oauth2:TOKEN@)
		// transparently; u.Path never includes the userinfo.
		u, err := url.Parse(remoteURL)
		if err != nil {
			return "", fmt.Errorf("cannot parse URL %q: %w", remoteURL, err)
		}
		path = u.Path
	}

	// Normalise: strip leading slash and optional .git suffix.
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSpace(path)

	if path == "" {
		return "", fmt.Errorf("cannot derive repo path from URL %q: path is empty", remoteURL)
	}
	return path, nil
}

// RepoIDFromURL derives the repo-id component used in Podman secret names
// from a remote URL. The repo-id is PathFromURL's value with "/" replaced by
// "-" to satisfy Podman's naming rules (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`, no
// slashes).
//
// GitLab nested subgroups (e.g. group/subgroup/project) are supported — the
// full path is flattened, not truncated.
func RepoIDFromURL(remoteURL string) (string, error) {
	path, err := PathFromURL(remoteURL)
	if err != nil {
		return "", err
	}
	// Flatten for Podman naming compatibility (spec §7: slashes rejected outright).
	return strings.ReplaceAll(path, "/", "-"), nil
}

// SecretName constructs the Podman secret name for a per-repo credential.
// Format: devsys-<project>-<repoID>-<platform>-token
// where repoID is the value returned by RepoIDFromURL (already flattened).
func SecretName(project, repoID, platform string) string {
	return fmt.Sprintf("devsys-%s-%s-%s-token", project, repoID, platform)
}

// hostFromRemoteURL extracts the hostname from any supported remote URL form.
func hostFromRemoteURL(remoteURL string) (string, error) {
	if strings.HasPrefix(remoteURL, "git@") {
		// git@host:path → host
		rest := strings.TrimPrefix(remoteURL, "git@")
		host, _, found := strings.Cut(rest, ":")
		if !found {
			return "", fmt.Errorf("cannot parse SCP-style SSH URL: %q", remoteURL)
		}
		return strings.ToLower(host), nil
	}
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", fmt.Errorf("cannot parse URL %q: %w", remoteURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in URL %q", remoteURL)
	}
	return strings.ToLower(u.Hostname()), nil
}
