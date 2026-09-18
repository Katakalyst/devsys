package cmd

// Tests for pure helper functions that don't require Podman or GitLab.
// These live in package cmd (not cmd_test) so they can access unexported helpers.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podmanfake"
	"github.com/katakalyst/devsys/internal/registry"
)

// ---------------------------------------------------------------------------
// webURLToSSH
// ---------------------------------------------------------------------------

func TestWebURLToSSH(t *testing.T) {
	tests := []struct {
		name    string
		webURL  string
		baseURL string
		want    string
	}{
		{
			name:    "gitlab.com project",
			webURL:  "https://gitlab.com/namespace/myproject",
			baseURL: "https://gitlab.com",
			want:    "git@gitlab.com:namespace/myproject.git",
		},
		{
			name:    "self-hosted gitlab",
			webURL:  "https://git.internal.example.com/team/repo",
			baseURL: "https://git.internal.example.com",
			want:    "git@git.internal.example.com:team/repo.git",
		},
		{
			name:    "nested namespace",
			webURL:  "https://gitlab.com/group/subgroup/project",
			baseURL: "https://gitlab.com",
			want:    "git@gitlab.com:group/subgroup/project.git",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := webURLToSSH(tc.webURL, tc.baseURL)
			if got != tc.want {
				t.Errorf("webURLToSSH(%q, %q) = %q; want %q", tc.webURL, tc.baseURL, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// containerToProject
// ---------------------------------------------------------------------------

func TestContainerToProject(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"devsys-myproject", "myproject"},
		{"devsys-foo-bar", "foo-bar"},
		// len("devsys-") == 7; the function requires len > 7, so bare "devsys-" is unchanged.
		{"devsys-", "devsys-"},
		{"notdevsys-project", "notdevsys-project"}, // no prefix → unchanged
		{"devsys", "devsys"},                       // no dash → unchanged
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := containerToProject(tc.input)
			if got != tc.want {
				t.Errorf("containerToProject(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// hostFromURL
// ---------------------------------------------------------------------------

func TestHostFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://gitlab.com", "gitlab.com"},
		{"https://git.internal.example.com", "git.internal.example.com"},
		{"http://mygit.corp", "mygit.corp"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := hostFromURL(tc.input)
			if got != tc.want {
				t.Errorf("hostFromURL(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// remoteNameFromURL
// ---------------------------------------------------------------------------

func TestRemoteNameFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.com/ns/proj.git", "github"},
		{"git@github.com:ns/proj.git", "github"},
		{"https://bitbucket.org/ns/proj.git", "bitbucket"},
		{"git@bitbucket.org:ns/proj.git", "bitbucket"},
		{"https://dev.azure.com/org/proj/_git/repo", "azure"},
		{"git@gitlab.mycompany.com:ns/proj.git", "mycompany"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := remoteNameFromURL(tc.input)
			if got != tc.want {
				t.Errorf("remoteNameFromURL(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// hostFromURL — fallback path
// ---------------------------------------------------------------------------

func TestHostFromURL_Fallback(t *testing.T) {
	// A bare hostname with no scheme triggers the manual-strip fallback.
	got := hostFromURL("gitlab.example.com")
	if got != "gitlab.example.com" {
		t.Errorf("hostFromURL(bare host) = %q; want %q", got, "gitlab.example.com")
	}
}

// ---------------------------------------------------------------------------
// containerField
// ---------------------------------------------------------------------------

func TestContainerField(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]interface{}
		key  string
		want string
	}{
		{
			name: "string value",
			m:    map[string]interface{}{"Names": "devsys-myproject"},
			key:  "Names", want: "devsys-myproject",
		},
		{
			name: "slice with one string",
			m:    map[string]interface{}{"Names": []interface{}{"devsys-myproject"}},
			key:  "Names", want: "devsys-myproject",
		},
		{
			name: "missing key",
			m:    map[string]interface{}{},
			key:  "Names", want: "",
		},
		{
			// Empty slice: len check fails, falls through to fmt.Sprintf("%v", v).
			name: "empty slice",
			m:    map[string]interface{}{"Names": []interface{}{}},
			key:  "Names", want: "[]",
		},
		{
			// Non-string first element: type assertion fails, falls through to Sprintf.
			name: "non-string slice element falls back to Sprintf",
			m:    map[string]interface{}{"Names": []interface{}{42}},
			key:  "Names", want: "[42]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containerField(tc.m, tc.key)
			if got != tc.want {
				t.Errorf("containerField(%v, %q) = %q; want %q", tc.m, tc.key, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// confirm
// ---------------------------------------------------------------------------

func TestConfirm(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"\n", false},      // blank = default No
		{"yes\n", false},   // partial match rejected
		{"no\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tc.input))
			got := confirm(reader, "test prompt")
			if got != tc.want {
				t.Errorf("confirm(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// getProjectIDFromGit
// ---------------------------------------------------------------------------

func newGitlabTestServer(t *testing.T, projectID int) *gitlab.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID})
	}))
	t.Cleanup(ts.Close)
	c := gitlab.NewClient(ts.URL, "test-token")
	c.DryRun = false
	return c
}

func writeGitConfig(t *testing.T, dir, content string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestGetProjectIDFromGit_OriginRemote(t *testing.T) {
	c := newGitlabTestServer(t, 55)
	dir := t.TempDir()
	writeGitConfig(t, dir, `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = https://gitlab.com/ns/myproject
	fetch = +refs/heads/*:refs/remotes/origin/*
`)
	id, err := getProjectIDFromGit(c, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 55 {
		t.Errorf("expected id 55, got %d", id)
	}
}

func TestGetProjectIDFromGit_SelfHostedOrigin(t *testing.T) {
	// devsys init always makes the GitLab remote "origin" (devsys CLI Spec,
	// Section 4), so getProjectIDFromGit trusts whatever origin's URL is,
	// self-hosted or not, without comparing it against a configured host.
	c := newGitlabTestServer(t, 88)
	dir := t.TempDir()
	writeGitConfig(t, dir, `[remote "origin"]
	url = https://git.mycompany.com/ns/myproject
`)
	id, err := getProjectIDFromGit(c, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 88 {
		t.Errorf("expected id 88, got %d", id)
	}
}

func TestGetProjectIDFromGit_NoOriginRemote(t *testing.T) {
	dir := t.TempDir()
	writeGitConfig(t, dir, `[remote "upstream"]
	url = https://gitlab.com/ns/myproject
`)
	_, err := getProjectIDFromGit(nil, dir)
	if err == nil {
		t.Error("expected error when no origin remote is found")
	}
}

func TestGetProjectIDFromGit_MissingConfigFile(t *testing.T) {
	_, err := getProjectIDFromGit(nil, t.TempDir()) // no .git/config
	if err == nil {
		t.Error("expected error for missing .git/config")
	}
}

// ---------------------------------------------------------------------------
// apiBaseURLFromRemote
// ---------------------------------------------------------------------------

func TestApiBaseURLFromRemote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https gitlab.com", "https://gitlab.com/ns/proj", "https://gitlab.com"},
		{"https self-hosted", "https://git.mycompany.com/ns/proj", "https://git.mycompany.com"},
		{"ssh gitlab.com", "git@gitlab.com:ns/proj.git", "https://gitlab.com"},
		{"ssh self-hosted", "git@git.mycompany.com:ns/proj.git", "https://git.mycompany.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := apiBaseURLFromRemote(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("apiBaseURLFromRemote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApiBaseURLFromRemote_Invalid(t *testing.T) {
	_, err := apiBaseURLFromRemote("git@:ns/proj.git")
	if err == nil {
		t.Error("expected error for a remote URL with no parseable host")
	}
}

// ---------------------------------------------------------------------------
// originRemoteURL
// ---------------------------------------------------------------------------

func TestOriginRemoteURL_IgnoresOtherRemotes(t *testing.T) {
	// A non-origin remote section appearing before origin must not be picked
	// up instead of origin's own url line.
	dir := t.TempDir()
	writeGitConfig(t, dir, `[remote "github"]
	url = https://github.com/ns/myproject
[remote "origin"]
	url = https://gitlab.com/ns/myproject
`)
	got, err := originRemoteURL(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://gitlab.com/ns/myproject" {
		t.Errorf("originRemoteURL() = %q; want the origin remote's URL", got)
	}
}

// ---------------------------------------------------------------------------
// gitlabClientForProject
// ---------------------------------------------------------------------------

func TestGitlabClientForProject_DerivesBaseURLFromOrigin(t *testing.T) {
	dir := t.TempDir()
	writeGitConfig(t, dir, `[remote "origin"]
	url = https://git.mycompany.com/ns/myproject
`)
	c, err := gitlabClientForProject(dir, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BaseURL != "https://git.mycompany.com" {
		t.Errorf("gitlabClientForProject BaseURL = %q; want %q", c.BaseURL, "https://git.mycompany.com")
	}
}

func TestGitlabClientForProject_NoOrigin_ReturnsError(t *testing.T) {
	_, err := gitlabClientForProject(t.TempDir(), "test-token") // no .git/config at all
	if err == nil {
		t.Error("expected error when project has no origin remote")
	}
}

// ---------------------------------------------------------------------------
// tokenExpiryDaysLeft — extracted logic from checkTokenExpiry
// ---------------------------------------------------------------------------

// tokenExpiryDaysLeft returns how many days until expiresAtStr (YYYY-MM-DD).
// Negative means already expired.
func tokenExpiryDaysLeft(expiresAtStr string) (int, bool) {
	t, err := time.Parse("2006-01-02", expiresAtStr)
	if err != nil {
		return 0, false
	}
	return int(time.Until(t).Hours() / 24), true
}

func TestTokenExpiryDaysLeft_Future(t *testing.T) {
	// A date far in the future should have many days left.
	future := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
	days, ok := tokenExpiryDaysLeft(future)
	if !ok {
		t.Fatal("parse failed")
	}
	if days < 300 {
		t.Errorf("expected >= 300 days, got %d", days)
	}
}

func TestTokenExpiryDaysLeft_Past(t *testing.T) {
	past := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")
	days, ok := tokenExpiryDaysLeft(past)
	if !ok {
		t.Fatal("parse failed")
	}
	if days > 0 {
		t.Errorf("expected negative days for past date, got %d", days)
	}
}

func TestTokenExpiryDaysLeft_InvalidDate(t *testing.T) {
	_, ok := tokenExpiryDaysLeft("not-a-date")
	if ok {
		t.Error("expected parse failure for invalid date")
	}
}

func TestTokenExpiryDaysLeft_SoonExpiry(t *testing.T) {
	// 10 days from now — should be within the 30-day warning window.
	soon := time.Now().AddDate(0, 0, 10).Format("2006-01-02")
	days, ok := tokenExpiryDaysLeft(soon)
	if !ok {
		t.Fatal("parse failed")
	}
	if days > 30 {
		t.Errorf("expected <= 30 days for soon-expiring token, got %d", days)
	}
}

// ---------------------------------------------------------------------------
// latestReleaseVersion
// ---------------------------------------------------------------------------

// withReleasesAPIURL points releasesAPIURL at a test server for the duration
// of the test, then restores the original value.
func withReleasesAPIURL(t *testing.T, url string) {
	t.Helper()
	orig := releasesAPIURL
	releasesAPIURL = url
	t.Cleanup(func() { releasesAPIURL = orig })
}

func TestLatestReleaseVersion_StripsLeadingV(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v1.2.3"})
	}))
	t.Cleanup(ts.Close)
	withReleasesAPIURL(t, ts.URL)

	got, err := latestReleaseVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1.2.3" {
		t.Errorf("latestReleaseVersion() = %q; want %q", got, "1.2.3")
	}
}

func TestLatestReleaseVersion_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	withReleasesAPIURL(t, ts.URL)

	_, err := latestReleaseVersion()
	if err == nil {
		t.Fatal("expected error on HTTP 404, got nil")
	}
}

func TestLatestReleaseVersion_EmptyTagName(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": ""})
	}))
	t.Cleanup(ts.Close)
	withReleasesAPIURL(t, ts.URL)

	_, err := latestReleaseVersion()
	if err == nil {
		t.Fatal("expected error for empty tag_name, got nil")
	}
}

func TestLatestReleaseVersion_MalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	t.Cleanup(ts.Close)
	withReleasesAPIURL(t, ts.URL)

	_, err := latestReleaseVersion()
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// ---------------------------------------------------------------------------
// updateCLI — dev-build short-circuit
// ---------------------------------------------------------------------------

func TestUpdateCLI_DevBuild_SkipsNetworkCall(t *testing.T) {
	origVersion := currentVersion
	currentVersion = "dev"
	t.Cleanup(func() { currentVersion = origVersion })

	// Point at a URL that would fail if actually called, to prove the dev-build
	// short-circuit returns before any network request is made.
	withReleasesAPIURL(t, "http://127.0.0.1:0/unreachable")

	if err := updateCLI(); err != nil {
		t.Fatalf("updateCLI() in dev mode: %v", err)
	}
}

func TestUpdateCLI_AlreadyUpToDate_NoSelfReplace(t *testing.T) {
	origVersion := currentVersion
	currentVersion = "1.2.3"
	t.Cleanup(func() { currentVersion = origVersion })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v1.2.3"})
	}))
	t.Cleanup(ts.Close)
	withReleasesAPIURL(t, ts.URL)

	// If updateCLI tried to self-replace here, it would fail (there is no
	// real running-binary path to swap in tests) — success alone proves the
	// already-up-to-date path was taken instead.
	if err := updateCLI(); err != nil {
		t.Fatalf("updateCLI(): %v", err)
	}
}

// ---------------------------------------------------------------------------
// releaseAssetURL
// ---------------------------------------------------------------------------

func TestReleaseAssetURL_MatchesCurrentPlatform(t *testing.T) {
	orig := releaseDownloadBaseURL
	releaseDownloadBaseURL = "https://example.test/download"
	t.Cleanup(func() { releaseDownloadBaseURL = orig })

	url, err := releaseAssetURL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(url, "https://example.test/download/devsys_") {
		t.Errorf("releaseAssetURL() = %q; want it to start with the configured base URL", url)
	}
}

// ---------------------------------------------------------------------------
// replaceBinary — actual file-replace mechanics, against a temp dir
// ---------------------------------------------------------------------------

func TestReplaceBinary_SwapsInNewContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("new binary content"))
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	destPath := dir + "/devsys-fake-binary"
	if err := os.WriteFile(destPath, []byte("old binary content"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := replaceBinary(ts.URL, destPath); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new binary content" {
		t.Errorf("destPath content = %q; want %q", got, "new binary content")
	}
	if _, err := os.Stat(destPath + ".old"); !os.IsNotExist(err) {
		t.Errorf("expected .old to be cleaned up, stat err = %v", err)
	}
	if _, err := os.Stat(destPath + ".new"); !os.IsNotExist(err) {
		t.Errorf("expected .new to be renamed away, stat err = %v", err)
	}
}

func TestReplaceBinary_DownloadFails_LeavesOriginalInPlace(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	destPath := dir + "/devsys-fake-binary"
	if err := os.WriteFile(destPath, []byte("old binary content"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := replaceBinary(ts.URL, destPath); err == nil {
		t.Fatal("expected error when download fails")
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "old binary content" {
		t.Errorf("original binary was modified despite download failure: %q", got)
	}
}

// ---------------------------------------------------------------------------
// updateProjectBaseImage
// ---------------------------------------------------------------------------

func TestUpdateProjectBaseImage_RewritesFromLineAndRebuilds(t *testing.T) {
	dir := t.TempDir()
	devsysDir := dir + "/.devsys"
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	containerfilePath := devsysDir + "/Containerfile"
	if err := os.WriteFile(containerfilePath, []byte("FROM example.test/ns/devsys-base:1.0.0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	registryTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tags": []string{"1.0.0", "2.0.0"}})
	}))
	t.Cleanup(registryTS.Close)
	origClient := registry.HTTPClient
	registry.HTTPClient = registryTS.Client()
	t.Cleanup(func() { registry.HTTPClient = origClient })
	registryHost := registryTS.URL[len("https://"):]

	// Point the Containerfile's FROM line at the fake registry's own host so
	// updateProjectBaseImage's lookup hits the test server, not example.test.
	if err := os.WriteFile(containerfilePath, []byte(fmt.Sprintf("FROM %s/ns/devsys-base:1.0.0\n", registryHost)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		VolumeExists:    true,
		ProjectPath:     dir,
	})

	if err := updateProjectBaseImage("testproject"); err != nil {
		t.Fatalf("updateProjectBaseImage: %v", err)
	}

	got, err := containerfileFromLine(containerfilePath)
	if err != nil {
		t.Fatalf("containerfileFromLine: %v", err)
	}
	want := registryHost + "/ns/devsys-base:2.0.0"
	if got != want {
		t.Errorf("Containerfile FROM line = %q; want %q", got, want)
	}
	if !rec.HasCall("build", "devsys-testproject") {
		t.Error("expected updateProjectBaseImage to rebuild the project")
	}
}
