package workspace_test

import (
	"errors"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"

	"github.com/katakalyst/devsys/internal/workspace"
)

// ---------------------------------------------------------------------------
// PlatformFromURL
// ---------------------------------------------------------------------------

func TestPlatformFromURL(t *testing.T) {
	cases := []struct {
		url      string
		want     string
		wantErr  bool
	}{
		// GitHub — HTTPS and SCP SSH
		{"https://github.com/owner/repo.git", "github", false},
		{"git@github.com:owner/repo.git", "github", false},
		// GitLab.com — HTTPS and SCP SSH
		{"https://gitlab.com/owner/project.git", "gitlab", false},
		{"git@gitlab.com:owner/project.git", "gitlab", false},
		// Self-hosted GitLab (any non-github.com host)
		{"https://mygitlab.example.com/team/project.git", "gitlab", false},
		{"git@mygitlab.example.com:team/project.git", "gitlab", false},
		// HTTPS with embedded credentials (GitLab token-in-URL form)
		{"https://oauth2:glpat-xxx@gitlab.com/owner/project.git", "gitlab", false},
		// Unparseable
		{":bad::url", "", true},
	}

	for _, tc := range cases {
		got, err := workspace.PlatformFromURL(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("PlatformFromURL(%q): expected error, got %q", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("PlatformFromURL(%q): unexpected error: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PlatformFromURL(%q): want %q, got %q", tc.url, tc.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// RepoIDFromURL
// ---------------------------------------------------------------------------

func TestRepoIDFromURL(t *testing.T) {
	cases := []struct {
		url     string
		want    string
		wantErr bool
	}{
		// Plain HTTPS
		{"https://gitlab.com/owner/project.git", "owner-project", false},
		{"https://github.com/owner/repo.git", "owner-repo", false},
		// HTTPS without .git suffix
		{"https://gitlab.com/owner/project", "owner-project", false},
		// HTTPS with embedded credentials (token in URL)
		{"https://oauth2:glpat-xxx@gitlab.com/owner/project.git", "owner-project", false},
		// SCP-style SSH
		{"git@github.com:owner/repo.git", "owner-repo", false},
		{"git@gitlab.com:owner/project.git", "owner-project", false},
		// Nested namespace / GitLab subgroups
		{"https://gitlab.com/group/subgroup/project.git", "group-subgroup-project", false},
		{"git@gitlab.com:group/subgroup/project.git", "group-subgroup-project", false},
		// ssh:// URL form
		{"ssh://git@gitlab.com/owner/project.git", "owner-project", false},
		// Self-hosted GitLab
		{"https://mygit.example.com/team/project.git", "team-project", false},
		// Empty path
		{"https://github.com/", "", true},
	}

	for _, tc := range cases {
		got, err := workspace.RepoIDFromURL(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("RepoIDFromURL(%q): expected error, got %q", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("RepoIDFromURL(%q): unexpected error: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RepoIDFromURL(%q): want %q, got %q", tc.url, tc.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SecretName
// ---------------------------------------------------------------------------

func TestSecretName(t *testing.T) {
	cases := []struct {
		project, repoID, platform, want string
	}{
		{"myproject", "owner-repo", "gitlab", "devsys-myproject-owner-repo-gitlab-token"},
		{"myproject", "owner-repo", "github", "devsys-myproject-owner-repo-github-token"},
		{"myproject", "group-subgroup-project", "gitlab", "devsys-myproject-group-subgroup-project-gitlab-token"},
	}
	for _, tc := range cases {
		got := workspace.SecretName(tc.project, tc.repoID, tc.platform)
		if got != tc.want {
			t.Errorf("SecretName(%q,%q,%q): want %q, got %q",
				tc.project, tc.repoID, tc.platform, tc.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Repo.OriginURL
// ---------------------------------------------------------------------------

// initRepoWithRemote creates a temporary git repo with a given origin URL
// and returns a workspace.Repo pointing at it.
func initRepoWithRemote(t *testing.T, originURL string) workspace.Repo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	if originURL != "" {
		cfg, _ := repo.Config()
		cfg.Remotes["origin"] = &gitconfig.RemoteConfig{
			Name: "origin",
			URLs: []string{originURL},
		}
		if err := repo.SetConfig(cfg); err != nil {
			t.Fatalf("set origin: %v", err)
		}
	}
	return workspace.Repo{AbsPath: dir, RelPath: "."}
}

func TestOriginURL_HasOrigin(t *testing.T) {
	const wantURL = "https://gitlab.com/owner/project.git"
	r := initRepoWithRemote(t, wantURL)

	got, err := r.OriginURL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantURL {
		t.Errorf("want %q, got %q", wantURL, got)
	}
}

func TestOriginURL_NoOrigin(t *testing.T) {
	r := initRepoWithRemote(t, "") // no remote set

	_, err := r.OriginURL()
	if !errors.Is(err, workspace.ErrNoOrigin) {
		t.Errorf("want ErrNoOrigin, got %v", err)
	}
}

func TestOriginURL_NotARepo(t *testing.T) {
	r := workspace.Repo{AbsPath: t.TempDir(), RelPath: "."}
	_, err := r.OriginURL()
	if err == nil {
		t.Fatal("expected error for non-repo directory, got nil")
	}
}
