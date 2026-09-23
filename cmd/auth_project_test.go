package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/podmanfake"
	"github.com/katakalyst/devsys/internal/workspace"
)

func futureDate(days int) string {
	return time.Now().AddDate(0, 0, days).Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// parseSelection — Git Remote & Credential Spec §9: "a list of indices,
// separated by comma or space, always — never inferred from concatenated
// digits."
// ---------------------------------------------------------------------------

func TestParseSelection(t *testing.T) {
	cases := []struct {
		input   string
		max     int
		want    []int
		wantErr bool
	}{
		{"1,5", 5, []int{0, 4}, false},
		{"1 5", 5, []int{0, 4}, false},
		{"1, 5", 5, []int{0, 4}, false},
		{"15", 20, []int{14}, false}, // "15" is index fifteen, never "1" and "5"
		{"3", 5, []int{2}, false},
		{"1,1,2", 5, []int{0, 1}, false}, // duplicates collapse, order preserved
		{"", 5, nil, true},
		{"0", 5, nil, true},
		{"6", 5, nil, true},
		{"x", 5, nil, true},
	}
	for _, tc := range cases {
		got, err := parseSelection(tc.input, tc.max)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSelection(%q, %d): expected error, got %v", tc.input, tc.max, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSelection(%q, %d): unexpected error: %v", tc.input, tc.max, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseSelection(%q, %d) = %v, want %v", tc.input, tc.max, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseSelection(%q, %d) = %v, want %v", tc.input, tc.max, got, tc.want)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// tokenExpiryText — the ⚠ warning must trigger at exactly the same 30-day
// threshold devsys enter's checkTokenExpiry already uses (Spec §9: "one
// shared threshold, not a second number to keep in sync").
// ---------------------------------------------------------------------------

func TestTokenExpiryText(t *testing.T) {
	if got := tokenExpiryText(""); got != "token ok" {
		t.Errorf("empty expiry: want %q, got %q", "token ok", got)
	}
	if got := tokenExpiryText("not-a-date"); got != "token ok" {
		t.Errorf("malformed expiry: want %q, got %q", "token ok", got)
	}

	future := futureDate(60)
	if got := tokenExpiryText(future); containsWarning(got) {
		t.Errorf("expiry 60 days out should not warn: got %q", got)
	}

	soon := futureDate(10)
	if got := tokenExpiryText(soon); !containsWarning(got) {
		t.Errorf("expiry 10 days out should warn: got %q", got)
	}
}

func containsWarning(s string) bool {
	for _, r := range s {
		if r == '⚠' {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// repoDefaultName
// ---------------------------------------------------------------------------

func TestRepoDefaultName(t *testing.T) {
	cases := []struct {
		project, relPath, want string
	}{
		{"myproject", ".", "myproject"},
		{"myproject", "frontend", "myproject-frontend"},
		{"myproject", "libs/shared", "myproject-libs-shared"},
	}
	for _, tc := range cases {
		got := repoDefaultName(tc.project, tc.relPath)
		if got != tc.want {
			t.Errorf("repoDefaultName(%q, %q) = %q, want %q", tc.project, tc.relPath, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// embedTokenInHTTPSURL — both platforms now use this identically (Git
// Remote & Credential Spec §7's corrected GitHub decision).
// ---------------------------------------------------------------------------

// remoteHasEmbeddedCredentials must distinguish a URL auth itself wrote
// (embedded userinfo) from a bare URL the agent set via plain `git remote
// add` — this is what tells rotate whether it's safe to rewrite the remote
// (Git Remote & Credential Spec §9's rotate mechanical detail).
func TestRemoteHasEmbeddedCredentials(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://oauth2:glpat-xxx@gitlab.com/owner/project.git", true},
		{"https://x-access-token:ghp-yyy@github.com/owner/repo.git", true},
		{"https://gitlab.com/owner/project.git", false},
		{"https://github.com/owner/repo.git", false},
		{"git@github.com:owner/repo.git", false}, // SCP-style has no URL userinfo at all
	}
	for _, tc := range cases {
		if got := remoteHasEmbeddedCredentials(tc.url); got != tc.want {
			t.Errorf("remoteHasEmbeddedCredentials(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseRepoAndPlatformArgs — Phase 5's scriptable "<project> [repo]
// [platform]" arg classification (Git Remote & Credential Spec §9).
// ---------------------------------------------------------------------------

func TestParseRepoAndPlatformArgs(t *testing.T) {
	cases := []struct {
		extra      []string
		repo, plat string
		wantErr    bool
	}{
		{nil, "", "", false},
		{[]string{"frontend"}, "frontend", "", false},
		{[]string{"gitlab"}, "", "gitlab", false},
		{[]string{"GitHub"}, "", "github", false}, // case-insensitive
		{[]string{"frontend", "gitlab"}, "frontend", "gitlab", false},
		{[]string{"gitlab", "frontend"}, "frontend", "gitlab", false}, // order-independent
		{[]string{"frontend", "backend"}, "", "", true},               // two non-platform args
		{[]string{"gitlab", "github"}, "", "", true},                  // platform given twice
		{[]string{"a", "b", "c"}, "", "", true},                       // too many
	}
	for _, tc := range cases {
		repo, plat, err := parseRepoAndPlatformArgs(tc.extra)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRepoAndPlatformArgs(%v): expected error, got repo=%q platform=%q", tc.extra, repo, plat)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRepoAndPlatformArgs(%v): unexpected error: %v", tc.extra, err)
			continue
		}
		if repo != tc.repo || plat != tc.plat {
			t.Errorf("parseRepoAndPlatformArgs(%v) = (%q, %q), want (%q, %q)", tc.extra, repo, plat, tc.repo, tc.plat)
		}
	}
}

// ---------------------------------------------------------------------------
// selectRepoForScriptable — Spec §9's stated error case: "[repo] omitted
// with 2+ repos present -> error listing the actual discovered paths, not
// a guess."
// ---------------------------------------------------------------------------

func TestSelectRepoForScriptable(t *testing.T) {
	one := []repoAuthStatus{{Repo: workspace.Repo{RelPath: "."}}}
	two := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "."}},
		{Repo: workspace.Repo{RelPath: "frontend"}},
	}

	// Single repo, no repo arg -> resolves to the only one.
	got, err := selectRepoForScriptable(one, "")
	if err != nil {
		t.Fatalf("single repo, no arg: unexpected error: %v", err)
	}
	if got.Repo.RelPath != "." {
		t.Errorf("single repo, no arg: got %q, want %q", got.Repo.RelPath, ".")
	}

	// Multiple repos, no repo arg -> error naming the actual paths.
	_, err = selectRepoForScriptable(two, "")
	if err == nil {
		t.Fatal("multiple repos, no arg: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "frontend") {
		t.Errorf("multiple repos, no arg: error should list discovered paths, got %q", err.Error())
	}

	// Multiple repos, explicit repo arg -> resolves correctly.
	got, err = selectRepoForScriptable(two, "frontend")
	if err != nil {
		t.Fatalf("explicit repo arg: unexpected error: %v", err)
	}
	if got.Repo.RelPath != "frontend" {
		t.Errorf("explicit repo arg: got %q, want %q", got.Repo.RelPath, "frontend")
	}

	// Repo arg that doesn't exist -> error.
	_, err = selectRepoForScriptable(two, "doesnotexist")
	if err == nil {
		t.Fatal("nonexistent repo arg: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// runAuthProjectScriptable's early validation — both checks must fire
// before any Podman/workspace call, so they're reachable in a unit test
// even without a real container (Spec §9: a malformed invocation fails on
// the actual mistake, not something unrelated that happens to run first).
// ---------------------------------------------------------------------------

func TestRunAuthProjectScriptable_RejectsMultipleOperationFlags(t *testing.T) {
	resetAuthScriptFlags(t)
	authScriptCreate = true
	authScriptAttach = "owner/repo"

	err := runAuthProjectScriptable("whatever-project", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "only one of") {
		t.Errorf("expected mutual-exclusion error, got %q", err.Error())
	}
}

func TestRunAuthProjectScriptable_RejectsPlatformWithRotate(t *testing.T) {
	resetAuthScriptFlags(t)
	authScriptRotate = true

	err := runAuthProjectScriptable("whatever-project", []string{"gitlab"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not accepted") {
		t.Errorf("expected platform-not-accepted error, got %q", err.Error())
	}
}

func TestRunAuthProjectScriptable_RejectsPlatformWithBareForm(t *testing.T) {
	resetAuthScriptFlags(t)
	// No operation flag set — the bare "ensure" form — still shouldn't
	// accept a platform arg, same reasoning as --rotate/--remove.
	err := runAuthProjectScriptable("whatever-project", []string{"frontend", "github"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not accepted") {
		t.Errorf("expected platform-not-accepted error, got %q", err.Error())
	}
}

// resetAuthScriptFlags clears every scriptable-form package-level flag
// variable before and after a test that sets some of them, so tests don't
// leak state into each other via cobra's shared global flag vars.
func resetAuthScriptFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		authScriptCreate = false
		authScriptAttach = ""
		authScriptRotate = false
		authScriptRemove = false
		authScriptName = ""
		authScriptToken = ""
		authScriptForce = false
	}
	clear()
	t.Cleanup(clear)
}

func TestEmbedTokenInHTTPSURL(t *testing.T) {
	cases := []struct {
		rawURL, user, token, want string
	}{
		{"https://gitlab.com/owner/project", "oauth2", "glpat-xxx", "https://oauth2:glpat-xxx@gitlab.com/owner/project.git"},
		{"https://gitlab.com/owner/project.git", "oauth2", "glpat-xxx", "https://oauth2:glpat-xxx@gitlab.com/owner/project.git"},
		{"https://github.com/owner/repo", "x-access-token", "ghp-yyy", "https://x-access-token:ghp-yyy@github.com/owner/repo.git"},
	}
	for _, tc := range cases {
		got, err := embedTokenInHTTPSURL(tc.rawURL, tc.user, tc.token)
		if err != nil {
			t.Fatalf("embedTokenInHTTPSURL(%q): unexpected error: %v", tc.rawURL, err)
		}
		if got != tc.want {
			t.Errorf("embedTokenInHTTPSURL(%q) = %q, want %q", tc.rawURL, got, tc.want)
		}
	}
}

func TestProjectRepoSecretNames_DerivesExactNamesFromDiscoveredRepos(t *testing.T) {
	root := t.TempDir()
	initTestRepoWithOrigin(t, root, "https://github.com/owner/app.git")
	initTestRepoWithOrigin(t, filepath.Join(root, "backend"), "https://gitlab.com/team/backend.git")

	// Every exact candidate exists. The important regression assertion is
	// that the function derives candidates from these repos instead of listing
	// all secrets and prefix-matching "foo", which also matched "foo-bar".
	podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	names, err := projectRepoSecretNames("foo", root)
	if err != nil {
		t.Fatalf("projectRepoSecretNames: %v", err)
	}
	want := []string{
		"devsys-foo-owner-app-github-token",
		"devsys-foo-team-backend-gitlab-token",
	}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "devsys-foo-bar-") {
			t.Fatalf("matched prefix-related project foo-bar secret: %s", name)
		}
	}
}

func TestProjectRepoSecretNames_SkipsNoRemoteAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	initTestRepoWithOrigin(t, root, "https://github.com/owner/shared.git")
	initTestRepoWithOrigin(t, filepath.Join(root, "duplicate"), "https://github.com/owner/shared.git")
	if _, err := gogit.PlainInit(filepath.Join(root, "no-remote"), false); err != nil {
		t.Fatalf("init no-remote repo: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	names, err := projectRepoSecretNames("project", root)
	if err != nil {
		t.Fatalf("projectRepoSecretNames: %v", err)
	}
	if len(names) != 1 || names[0] != "devsys-project-owner-shared-github-token" {
		t.Fatalf("names = %v, want one deduplicated shared secret", names)
	}
}

func initTestRepoWithOrigin(t *testing.T, path, remoteURL string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	repo, err := gogit.PlainInit(path, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteURL},
	}); err != nil {
		t.Fatalf("create origin: %v", err)
	}
}
