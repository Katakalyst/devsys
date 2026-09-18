package podman_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/podmanfake"
)

// TestFakePodman is the TestHelperProcess entry point. When this test binary is
// re-invoked as a fake podman subprocess, Handle detects the env var and exits.
// In normal test runs the call is a no-op and the test passes immediately.
func TestFakePodman(t *testing.T) { podmanfake.Handle() }

// captureDryRunOutput redirects DryRunOutput to a buffer for the duration of
// the test, then restores the original writer.
func captureDryRunOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	original := podman.DryRunOutput
	podman.DryRunOutput = buf
	t.Cleanup(func() { podman.DryRunOutput = original })
	return buf
}

// setDryRun enables/disables DryRun for the duration of a test.
func setDryRun(t *testing.T, v bool) {
	t.Helper()
	original := podman.DryRun
	podman.DryRun = v
	t.Cleanup(func() { podman.DryRun = original })
}

// ---------------------------------------------------------------------------
// DryRun — mutating commands
// ---------------------------------------------------------------------------

func TestRunPodman_DryRun_MutatingCommandsPrint(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	out, err := podman.RunPodman("create", "--name", "devsys-test", "devsys-test-image")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output in dry-run, got %q", out)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] prefix in output, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "create") {
		t.Errorf("expected command name in output, got: %q", buf.String())
	}
}

func TestRunPodman_DryRun_Build(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	_, err := podman.RunPodman("build", "-t", "mytag", "-f", "Containerfile", ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "build") {
		t.Errorf("expected build command in output, got: %q", buf.String())
	}
}

func TestRunPodman_DryRun_SecretCreate(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	_, err := podman.RunPodman("secret", "create", "--label", "devsys=true", "my-secret", "-")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] output for secret create, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// DryRun — read-only commands are NOT skipped
// (We can't run real podman in unit tests, but we can verify they don't print
// a [dry-run] prefix — they fail with a real exec error instead.)
// ---------------------------------------------------------------------------

func TestRunPodman_DryRun_ReadOnlyCommandsNotSkipped(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	// podman --version is read-only; in dry-run it should actually run (and
	// fail or succeed depending on whether podman is installed), NOT print
	// a [dry-run] prefix.
	podman.RunPodman("--version") //nolint:errcheck — we don't care if podman is absent

	if strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("read-only command should not print [dry-run] prefix, got: %q", buf.String())
	}
}

func TestRunPodman_DryRun_InspectNotSkipped(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	podman.RunPodman("inspect", "some-container") //nolint:errcheck

	if strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("inspect should not be a dry-run no-op, got: %q", buf.String())
	}
}

func TestRunPodman_DryRun_VolumeInspectNotSkipped(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	podman.RunPodman("volume", "inspect", "some-volume") //nolint:errcheck

	if strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("volume inspect should not be a dry-run no-op, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// DryRun off — mutating commands attempt real execution
// (We verify they return an error when podman isn't available, NOT a no-op.)
// ---------------------------------------------------------------------------

func TestRunPodman_NoDryRun_MutatingCommandsExecute(t *testing.T) {
	setDryRun(t, false)
	buf := captureDryRunOutput(t)

	// This will fail unless podman is installed — the important thing is that
	// it does NOT silently no-op and does NOT print [dry-run].
	podman.RunPodman("create", "--name", "nonexistent-test-container", "nonexistent-image") //nolint:errcheck

	if strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("with DryRun=false, must not print [dry-run] prefix")
	}
}

// ---------------------------------------------------------------------------
// CreateSecretFromStdin — DryRun
// ---------------------------------------------------------------------------

func TestCreateSecretFromStdin_DryRun(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	labels := map[string]string{"devsys": "true", "devsys.expires-at": "2026-12-31"}
	err := podman.CreateSecretFromStdin("test-secret", "super-secret-value", labels)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "[dry-run]") {
		t.Errorf("expected [dry-run] prefix, got: %q", output)
	}
	// Secret value must never appear in dry-run output.
	if strings.Contains(output, "super-secret-value") {
		t.Errorf("secret value must not appear in dry-run output: %q", output)
	}
}

// ---------------------------------------------------------------------------
// ExecInteractive — DryRun
// ---------------------------------------------------------------------------

func TestExecInteractive_DryRun(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	err := podman.ExecInteractive("devsys-myproject", "claude")
	if err != nil {
		t.Fatalf("unexpected error in dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] output, got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "claude") {
		t.Errorf("expected command name in output, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// RunPodmanLive — DryRun
// ---------------------------------------------------------------------------

func TestRunPodmanLive_DryRun_MutatingCommandsPrint(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	err := podman.RunPodmanLive("pull", "ghcr.io/example/image:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] prefix, got: %q", buf.String())
	}
}

func TestRunPodmanLive_NoDryRun_ReadOnlyExecutes(t *testing.T) {
	setDryRun(t, false)
	buf := captureDryRunOutput(t)

	err := podman.RunPodmanLive("--version")
	if err != nil {
		t.Skipf("podman not available: %v", err)
	}
	if strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("must not print [dry-run] when DryRun is false")
	}
}

// ---------------------------------------------------------------------------
// DeleteSecret — DryRun
// ---------------------------------------------------------------------------

func TestDeleteSecret_DryRun(t *testing.T) {
	setDryRun(t, true)
	buf := captureDryRunOutput(t)

	err := podman.DeleteSecret("devsys-test-nonexistent-secret-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("expected [dry-run] output for DeleteSecret, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// State-checking helpers — require a real Podman installation.
// All resource names are chosen to be non-existent so nothing is created or
// modified.
// ---------------------------------------------------------------------------

func TestSecretExists_NotFound(t *testing.T) {
	if podman.SecretExists("devsys-test-nonexistent-secret-xyz-99999") {
		t.Error("expected false for non-existent secret")
	}
}

func TestGetSecretValue_NotFound(t *testing.T) {
	_, err := podman.GetSecretValue("devsys-test-nonexistent-secret-xyz-99999")
	if err == nil {
		t.Error("expected error for non-existent secret")
	}
}

func TestGetSecretLabels_NotFound(t *testing.T) {
	_, err := podman.GetSecretLabels("devsys-test-nonexistent-secret-xyz-99999")
	if err == nil {
		t.Error("expected error for non-existent secret")
	}
}

func TestContainerExists_NotFound(t *testing.T) {
	if podman.ContainerExists("devsys-test-nonexistent-container-xyz-99999") {
		t.Error("expected false for non-existent container")
	}
}

func TestContainerIsRunning_NotFound(t *testing.T) {
	if podman.ContainerIsRunning("devsys-test-nonexistent-container-xyz-99999") {
		t.Error("expected false for non-existent container")
	}
}

func TestVolumeExists_NotFound(t *testing.T) {
	if podman.VolumeExists("devsys-test-nonexistent-volume-xyz-99999") {
		t.Error("expected false for non-existent volume")
	}
}

func TestListDevsysContainers_NoError(t *testing.T) {
	// May return an empty list on a clean machine; must not error.
	_, err := podman.ListDevsysContainers()
	if err != nil {
		t.Errorf("unexpected error listing devsys containers: %v", err)
	}
}

func TestListDevsysVolumes_NoError(t *testing.T) {
	_, err := podman.ListDevsysVolumes()
	if err != nil {
		t.Errorf("unexpected error listing devsys volumes: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration tests — create real Podman resources and always clean up.
// All resource names contain "devsys-test" so they are visually distinct from
// production resources on the machine.
// ---------------------------------------------------------------------------

// createTestSecret creates a Podman secret for the duration of the test and
// removes it on cleanup. Returns false (with t.Skip) if creation fails.
func createTestSecret(t *testing.T, name, value string, labels map[string]string) bool {
	t.Helper()
	t.Cleanup(func() {
		podman.DeleteSecret(name) //nolint:errcheck — best-effort cleanup
	})
	if err := podman.CreateSecretFromStdin(name, value, labels); err != nil {
		t.Skipf("cannot create Podman secret (Podman may not support this operation): %v", err)
		return false
	}
	return true
}

func TestCreateSecretFromStdin_Real(t *testing.T) {
	setDryRun(t, false)
	name := "devsys-test-create-secret-coverage"
	if !createTestSecret(t, name, "test-secret-value", map[string]string{"devsys": "true"}) {
		return
	}
	if !podman.SecretExists(name) {
		t.Error("secret should exist immediately after creation")
	}
}

func TestGetSecretValue_Success(t *testing.T) {
	setDryRun(t, false)
	name := "devsys-test-getvalue-coverage"
	if !createTestSecret(t, name, "my-secret-value-xyz", nil) {
		return
	}
	val, err := podman.GetSecretValue(name)
	if err != nil {
		t.Fatalf("GetSecretValue error: %v", err)
	}
	if val != "my-secret-value-xyz" {
		t.Errorf("GetSecretValue: want %q, got %q", "my-secret-value-xyz", val)
	}
}

func TestGetSecretLabels_WithLabels(t *testing.T) {
	setDryRun(t, false)
	name := "devsys-test-getlabels-coverage"
	labels := map[string]string{
		"devsys":             "true",
		"devsys.expires-at": "2026-12-31",
	}
	if !createTestSecret(t, name, "val", labels) {
		return
	}
	got, err := podman.GetSecretLabels(name)
	if err != nil {
		t.Fatalf("GetSecretLabels error: %v", err)
	}
	if got["devsys"] != "true" {
		t.Errorf("expected label devsys=true, got: %v", got)
	}
	if got["devsys.expires-at"] != "2026-12-31" {
		t.Errorf("expected label devsys.expires-at=2026-12-31, got: %v", got)
	}
}

func TestGetSecretLabels_NoLabels_ReturnsEmptyMap(t *testing.T) {
	setDryRun(t, false)
	name := "devsys-test-nolabels-coverage"
	if !createTestSecret(t, name, "val", nil) {
		return
	}
	got, err := podman.GetSecretLabels(name)
	if err != nil {
		t.Fatalf("GetSecretLabels error: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil empty map for a secret with no labels")
	}
}

// createTestVolume creates a labelled Podman volume for the test duration.
func createTestVolume(t *testing.T, name string) bool {
	t.Helper()
	t.Cleanup(func() {
		podman.RunPodman("volume", "rm", name) //nolint:errcheck
	})
	if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", name); err != nil {
		t.Skipf("cannot create Podman volume: %v", err)
		return false
	}
	return true
}

func TestListDevsysVolumes_WithData(t *testing.T) {
	setDryRun(t, false)
	volName := "devsys-test-list-volume-coverage"
	if !createTestVolume(t, volName) {
		return
	}
	volumes, err := podman.ListDevsysVolumes()
	if err != nil {
		t.Fatalf("ListDevsysVolumes error: %v", err)
	}
	if len(volumes) == 0 {
		t.Error("expected at least one devsys-labelled volume after creating one")
	}
}
