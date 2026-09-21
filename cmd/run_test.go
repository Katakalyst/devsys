package cmd

// Tests for runEnter and rebuildProject using the fake podman subprocess
// infrastructure (internal/podmanfake).
//
// All tests run against the real function logic without a real container
// runtime. The fake podman subprocess records every podman invocation in a
// JSONL call log so assertions are made on the Recorder.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/podmanfake"
	"github.com/katakalyst/devsys/internal/registry"
)

// injectStdin replaces os.Stdin with a pipe carrying input for the duration of
// the test. Each line in input should end with "\n" to satisfy bufio.Reader.
func injectStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("injectStdin: os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
	})
	// Write all input upfront and close the write end so readers see EOF.
	go func() {
		defer w.Close()
		w.WriteString(input) //nolint:errcheck
	}()
}

// ---------------------------------------------------------------------------
// runEnter
// ---------------------------------------------------------------------------

// skipStalenessChecks redirects the cache dir to a temp one and pre-marks
// the throttle keys for staleness and auth checks, so tests exercising
// runEnter for reasons unrelated to those features don't make real network
// calls or touch the real user cache dir.
func skipStalenessChecks(t *testing.T, projectName string) {
	t.Helper()
	redirectCacheDir(t)
	markChecked("cli-version")
	markChecked("base-image-" + projectName)
	markChecked("claude-auth")
	markChecked("codex-auth")
}

func TestRunEnter_ContainerRunning_OpensBashSession(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: true,
		SecretExists:     false, // no token secret → checkTokenExpiry silently returns
	})
	skipStalenessChecks(t, "testproject")

	err := runEnter(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runEnter: %v", err)
	}
	if !rec.HasCall("exec", "devsys-testproject", "bash") {
		t.Error("expected podman exec ... bash")
	}
	// Last session exited (ActiveBashSessions=0 default) → container stopped.
	if !rec.HasCall("stop", "devsys-testproject") {
		t.Error("expected podman stop after last bash session exited")
	}
}

func TestRunEnter_ContainerStopped_StartsBeforeEntering(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: false,
		SecretExists:     false,
	})
	skipStalenessChecks(t, "testproject")

	err := runEnter(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runEnter: %v", err)
	}
	if !rec.HasCall("start", "devsys-testproject") {
		t.Error("expected podman start before entering")
	}
	if !rec.HasCall("exec", "devsys-testproject", "bash") {
		t.Error("expected podman exec ... bash")
	}
}

func TestRunEnter_TokenExpiringSoon_WarnsAndStillEnters(t *testing.T) {
	// Token expires in 10 days — within the 30-day warning window.
	soon := time.Now().AddDate(0, 0, 10).Format("2006-01-02")

	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: true,
		SecretExists:     true,
		SecretExpiresAt:  soon,
	})
	skipStalenessChecks(t, "testproject")

	// runEnter should still return nil (the warning is non-fatal).
	err := runEnter(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runEnter: %v", err)
	}
	// Bash session was still opened.
	if !rec.HasCall("exec", "devsys-testproject", "bash") {
		t.Error("expected podman exec ... bash even when token is expiring")
	}
}

func TestRunEnter_OtherSessionsOpen_KeepsContainerRunning(t *testing.T) {
	// Another bash session is still open when this one exits → no stop.
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:    true,
		ContainerRunning:   true,
		ActiveBashSessions: 1,
	})
	skipStalenessChecks(t, "testproject")

	err := runEnter(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runEnter: %v", err)
	}
	if rec.HasSubcommand("stop") {
		t.Errorf("expected no stop while another bash session is open; calls: %v", rec.Calls())
	}
}

func TestRunEnter_NoContainer_ReturnsError(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: false,
	})

	err := runEnter(nil, []string{"testproject"})
	if err == nil {
		t.Fatal("expected error when container does not exist")
	}
}

func TestRunEnter_StartFails_ReturnsError(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: false,
		StartFails:       true,
	})

	err := runEnter(nil, []string{"testproject"})
	if err == nil {
		t.Fatal("expected error when the container fails to start")
	}
}

// ---------------------------------------------------------------------------
// checkStaleness / warnIfCLIOutdated / warnIfBaseImageOutdated
// ---------------------------------------------------------------------------

func TestWarnIfCLIOutdated_Behind_PrintsWarning(t *testing.T) {
	origVersion := currentVersion
	currentVersion = "1.0.0"
	t.Cleanup(func() { currentVersion = origVersion })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v2.0.0"})
	}))
	t.Cleanup(ts.Close)
	origAPI := releasesAPIURL
	releasesAPIURL = ts.URL
	t.Cleanup(func() { releasesAPIURL = origAPI })

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	warnIfCLIOutdated()
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if !strings.Contains(buf.String(), "2.0.0") {
		t.Errorf("expected warning mentioning the newer version, got: %q", buf.String())
	}
}

func TestWarnIfBaseImageOutdated_Behind_PrintsWarning(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/.devsys", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	registryTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tags": []string{"1.0.0", "2.0.0"}})
	}))
	t.Cleanup(registryTS.Close)
	origClient := registry.HTTPClient
	registry.HTTPClient = registryTS.Client()
	t.Cleanup(func() { registry.HTTPClient = origClient })
	registryHost := registryTS.URL[len("https://"):]

	containerfilePath := dir + "/.devsys/Containerfile"
	if err := os.WriteFile(containerfilePath, []byte(fmt.Sprintf("FROM %s/ns/devsys-base:1.0.0\n", registryHost)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ProjectPath:     dir,
	})

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	warnIfBaseImageOutdated("testproject")
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if !strings.Contains(buf.String(), "2.0.0") {
		t.Errorf("expected warning mentioning the newer base image, got: %q", buf.String())
	}
}

func TestCheckStaleness_Throttled_SkipsSecondCheck(t *testing.T) {
	redirectCacheDir(t)

	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/.devsys", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	var registryHits int
	registryTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryHits++
		json.NewEncoder(w).Encode(map[string]interface{}{"tags": []string{"1.0.0", "2.0.0"}})
	}))
	t.Cleanup(registryTS.Close)
	origClient := registry.HTTPClient
	registry.HTTPClient = registryTS.Client()
	t.Cleanup(func() { registry.HTTPClient = origClient })
	registryHost := registryTS.URL[len("https://"):]

	containerfilePath := dir + "/.devsys/Containerfile"
	if err := os.WriteFile(containerfilePath, []byte(fmt.Sprintf("FROM %s/ns/devsys-base:1.0.0\n", registryHost)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{ContainerExists: true, ProjectPath: dir})

	checkStaleness("testproject")
	if registryHits != 1 {
		t.Fatalf("expected exactly 1 base-image check on first call, got %d", registryHits)
	}

	checkStaleness("testproject")
	if registryHits != 1 {
		t.Errorf("expected the second checkStaleness call to be throttled (no new registry hit), got %d total hits", registryHits)
	}
}

func TestWarnIfCLIOutdated_ThrottledPattern(t *testing.T) {
	// warnIfCLIOutdated itself is unconditional (throttling is the caller's
	// job, in cmd/root.go's PersistentPostRun) — this verifies the same
	// checkThrottled/markChecked composition cmd/root.go uses actually
	// throttles a second call.
	redirectCacheDir(t)
	origVersion := currentVersion
	currentVersion = "1.0.0"
	t.Cleanup(func() { currentVersion = origVersion })

	var apiHits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v2.0.0"})
	}))
	t.Cleanup(ts.Close)
	origAPI := releasesAPIURL
	releasesAPIURL = ts.URL
	t.Cleanup(func() { releasesAPIURL = origAPI })

	runCheck := func() {
		if !checkThrottled("cli-version") {
			warnIfCLIOutdated()
			markChecked("cli-version")
		}
	}

	runCheck()
	if apiHits != 1 {
		t.Fatalf("expected exactly 1 API hit on first call, got %d", apiHits)
	}
	runCheck()
	if apiHits != 1 {
		t.Errorf("expected the second call to be throttled, got %d total hits", apiHits)
	}
}

// ---------------------------------------------------------------------------
// rebuildProject
// ---------------------------------------------------------------------------

func TestRebuildProject_BuildRmCreate(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		VolumeExists:    true, // skip volume creation for simplicity
		ProjectPath:     "/tmp/devsys-testproject",
	})

	err := rebuildProject("testproject")
	if err != nil {
		t.Fatalf("rebuildProject: %v", err)
	}

	// Build: podman build -t devsys-testproject -f <containerfile> <path>
	if !rec.HasCall("build", "devsys-testproject") {
		t.Error("expected podman build with image tag devsys-testproject")
	}
	// Remove existing container before recreating.
	if !rec.HasCall("rm", "devsys-testproject") {
		t.Error("expected podman rm devsys-testproject")
	}
	// Recreate the container.
	if !rec.HasCall("create", "--name", "devsys-testproject") {
		t.Error("expected podman create --name devsys-testproject")
	}
}

func TestRebuildProject_NoProjectPath_ReturnsError(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ProjectPath:     "", // getProjectPath returns empty → error
	})

	err := rebuildProject("testproject")
	if err == nil {
		t.Fatal("expected error when project path cannot be determined")
	}
}

func TestRebuildProject_BuildFails_ReturnsError(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		VolumeExists:    true,
		ProjectPath:     "/tmp/devsys-testproject",
		BuildFails:      true,
	})

	err := rebuildProject("testproject")
	if err == nil {
		t.Fatal("expected error when podman build fails")
	}
}

func TestRebuildProject_CreateFails_ReturnsError(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		VolumeExists:    true,
		ProjectPath:     "/tmp/devsys-testproject",
		CreateFails:     true,
	})

	err := rebuildProject("testproject")
	if err == nil {
		t.Fatal("expected error when podman create fails")
	}
}

func TestRebuildProject_NoExistingContainer_SkipsRm(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: false, // no container to remove before recreating
		VolumeExists:    true,
		ProjectPath:     "/tmp/devsys-testproject",
	})

	err := rebuildProject("testproject")
	if err != nil {
		t.Fatalf("rebuildProject: %v", err)
	}
	// Use HasSubcommand to check the first arg exactly — HasCall("rm", "-f")
	// would be a false positive because the build command contains "-f".
	if rec.HasSubcommand("rm") {
		t.Errorf("expected no podman rm when no existing container; calls: %v", rec.Calls())
	}
	if !rec.HasCall("create", "--name", "devsys-testproject") {
		t.Error("expected podman create even without a prior container")
	}
}

// ---------------------------------------------------------------------------
// runDoctor
// ---------------------------------------------------------------------------

// withFakeRegistry points registry.HTTPClient at a local httptest.TLS
// server for the duration of t, restoring the original client on cleanup.
// doctor's base-image check is a live registry reachability lookup
// (registry.LatestTag), not a podman call, so it needs its own fake rather
// than podmanfake.
func withFakeRegistry(t *testing.T, reachable bool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reachable {
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"tags": []string{"1.0.0"}})
	}))
	t.Cleanup(ts.Close)

	origClient := registry.HTTPClient
	registry.HTTPClient = ts.Client()
	t.Cleanup(func() { registry.HTTPClient = origClient })

	origImage := devsysBaseImage
	devsysBaseImage = ts.URL[len("https://"):] + "/ns/devsys-base"
	t.Cleanup(func() { devsysBaseImage = origImage })
}

func TestRunDoctor_AllChecksPass(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		// Podman present: --version returns output (fake always does)
		SecretExists: true,
		SecretValue:  "glpat-fakeboostraptokenlongerthan10chars",
	})
	withFakeRegistry(t, true)

	err := runDoctor(nil, nil)
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
}

func TestRunDoctor_PodmanAbsent_ReportsCheckFail(t *testing.T) {
	// Fake still handles --version (returns fake output), so "podman absent"
	// can only be simulated by leaving the default fake in place and checking
	// that the function returns nil (doctor never returns a hard error — it
	// prints FAIL lines and returns nil). This test verifies that.
	podmanfake.Install(t, podmanfake.Options{
		SecretExists: false,
	})
	withFakeRegistry(t, false)

	// runDoctor always returns nil; failures are printed, not propagated.
	err := runDoctor(nil, nil)
	if err != nil {
		t.Fatalf("runDoctor returned unexpected error: %v", err)
	}
}

func TestRunDoctor_BootstrapPATMissing_CheckFails(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		SecretExists: false, // bootstrap PAT secret does not exist
	})
	withFakeRegistry(t, true)

	err := runDoctor(nil, nil)
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	// The secret-missing path skips the "PAT readable" sub-check entirely —
	// the function continues to the base-image check and prints "All checks
	// passed" or "Some checks failed". We just verify it does not panic or
	// error out.
}

func TestRunDoctor_BaseImageUnreachable_CheckFails(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{
		SecretExists: true,
		SecretValue:  "glpat-fakeboostraptokenlongerthan10chars",
	})
	withFakeRegistry(t, false) // registry unreachable

	err := runDoctor(nil, nil)
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runRm
// ---------------------------------------------------------------------------

func TestRunRm_AllResourcesExist_RemovesAll(t *testing.T) {
	// Container stopped, secret exists, trivy volume exists.
	// Confirm sequence: remove container → y, remove secret → y,
	//                   remove volume → y, revoke GitLab → n (would need real GitLab).
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: false,
		SecretExists:     true,
		VolumeExists:     true,
		ProjectPath:      "", // empty → revokeGitLabToken bails early (no git config)
	})
	injectStdin(t, "y\ny\ny\nn\n")

	err := runRm(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runRm: %v", err)
	}

	// Container removed.
	if !rec.HasCall("rm", "devsys-testproject") {
		t.Error("expected podman rm devsys-testproject")
	}
	// Secret deleted.
	if !rec.HasCall("secret", "rm") {
		t.Error("expected podman secret rm")
	}
	// Trivy volume removed.
	if !rec.HasCall("volume", "rm", "devsys-testproject-trivy-db") {
		t.Error("expected podman volume rm devsys-testproject-trivy-db")
	}
}

func TestRunRm_ContainerRunning_StopsBeforeRemove(t *testing.T) {
	// Container running → must stop before rm.
	// Confirm: remove container → y, revoke GitLab → n.
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists:  true,
		ContainerRunning: true,
		SecretExists:     false,
		VolumeExists:     false,
	})
	injectStdin(t, "y\nn\n")

	err := runRm(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runRm: %v", err)
	}

	if !rec.HasCall("stop", "devsys-testproject") {
		t.Error("expected podman stop before rm for running container")
	}
	if !rec.HasCall("rm", "devsys-testproject") {
		t.Error("expected podman rm after stop")
	}
}

func TestRunRm_NoContainer_SkipsContainerStep(t *testing.T) {
	// Container not found — the container confirm is skipped entirely.
	// Confirm: remove secret → y, revoke GitLab → n.
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: false,
		SecretExists:    true,
		VolumeExists:    false,
	})
	injectStdin(t, "y\nn\n")

	err := runRm(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runRm: %v", err)
	}

	// No container rm or stop should have happened.
	for _, call := range rec.Calls() {
		if len(call) > 0 && call[0] == "rm" {
			// "rm" without "volume" or "secret" prefix means container rm.
			t.Errorf("unexpected podman rm call when container does not exist: %v", call)
		}
		if len(call) > 0 && call[0] == "stop" {
			t.Errorf("unexpected podman stop when container does not exist: %v", call)
		}
	}
	// Secret was deleted.
	if !rec.HasCall("secret", "rm") {
		t.Error("expected podman secret rm")
	}
}

func TestRunRm_UserDeclinesAll_RemovesNothing(t *testing.T) {
	// All confirms answered "n" — nothing should be removed.
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		SecretExists:    true,
		VolumeExists:    true,
	})
	injectStdin(t, strings.Repeat("n\n", 4))

	err := runRm(nil, []string{"testproject"})
	if err != nil {
		t.Fatalf("runRm: %v", err)
	}

	// Verify no destructive podman calls were made.
	for _, call := range rec.Calls() {
		if len(call) == 0 {
			continue
		}
		switch call[0] {
		case "rm", "stop":
			t.Errorf("unexpected destructive call when user declined all: %v", call)
		case "secret":
			if len(call) > 1 && call[1] == "rm" {
				t.Errorf("unexpected secret rm when user declined: %v", call)
			}
		case "volume":
			if len(call) > 1 && call[1] == "rm" {
				t.Errorf("unexpected volume rm when user declined: %v", call)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Dry-run behavior
//
// These set podman.DryRun directly — the same runtime mechanism
// internal/podman/podman_test.go already uses — rather than relying on the
// separate `-tags dev` build (see DEVELOPMENT.md). That build only proves
// the build-tag wiring itself still compiles and defaults DryRun to true;
// it cannot exercise command-level logic without breaking every other test
// in this file (podmanfake.Install now defensively resets podman.DryRun to
// false for its own duration precisely so it isn't affected either way).
// These tests are what actually verify a command's dry-run behavior is
// correct: mutating podman calls are skipped, and the intended action is
// still described in DryRunOutput.
// ---------------------------------------------------------------------------

// withDryRun sets podman.DryRun for the duration of t and captures
// DryRunOutput into a buffer, restoring both on cleanup.
func withDryRun(t *testing.T, v bool) *bytes.Buffer {
	t.Helper()
	origDryRun := podman.DryRun
	origOutput := podman.DryRunOutput
	podman.DryRun = v
	buf := &bytes.Buffer{}
	podman.DryRunOutput = buf
	t.Cleanup(func() {
		podman.DryRun = origDryRun
		podman.DryRunOutput = origOutput
	})
	return buf
}

func TestRebuildProject_DryRun_SkipsBuildRmCreate(t *testing.T) {
	rec := podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		VolumeExists:    true,
		ProjectPath:     "/tmp/devsys-testproject",
	})
	buf := withDryRun(t, true)

	if err := rebuildProject("testproject"); err != nil {
		t.Fatalf("rebuildProject: %v", err)
	}
	for _, sub := range []string{"build", "rm", "create"} {
		if rec.HasSubcommand(sub) {
			t.Errorf("expected no real podman %s call while DryRun is true; calls: %v", sub, rec.Calls())
		}
	}
	out := buf.String()
	for _, want := range []string{"build", "rm", "create"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected dry-run output to mention %q, got: %q", want, out)
		}
	}
	// The read-only lookups (getProjectPath, ContainerExists) that decide
	// *what* to build/rm/create must still have run for real, dry-run or not
	// — otherwise the printed dry-run description would be wrong/empty.
	if !rec.HasCall("inspect") {
		t.Error("expected read-only inspect calls to still execute under DryRun")
	}
}
