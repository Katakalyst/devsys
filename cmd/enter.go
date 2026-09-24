package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
)

var enterCmd = &cobra.Command{
	Use:   "enter <project>",
	Short: "Open a shell inside a project container",
	Args:  cobra.ExactArgs(1),
	RunE:  runEnter,
}

func runEnter(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}

	// Block if the project's own .devsys/Containerfile has been edited since
	// the image was built — the running container would be inconsistent with
	// the declared stack.
	if err := checkContainerfileStale(projectName); err != nil {
		return err
	}

	// Block if .devsys/ports has changed since the image was built — port
	// mappings are baked into the container at creation time and cannot be
	// changed without a rebuild.
	if err := checkPortsStale(projectName); err != nil {
		return err
	}

	// Ensure container is running.
	if !podman.ContainerIsRunning(containerName) {
		if _, err := podman.RunPodman("start", containerName); err != nil {
			return fmt.Errorf("cannot start container: %w", err)
		}
	}

	// Check token expiry, and (throttled, non-blocking) whether this
	// project's base image is behind current (devsys CLI Spec, Section
	// 12.5). The devsys-itself check runs globally for every command
	// (cmd/root.go's PersistentPostRun), not specifically here.
	checkTokenExpiry(projectName)
	checkStaleness(projectName)

	// Open a bash shell interactively.
	shellErr := podman.ExecInteractive(containerName, "bash")

	// After the shell exits, stop the container if no bash sessions remain.
	// This keeps the container running only while someone is inside it.
	// (The container's watchdog process handles the crash/kill-9 case
	// independently; this is the normal-exit path.)
	if podman.ContainerIsRunning(containerName) && activeBashSessions(containerName) == 0 {
		if _, err := podman.RunPodman("stop", containerName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot stop container: %v\n", err)
		}
	}
	return shellErr
}

// activeBashSessions returns the number of bash processes currently running
// inside the container. Returns 0 on any error (treated as no sessions).
func activeBashSessions(containerName string) int {
	out, err := podman.RunPodman("top", containerName, "comm")
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// First line is the "COMMAND" header — skip it.
	count := 0
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "bash" {
			count++
		}
	}
	return count
}

// checkContainerfileStale returns an error if the project's .devsys/Containerfile
// has changed since the image was last built. It compares the file's current
// SHA256 hash against the devsys.containerfile-hash label baked into the image
// at build time. Gracefully skips (returns nil) when the image has no label —
// that means it was built before this feature was introduced.
func checkContainerfileStale(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}
	containerfilePath := filepath.Join(projectPath, ".devsys", "Containerfile")
	data, err := os.ReadFile(containerfilePath)
	if err != nil {
		return fmt.Errorf("cannot read .devsys/Containerfile: %w", err)
	}
	currentHash := fmt.Sprintf("%x", sha256.Sum256(data))

	imageTag := fmt.Sprintf("devsys-%s", projectName)
	storedHash, err := podman.GetImageLabel(imageTag, "devsys.containerfile-hash")
	if err != nil {
		return fmt.Errorf("cannot read image label for %s: %w", imageTag, err)
	}
	if storedHash == "" {
		return fmt.Errorf("image %s has no containerfile hash — run 'devsys rebuild %s' to rebuild", imageTag, projectName)
	}
	if currentHash != storedHash {
		return fmt.Errorf(".devsys/Containerfile has changed since the last build — run 'devsys rebuild %s' first", projectName)
	}
	return nil
}

// checkPortsStale returns an error if .devsys/ports has changed since the
// image was last built. It compares the file's current SHA256 hash (or the
// hash of empty bytes when the file is absent) against the devsys.ports-hash
// label baked into the image at build time.
func checkPortsStale(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}
	currentHash := portsFileHash(projectPath)

	imageTag := fmt.Sprintf("devsys-%s", projectName)
	storedHash, err := podman.GetImageLabel(imageTag, "devsys.ports-hash")
	if err != nil {
		return fmt.Errorf("cannot read image label for %s: %w", imageTag, err)
	}
	if storedHash == "" {
		return fmt.Errorf("image %s has no ports hash — run 'devsys rebuild %s' to rebuild", imageTag, projectName)
	}
	if currentHash != storedHash {
		return fmt.Errorf(".devsys/ports has changed since the last build — run 'devsys rebuild %s' first", projectName)
	}
	return nil
}

// checkStaleness prints a non-blocking warning if this project's base image
// is behind the newest available version. Checked live on every `devsys
// enter` (previously throttled to once per 24h — dropped: a cached "you're
// up to date" answer actively works against noticing a version that shipped
// minutes ago, which defeats the point of checking at all).
func checkStaleness(projectName string) {
	warnIfBaseImageOutdated(projectName)
}

// warnIfCLIOutdated prints a warning if the running devsys binary is behind
// the newest published release. Any failure to check (no network, dev
// build) is silently skipped — this is a courtesy notice, not a
// requirement. Called from cmd/root.go's PersistentPostRun, throttled the
// same way as warnIfBaseImageOutdated.
func warnIfCLIOutdated() {
	if currentVersion == "dev" {
		return
	}
	latest, err := latestReleaseVersion()
	if err != nil || latest == currentVersion {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: devsys %s is available (you have %s). Run 'devsys update' to upgrade.\n", latest, currentVersion)
}

// warnIfBaseImageOutdated prints a warning if this project's built image is
// behind the newest published devsys-base. Reads the devsys.base-version
// label baked into the project image at build time and compares it against
// the latest semver tag on GHCR. Silently skipped on any failure.
func warnIfBaseImageOutdated(projectName string) {
	imageTag := fmt.Sprintf("devsys-%s", projectName)
	current, err := podman.GetImageLabel(imageTag, "devsys.base-version")
	if err != nil || current == "" || current == "dev" {
		return
	}
	latest, err := registry.LatestTag(devsysBaseImage)
	if err != nil || latest == current {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: a newer devsys-base is available for '%s' (%s → %s). Run 'devsys rebuild %s' to upgrade.\n",
		projectName, current, latest, projectName)
}

// checkTokenExpiry warns about any expiring GitLab token for the project —
// both the legacy single project-wide secret (devsys-<project>-gitlab-token,
// still held by any project created before the Git Remote & Credential
// Spec's rework and never re-authed since) and every per-repo secret the
// new devsys init/auth actually create. `secret rotate` no longer exists
// (Phase 7) — `devsys auth <project>` is the rerunnable command for this now,
// for both the legacy and per-repo cases alike.
func checkTokenExpiry(projectName string) {
	warnIfExpiring(fmt.Sprintf("devsys-%s-gitlab-token", projectName), projectName)

	containerName := fmt.Sprintf("devsys-%s", projectName)
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return
	}
	secretNames, err := projectRepoSecretNames(projectName, projectPath)
	if err != nil {
		return
	}
	for _, secretName := range secretNames {
		warnIfExpiring(secretName, projectName)
	}
}

func warnIfExpiring(secretName, projectName string) {
	labels, err := podman.GetSecretLabels(secretName)
	if err != nil {
		return
	}
	expiresAtStr, ok := labels["devsys.expires-at"]
	if !ok || expiresAtStr == "" {
		return
	}
	expiresAt, err := time.Parse("2006-01-02", expiresAtStr)
	if err != nil {
		return
	}
	daysLeft := int(time.Until(expiresAt).Hours() / 24)
	if daysLeft <= tokenExpiryWarnDays {
		fmt.Fprintf(os.Stderr, "Warning: token %s expires in %d day(s) (%s). Run 'devsys auth %s' to renew.\n",
			secretName, daysLeft, expiresAtStr, projectName)
	}
}
