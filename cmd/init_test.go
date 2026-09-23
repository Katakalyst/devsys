package cmd

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
	"github.com/katakalyst/devsys/internal/registry"
)

// ---------------------------------------------------------------------------
// createLocalRepo — purely local `git init`, no credentials (Git Remote &
// Credential Spec §7).
// ---------------------------------------------------------------------------

func TestCreateLocalRepo_BlankPathMeansRoot(t *testing.T) {
	workspaceRoot := t.TempDir()
	reader := bufio.NewReader(strings.NewReader("\n"))

	if err := createLocalRepo(reader, workspaceRoot); err != nil {
		t.Fatalf("createLocalRepo: unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".git")); err != nil {
		t.Errorf("expected .git at workspace root, stat error: %v", err)
	}
}

func TestCreateLocalRepo_SubPath(t *testing.T) {
	workspaceRoot := t.TempDir()
	reader := bufio.NewReader(strings.NewReader("frontend\n"))

	if err := createLocalRepo(reader, workspaceRoot); err != nil {
		t.Fatalf("createLocalRepo: unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "frontend", ".git")); err != nil {
		t.Errorf("expected .git at frontend/, stat error: %v", err)
	}
}

func TestCreateLocalRepo_AlreadyExists(t *testing.T) {
	workspaceRoot := t.TempDir()
	reader := bufio.NewReader(strings.NewReader("\n"))
	if err := createLocalRepo(reader, workspaceRoot); err != nil {
		t.Fatalf("first createLocalRepo: unexpected error: %v", err)
	}

	reader2 := bufio.NewReader(strings.NewReader("\n"))
	if err := createLocalRepo(reader2, workspaceRoot); err == nil {
		t.Error("expected error creating a repo where one already exists, got nil")
	}
}

// TestCreateLocalRepo_RejectsEscapingPath covers documents/TODO.md's fixed
// path-escape item: entering ".." (or a path that cleans to one) must not
// initialize a repo outside the workspace, despite the prompt's promise that
// the path is within it.
func TestCreateLocalRepo_RejectsEscapingPath(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	reader := bufio.NewReader(strings.NewReader("..\n"))
	if err := createLocalRepo(reader, workspaceRoot); err == nil {
		t.Error("expected error for \"..\", got nil")
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		t.Error("must not have created a repo outside the workspace")
	}
}

func TestResolveWorkspacePath(t *testing.T) {
	root := t.TempDir()
	workspaceRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	valid := []string{
		".",
		"frontend",
		filepath.Join("libs", "shared"),
		filepath.Join("a", "..", "b"), // cleans to "b", stays within
	}
	for _, relPath := range valid {
		got, err := resolveWorkspacePath(workspaceRoot, relPath)
		if err != nil {
			t.Errorf("resolveWorkspacePath(%q): unexpected error: %v", relPath, err)
			continue
		}
		if !strings.HasPrefix(got, workspaceRoot) {
			t.Errorf("resolveWorkspacePath(%q) = %q, want a path under %q", relPath, got, workspaceRoot)
		}
	}

	escaping := []string{
		"..",
		filepath.Join("..", "sibling"),
		filepath.Join("a", "..", ".."),
		filepath.Join("a", "..", "..", "b"),
	}
	for _, relPath := range escaping {
		if _, err := resolveWorkspacePath(workspaceRoot, relPath); err == nil {
			t.Errorf("resolveWorkspacePath(%q): expected error (escapes workspace), got nil", relPath)
		}
	}

	otherAbs := "/etc"
	if runtime.GOOS == "windows" {
		otherAbs = filepath.VolumeName(workspaceRoot) + `\etc`
	}
	absolute := []string{
		filepath.Join(root, "elsewhere"), // absolute, even though it happens to be under root
		otherAbs,
	}
	for _, relPath := range absolute {
		if _, err := resolveWorkspacePath(workspaceRoot, relPath); err == nil {
			t.Errorf("resolveWorkspacePath(%q): expected error (absolute path rejected), got nil", relPath)
		}
	}
}

// ---------------------------------------------------------------------------
// runInitRepoSetup — the discovery + repeatable "create a new repo here"
// loop (Spec §7/§8/§9), reusing auth's own listing/configure logic.
// ---------------------------------------------------------------------------

func TestRunInitRepoSetup_ImmediateDone_NoRepos(t *testing.T) {
	workspaceRoot := t.TempDir()
	reader := bufio.NewReader(strings.NewReader("d\n"))

	changed, err := runInitRepoSetup(reader, "testproject", workspaceRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Error("expected changed=false when nothing was configured")
	}
}

func TestRunInitRepoSetup_CreatesRepoThenDone(t *testing.T) {
	workspaceRoot := t.TempDir()
	// "n" -> create a repo -> blank path (root) -> back to listing -> "d" done.
	reader := bufio.NewReader(strings.NewReader("n\n\nd\n"))

	changed, err := runInitRepoSetup(reader, "testproject", workspaceRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Creating a *local* repo alone (no remote/credential configured) must
	// not count as "changed" — that flag exists purely to tell a re-run of
	// init whether to point the user at `devsys auth` for un-mounted
	// credentials (Git Remote & Credential Spec §7's creation-time-only
	// secret attachment), and a bare local git init has no credential at all.
	if changed {
		t.Error("expected changed=false — only a local repo was created, no credential configured")
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".git")); err != nil {
		t.Errorf("expected .git at workspace root after 'n' + blank path, stat error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runInit — full happy path, no repos created, verifying the Containerfile/
// build/create steps that follow the repo-setup loop.
// ---------------------------------------------------------------------------

func TestRunInit_EmptyProject_BuildsAndCreatesContainer(t *testing.T) {
	workspaceRoot := t.TempDir()

	registryTS := httptest.NewTLSServer(fakeRegistryTagsHandler())
	t.Cleanup(registryTS.Close)
	origClient := registry.HTTPClient
	registry.HTTPClient = registryTS.Client()
	t.Cleanup(func() { registry.HTTPClient = origClient })
	registryHost := registryTS.URL[len("https://"):]

	origBaseImage := devsysBaseImage
	devsysBaseImage = registryHost + "/ns/devsys-base"
	t.Cleanup(func() { devsysBaseImage = origBaseImage })

	rec := podmanfake.Install(t, podmanfake.Options{ContainerExists: false})

	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	go func() {
		w.WriteString("d\n") // no repos to set up
		w.Close()
	}()

	if err := runInit(initCmd, []string{workspaceRoot, "testproject"}); err != nil {
		t.Fatalf("runInit: unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspaceRoot, ".devsys", "Containerfile")); err != nil {
		t.Errorf("expected .devsys/Containerfile to be generated, stat error: %v", err)
	}
	if !rec.HasCall("build", "devsys-testproject") {
		t.Error("expected podman build for devsys-testproject")
	}
	if !rec.HasCall("create", "--name", "devsys-testproject") {
		t.Error("expected podman create for devsys-testproject")
	}
}

// fakeRegistryTagsHandler serves a minimal OCI tags/list response so
// registry.LatestReference resolves to a usable "host/ns/devsys-base:1.0.0"
// reference without touching the real registry.
func fakeRegistryTagsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tags":["1.0.0"]}`))
	}
}
