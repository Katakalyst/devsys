package cmd

import (
	"bufio"
	"io"
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
// githubExpiresAtLabels / promptGitHubExpiresAt — self-reported GitHub PAT
// expiry (GitHub's API never exposes it), stored under the same
// devsys.expires-at label GitLab's own minted tokens carry so
// tokenExpiryText's existing warning works for GitHub too.
// ---------------------------------------------------------------------------

func TestGithubExpiresAtLabels(t *testing.T) {
	labels, err := githubExpiresAtLabels("")
	if err != nil || labels != nil {
		t.Errorf("blank input: want (nil, nil), got (%v, %v)", labels, err)
	}

	labels, err = githubExpiresAtLabels("2027-01-15")
	if err != nil {
		t.Fatalf("valid date: unexpected error: %v", err)
	}
	if labels["devsys.expires-at"] != "2027-01-15" {
		t.Errorf("valid date: want label 2027-01-15, got %v", labels)
	}

	if _, err := githubExpiresAtLabels("not-a-date"); err == nil {
		t.Error("malformed date: expected error, got nil")
	}
	if _, err := githubExpiresAtLabels("01/15/2027"); err == nil {
		t.Error("wrong format date: expected error, got nil")
	}
}

func TestPromptGitHubExpiresAt(t *testing.T) {
	// Blank answer -> nil labels, no error, no re-prompt.
	labels, err := promptGitHubExpiresAt(bufio.NewReader(strings.NewReader("\n")))
	if err != nil || labels != nil {
		t.Errorf("blank answer: want (nil, nil), got (%v, %v)", labels, err)
	}

	// Valid date -> stored under the shared label key.
	labels, err = promptGitHubExpiresAt(bufio.NewReader(strings.NewReader("2027-06-01\n")))
	if err != nil {
		t.Fatalf("valid date: unexpected error: %v", err)
	}
	if labels["devsys.expires-at"] != "2027-06-01" {
		t.Errorf("valid date: want label 2027-06-01, got %v", labels)
	}

	// Malformed input is reprompted, not rejected outright.
	labels, err = promptGitHubExpiresAt(bufio.NewReader(strings.NewReader("nonsense\n2027-06-01\n")))
	if err != nil {
		t.Fatalf("reprompt after malformed input: unexpected error: %v", err)
	}
	if labels["devsys.expires-at"] != "2027-06-01" {
		t.Errorf("reprompt after malformed input: want label 2027-06-01, got %v", labels)
	}
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
// agentAuthVolumeName / createProjectContainerWithRepoSecrets — per-project
// Claude/Codex credential volumes (documents/TODO.md's per-project agent
// credential item), replacing the single machine-wide devsys-claude-auth/
// devsys-codex-auth every project used to share.
// ---------------------------------------------------------------------------

func TestAgentAuthVolumeName(t *testing.T) {
	cases := []struct {
		project, agent, want string
	}{
		{"myproject", "claude", "devsys-myproject-claude-auth"},
		{"myproject", "codex", "devsys-myproject-codex-auth"},
		{"foo-bar", "claude", "devsys-foo-bar-claude-auth"},
	}
	for _, tc := range cases {
		got := agentAuthVolumeName(tc.project, tc.agent)
		if got != tc.want {
			t.Errorf("agentAuthVolumeName(%q, %q) = %q, want %q", tc.project, tc.agent, got, tc.want)
		}
	}
}

func TestCreateProjectContainerWithRepoSecrets_MountsPerProjectAgentVolumes(t *testing.T) {
	root := t.TempDir()
	rec := podmanfake.Install(t, podmanfake.Options{})

	if err := createProjectContainerWithRepoSecrets("devsys-foo", "foo", root, "devsys-foo"); err != nil {
		t.Fatalf("createProjectContainerWithRepoSecrets: %v", err)
	}

	for _, vol := range []string{"devsys-foo-claude-auth", "devsys-foo-codex-auth", "devsys-foo-trivy-db"} {
		if !rec.HasCall("volume", "create", vol) {
			t.Errorf("expected podman volume create for %s; calls: %v", vol, rec.Calls())
		}
	}
	if !rec.HasCall("create", "--volume", "devsys-foo-claude-auth:/root/.claude") {
		t.Error("expected container create to mount devsys-foo-claude-auth at /root/.claude")
	}
	if !rec.HasCall("create", "--volume", "devsys-foo-codex-auth:/root/.codex") {
		t.Error("expected container create to mount devsys-foo-codex-auth at /root/.codex")
	}
	// A second project must get its own, distinctly-named volumes — never
	// the shared machine-wide names this replaces.
	if rec.HasCall("volume", "create", "devsys-claude-auth") || rec.HasCall("volume", "create", "devsys-codex-auth") {
		t.Error("must not create the old machine-wide devsys-claude-auth/devsys-codex-auth volumes")
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
// parseRepoAndPlatformArgs — Phase 5's scriptable "<project> [repo] [remote]
// [platform]" arg classification (Git Remote & Credential Spec §9).
// ---------------------------------------------------------------------------

func TestParseRepoAndPlatformArgs(t *testing.T) {
	cases := []struct {
		extra              []string
		repo, remote, plat string
		wantErr            bool
	}{
		{nil, "", "", "", false},
		{[]string{"frontend"}, "frontend", "", "", false},
		{[]string{"gitlab"}, "", "", "gitlab", false},
		{[]string{"GitHub"}, "", "", "github", false}, // case-insensitive
		{[]string{"frontend", "gitlab"}, "frontend", "", "gitlab", false},
		{[]string{"gitlab", "frontend"}, "frontend", "", "gitlab", false}, // order-independent
		{[]string{"frontend", "upstream"}, "frontend", "upstream", "", false},
		{[]string{"frontend", "upstream", "gitlab"}, "frontend", "upstream", "gitlab", false},
		{[]string{"gitlab", "frontend", "upstream"}, "frontend", "upstream", "gitlab", false}, // platform pulled out regardless of position
		{[]string{"gitlab", "github"}, "", "", "", true},                                      // platform given twice
		{[]string{"a", "b", "c", "d"}, "", "", "", true},                                      // too many
		{[]string{"a", "b", "c"}, "", "", "", true},                                            // three non-platform positionals
	}
	for _, tc := range cases {
		repo, remote, plat, err := parseRepoAndPlatformArgs(tc.extra)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRepoAndPlatformArgs(%v): expected error, got repo=%q remote=%q platform=%q", tc.extra, repo, remote, plat)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRepoAndPlatformArgs(%v): unexpected error: %v", tc.extra, err)
			continue
		}
		if repo != tc.repo || remote != tc.remote || plat != tc.plat {
			t.Errorf("parseRepoAndPlatformArgs(%v) = (%q, %q, %q), want (%q, %q, %q)", tc.extra, repo, remote, plat, tc.repo, tc.remote, tc.plat)
		}
	}
}

// ---------------------------------------------------------------------------
// selectRepoForScriptable — Spec §9's stated error cases: "[repo] omitted
// with 2+ repos present -> error listing the actual discovered paths, not
// a guess," and the same rule one level down for [remote].
// ---------------------------------------------------------------------------

func TestSelectRepoForScriptable(t *testing.T) {
	one := []repoAuthStatus{{Repo: workspace.Repo{RelPath: "."}}}
	two := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "."}},
		{Repo: workspace.Repo{RelPath: "frontend"}},
	}

	// Single repo, no repo arg -> resolves to the only one.
	got, err := selectRepoForScriptable(one, "", "")
	if err != nil {
		t.Fatalf("single repo, no arg: unexpected error: %v", err)
	}
	if got.Repo.RelPath != "." {
		t.Errorf("single repo, no arg: got %q, want %q", got.Repo.RelPath, ".")
	}

	// Multiple repos, no repo arg -> error naming the actual paths.
	_, err = selectRepoForScriptable(two, "", "")
	if err == nil {
		t.Fatal("multiple repos, no arg: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "frontend") {
		t.Errorf("multiple repos, no arg: error should list discovered paths, got %q", err.Error())
	}

	// Multiple repos, explicit repo arg -> resolves correctly.
	got, err = selectRepoForScriptable(two, "frontend", "")
	if err != nil {
		t.Fatalf("explicit repo arg: unexpected error: %v", err)
	}
	if got.Repo.RelPath != "frontend" {
		t.Errorf("explicit repo arg: got %q, want %q", got.Repo.RelPath, "frontend")
	}

	// Repo arg that doesn't exist -> error.
	_, err = selectRepoForScriptable(two, "doesnotexist", "")
	if err == nil {
		t.Fatal("nonexistent repo arg: expected error, got nil")
	}
}

func TestSelectRepoForScriptable_MultiRemote(t *testing.T) {
	multi := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "frontend"}, RemoteName: "origin", HasRemote: true},
		{Repo: workspace.Repo{RelPath: "frontend"}, RemoteName: "upstream", HasRemote: true},
	}

	// Repo has one remote row implicitly resolved when it's the only repo
	// and no remote arg is given -> ambiguous, must error naming both.
	_, err := selectRepoForScriptable(multi, "", "")
	if err == nil {
		t.Fatal("multiple remotes, no remote arg: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "origin") || !strings.Contains(err.Error(), "upstream") {
		t.Errorf("multiple remotes, no remote arg: error should list discovered remotes, got %q", err.Error())
	}

	// Explicit remote arg resolves unambiguously.
	got, err := selectRepoForScriptable(multi, "frontend", "upstream")
	if err != nil {
		t.Fatalf("explicit remote arg: unexpected error: %v", err)
	}
	if got.RemoteName != "upstream" {
		t.Errorf("explicit remote arg: got remote %q, want %q", got.RemoteName, "upstream")
	}

	// Remote arg that doesn't exist on this repo -> error.
	_, err = selectRepoForScriptable(multi, "frontend", "doesnotexist")
	if err == nil {
		t.Fatal("nonexistent remote arg: expected error, got nil")
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

func TestProjectRepoSecretNames_MultipleRemotesOnOneRepo(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/app.git"}}); err != nil {
		t.Fatalf("create origin: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "mirror", URLs: []string{"https://gitlab.com/owner/app-mirror.git"}}); err != nil {
		t.Fatalf("create mirror: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	names, err := projectRepoSecretNames("foo", root)
	if err != nil {
		t.Fatalf("projectRepoSecretNames: %v", err)
	}
	want := map[string]bool{
		"devsys-foo-owner-app-github-token":        true,
		"devsys-foo-owner-app-mirror-gitlab-token": true,
	}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want two entries covering both remotes", names)
	}
	for _, name := range names {
		if !want[name] {
			t.Errorf("unexpected secret name %q", name)
		}
	}
}

func TestGatherRepoStatuses_MultipleRemotesYieldOneRowEach(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/app.git"}}); err != nil {
		t.Fatalf("create origin: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "upstream", URLs: []string{"https://gitlab.com/owner/app-upstream.git"}}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{SecretExists: false})
	statuses, err := gatherRepoStatuses("foo", root)
	if err != nil {
		t.Fatalf("gatherRepoStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("want 2 statuses (one per remote), got %d: %+v", len(statuses), statuses)
	}
	names := map[string]string{}
	for _, st := range statuses {
		names[st.RemoteName] = st.Platform
	}
	if names["origin"] != "github" || names["upstream"] != "gitlab" {
		t.Errorf("want origin=github, upstream=gitlab, got %v", names)
	}
}

func TestGatherRepoStatuses_SingleRemoteYieldsOneStatus(t *testing.T) {
	root := t.TempDir()
	initTestRepoWithOrigin(t, root, "https://gitlab.com/owner/app.git")

	podmanfake.Install(t, podmanfake.Options{SecretExists: false})
	statuses, err := gatherRepoStatuses("foo", root)
	if err != nil {
		t.Fatalf("gatherRepoStatuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].RemoteName != "origin" {
		t.Fatalf("want exactly one origin status, got %+v", statuses)
	}
}

func TestRecreateContainerForAuth_DeclinedExplainsRealRecoveryPath(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: true,
	})
	injectStdin(t, "n\n")

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("capture stdout: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		r.Close()
	})

	err = recreateContainerForAuth("testproject", true)
	w.Close()
	os.Stdout = origStdout
	if err != nil {
		t.Fatalf("recreateContainerForAuth: %v", err)
	}
	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	out := string(outBytes)
	for _, want := range []string{"credentials are stored", "Exit every active 'devsys enter testproject' session", "devsys rebuild testproject"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got %q", want, out)
		}
	}
	if strings.Contains(out, "Run 'devsys auth testproject' again") || strings.Contains(out, "or 'devsys enter testproject'") {
		t.Errorf("output still suggests a command that cannot update secret mounts: %q", out)
	}
	for _, subcommand := range []string{"stop", "rm", "create"} {
		if rec.HasSubcommand(subcommand) {
			t.Errorf("declined recreation must not call podman %s; calls: %v", subcommand, rec.Calls())
		}
	}
}

// ---------------------------------------------------------------------------
// writeRemote / clearRemote — pure go-git operations
// ---------------------------------------------------------------------------

func TestWriteRemote_CreatesAndUpdates(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}

	if err := writeRemote(dir, "origin", "https://github.com/owner/repo.git"); err != nil {
		t.Fatalf("writeRemote: %v", err)
	}

	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rem, ok := cfg.Remotes["origin"]
	if !ok {
		t.Fatal("remote 'origin' not found after writeRemote")
	}
	if rem.URLs[0] != "https://github.com/owner/repo.git" {
		t.Errorf("want URL %q, got %q", "https://github.com/owner/repo.git", rem.URLs[0])
	}

	// Overwrite with a new URL.
	if err := writeRemote(dir, "origin", "https://gitlab.com/owner/repo.git"); err != nil {
		t.Fatalf("writeRemote overwrite: %v", err)
	}
	cfg, _ = repo.Config()
	if cfg.Remotes["origin"].URLs[0] != "https://gitlab.com/owner/repo.git" {
		t.Errorf("overwrite: want gitlab URL, got %q", cfg.Remotes["origin"].URLs[0])
	}
}

func TestClearRemote_RemovesRemote(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/repo.git"}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	if err := clearRemote(dir, "origin"); err != nil {
		t.Fatalf("clearRemote: %v", err)
	}

	cfg, _ := repo.Config()
	if _, ok := cfg.Remotes["origin"]; ok {
		t.Error("remote 'origin' still present after clearRemote")
	}
}

func TestClearRemote_NonexistentIsNoError(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := clearRemote(dir, "doesnotexist"); err != nil {
		t.Errorf("clearRemote nonexistent remote: expected no error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// storeRepoSecret — creates/overwrites via podman secret create/rm
// ---------------------------------------------------------------------------

func TestStoreRepoSecret_CreatesWhenAbsent(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: false})

	if err := storeRepoSecret("my-secret", "token-value", map[string]string{"devsys.expires-at": "2027-01-01"}); err != nil {
		t.Fatalf("storeRepoSecret: %v", err)
	}

	// Must call secret create, must not call secret rm (nothing to delete).
	if !rec.HasCall("secret", "create", "my-secret") {
		t.Errorf("expected podman secret create; calls: %v", rec.Calls())
	}
	if rec.HasSubcommand("rm") {
		t.Errorf("should not rm when secret doesn't exist; calls: %v", rec.Calls())
	}
}

func TestStoreRepoSecret_DeletesBeforeRecreate(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: true})

	if err := storeRepoSecret("existing-secret", "new-token", nil); err != nil {
		t.Fatalf("storeRepoSecret: %v", err)
	}

	// rm must precede create.
	if !rec.HasCall("secret", "rm", "existing-secret") {
		t.Errorf("expected podman secret rm; calls: %v", rec.Calls())
	}
	if !rec.HasCall("secret", "create", "existing-secret") {
		t.Errorf("expected podman secret create; calls: %v", rec.Calls())
	}
}

// ---------------------------------------------------------------------------
// revokeAndDeleteSecret — no-secret and github paths
// ---------------------------------------------------------------------------

func TestRevokeAndDeleteSecret_NoSecretIsNoOp(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: false})

	st := &repoAuthStatus{Platform: "github", SecretName: "", HasToken: false}
	if err := revokeAndDeleteSecret(st); err != nil {
		t.Fatalf("revokeAndDeleteSecret: %v", err)
	}

	if rec.HasSubcommand("rm") {
		t.Errorf("should not call podman secret rm when SecretName is empty; calls: %v", rec.Calls())
	}
}

func TestRevokeAndDeleteSecret_GitHubPrintsReminderAndDeletes(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	flush := captureStdout(t)

	st := &repoAuthStatus{
		Platform:   "github",
		RemoteURL:  "https://github.com/owner/repo.git",
		SecretName: "devsys-foo-owner-repo-github-token",
		HasToken:   true,
	}
	if err := revokeAndDeleteSecret(st); err != nil {
		t.Fatalf("revokeAndDeleteSecret: %v", err)
	}

	out := flush()
	if !strings.Contains(out, "revoke") {
		t.Errorf("expected revoke reminder in output, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// scriptableRemove
// ---------------------------------------------------------------------------

func TestScriptableRemove_NoTokenIsNoOp(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{SecretExists: false})
	flush := captureStdout(t)

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "."}, HasToken: false}
	changed, err := scriptableRemove(st)
	out := flush()
	if err != nil {
		t.Fatalf("scriptableRemove: %v", err)
	}
	if changed {
		t.Error("expected changed=false when no token to remove")
	}
	if !strings.Contains(out, "no token to remove") {
		t.Errorf("expected 'no token to remove' in output, got %q", out)
	}
}

func TestScriptableRemove_WithToken_RemovesAndReturnsChanged(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	flush := captureStdout(t)

	st := &repoAuthStatus{
		Repo:       workspace.Repo{RelPath: "."},
		Platform:   "github",
		RemoteURL:  "https://github.com/owner/repo.git",
		SecretName: "devsys-foo-owner-repo-github-token",
		HasToken:   true,
	}
	changed, err := scriptableRemove(st)
	flush()
	if err != nil {
		t.Fatalf("scriptableRemove: %v", err)
	}
	if !changed {
		t.Error("expected changed=true after removing token")
	}
	if st.HasToken || st.SecretName != "" {
		t.Errorf("st not updated after remove: HasToken=%v SecretName=%q", st.HasToken, st.SecretName)
	}
}

// ---------------------------------------------------------------------------
// scriptableEnsure
// ---------------------------------------------------------------------------

func TestScriptableEnsure_NoRemoteErrors(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{})

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "myrepo"}, HasRemote: false}
	_, err := scriptableEnsure("proj", st)
	if err == nil {
		t.Fatal("expected error when repo has no remote")
	}
	if !strings.Contains(err.Error(), "--create") {
		t.Errorf("expected error to mention --create, got %q", err.Error())
	}
}

func TestScriptableEnsure_AlreadyConfiguredPrintsStatus(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{})
	flush := captureStdout(t)

	st := &repoAuthStatus{
		Repo:      workspace.Repo{RelPath: "myrepo"},
		HasRemote: true,
		HasToken:  true,
		Platform:  "gitlab",
		ExpiresAt: "",
	}
	changed, err := scriptableEnsure("proj", st)
	out := flush()
	if err != nil {
		t.Fatalf("scriptableEnsure: %v", err)
	}
	if changed {
		t.Error("expected changed=false when already configured")
	}
	if !strings.Contains(out, "already configured") {
		t.Errorf("expected 'already configured' in output, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// runScriptableOperation dispatch
// ---------------------------------------------------------------------------

func TestRunScriptableOperation_RemoveNoToken(t *testing.T) {
	resetAuthScriptFlags(t)
	authScriptRemove = true
	podmanfake.Install(t, podmanfake.Options{})
	flush := captureStdout(t)

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "."}, HasToken: false}
	changed, err := runScriptableOperation("proj", st, "")
	flush()
	if err != nil {
		t.Fatalf("runScriptableOperation --remove no token: %v", err)
	}
	if changed {
		t.Error("expected changed=false for --remove with no token")
	}
}

func TestRunScriptableOperation_RotateNoToken_Errors(t *testing.T) {
	resetAuthScriptFlags(t)
	authScriptRotate = true
	podmanfake.Install(t, podmanfake.Options{})

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "."}, HasToken: false}
	_, err := runScriptableOperation("proj", st, "")
	if err == nil {
		t.Fatal("expected error for --rotate with no token")
	}
}

func TestRunScriptableOperation_CreateMissingPlatform_Errors(t *testing.T) {
	resetAuthScriptFlags(t)
	authScriptCreate = true
	podmanfake.Install(t, podmanfake.Options{})

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "."}, HasRemote: false}
	_, err := runScriptableOperation("proj", st, "") // platformArg = ""
	if err == nil {
		t.Fatal("expected error for --create without platform")
	}
	if !strings.Contains(err.Error(), "platform is required") {
		t.Errorf("expected platform-required error, got %q", err.Error())
	}
}

func TestRunScriptableOperation_DefaultEnsureNoRemote_Errors(t *testing.T) {
	resetAuthScriptFlags(t)
	podmanfake.Install(t, podmanfake.Options{})

	st := &repoAuthStatus{Repo: workspace.Repo{RelPath: "."}, HasRemote: false}
	_, err := runScriptableOperation("proj", st, "")
	if err == nil {
		t.Fatal("expected error for bare ensure with no remote")
	}
}

// ---------------------------------------------------------------------------
// printRepoListing — multi-remote shows remote name in parens
// ---------------------------------------------------------------------------

func TestPrintRepoListing_SingleRemote_NoParens(t *testing.T) {
	flush := captureStdout(t)
	statuses := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "myrepo"}, RemoteName: "origin", HasRemote: true, HasToken: true, Platform: "github"},
	}
	printRepoListing(statuses)
	out := flush()
	if strings.Contains(out, "(") {
		t.Errorf("single-remote repo should not show remote name in parens, got %q", out)
	}
	if !strings.Contains(out, "myrepo") {
		t.Errorf("expected repo path in output, got %q", out)
	}
}

func TestPrintRepoListing_MultiRemote_ShowsRemoteNameInParens(t *testing.T) {
	flush := captureStdout(t)
	statuses := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "myrepo"}, RemoteName: "origin", HasRemote: true, HasToken: true, Platform: "github"},
		{Repo: workspace.Repo{RelPath: "myrepo"}, RemoteName: "upstream", HasRemote: true, HasToken: false, Platform: "gitlab"},
	}
	printRepoListing(statuses)
	out := flush()
	if !strings.Contains(out, "(origin)") {
		t.Errorf("expected '(origin)' for multi-remote repo, got %q", out)
	}
	if !strings.Contains(out, "(upstream)") {
		t.Errorf("expected '(upstream)' for multi-remote repo, got %q", out)
	}
}

func TestPrintRepoListing_NoRemote(t *testing.T) {
	flush := captureStdout(t)
	statuses := []repoAuthStatus{
		{Repo: workspace.Repo{RelPath: "myrepo"}, HasRemote: false},
	}
	printRepoListing(statuses)
	out := flush()
	if !strings.Contains(out, "no remote") {
		t.Errorf("expected 'no remote' in output, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// gatherRepoStatuses — HasToken + ExpiresAt path
// ---------------------------------------------------------------------------

func TestGatherRepoStatuses_WithTokenAndExpiresAt(t *testing.T) {
	root := t.TempDir()
	initTestRepoWithOrigin(t, root, "https://github.com/owner/app.git")

	podmanfake.Install(t, podmanfake.Options{
		SecretExists:    true,
		SecretExpiresAt: "2027-06-01",
	})
	statuses, err := gatherRepoStatuses("foo", root)
	if err != nil {
		t.Fatalf("gatherRepoStatuses: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(statuses))
	}
	st := statuses[0]
	if !st.HasToken {
		t.Error("expected HasToken=true")
	}
	if st.ExpiresAt != "2027-06-01" {
		t.Errorf("expected ExpiresAt=%q, got %q", "2027-06-01", st.ExpiresAt)
	}
}

// ---------------------------------------------------------------------------
// recreateContainerForAuth — not-running path and non-interactive running path
// ---------------------------------------------------------------------------

func TestRecreateContainerForAuth_NotRunning_SkipsStop(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: false,
		ProjectPath:      t.TempDir(),
	})

	if err := recreateContainerForAuth("testproject", true); err != nil {
		t.Fatalf("recreateContainerForAuth: %v", err)
	}

	if rec.HasSubcommand("stop") {
		t.Errorf("should not call podman stop when container is not running; calls: %v", rec.Calls())
	}
	if !rec.HasSubcommand("rm") {
		t.Errorf("expected podman rm; calls: %v", rec.Calls())
	}
	if !rec.HasSubcommand("create") {
		t.Errorf("expected podman create; calls: %v", rec.Calls())
	}
}

func TestRecreateContainerForAuth_RunningNonInteractive_ProceedsWithoutConfirm(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: true,
		ProjectPath:      t.TempDir(),
	})
	flush := captureStdout(t)

	if err := recreateContainerForAuth("testproject", false); err != nil {
		t.Fatalf("recreateContainerForAuth: %v", err)
	}
	out := flush()

	if !strings.Contains(out, "non-interactive") {
		t.Errorf("expected non-interactive message in output, got %q", out)
	}
	if !rec.HasSubcommand("stop") {
		t.Errorf("expected podman stop; calls: %v", rec.Calls())
	}
	if !rec.HasSubcommand("rm") {
		t.Errorf("expected podman rm; calls: %v", rec.Calls())
	}
	if !rec.HasSubcommand("create") {
		t.Errorf("expected podman create; calls: %v", rec.Calls())
	}
}

// ---------------------------------------------------------------------------
// gitLabClient — errors when bootstrap PAT is missing
// ---------------------------------------------------------------------------

func TestGitLabClient_MissingBootstrapPAT_Errors(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{SecretExists: false})

	_, err := gitLabClient()
	if err == nil {
		t.Fatal("expected error when bootstrap PAT missing")
	}
	if !strings.Contains(err.Error(), "devsys auth gitlab") {
		t.Errorf("expected error to mention 'devsys auth gitlab', got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// runAuthProjectScriptable end-to-end — "no change needed" path:
// container exists + project path set + git repo with remote + secret exists
// → scriptableEnsure returns (false, nil)
// ---------------------------------------------------------------------------

func TestRunAuthProjectScriptable_NoChangeNeeded(t *testing.T) {
	resetAuthScriptFlags(t)

	root := t.TempDir()
	initTestRepoWithOrigin(t, root, "https://github.com/owner/app.git")

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ProjectPath:     root,
		SecretExists:    true,
	})
	flush := captureStdout(t)

	err := runAuthProjectScriptable("myproject", nil)
	out := flush()
	if err != nil {
		t.Fatalf("runAuthProjectScriptable: %v", err)
	}
	if !strings.Contains(out, "already configured") {
		t.Errorf("expected 'already configured' in output, got %q", out)
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
