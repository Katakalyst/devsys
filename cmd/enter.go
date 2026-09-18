package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
)

var enterCmd = &cobra.Command{
	Use:   "enter <project>",
	Short: "Open Claude inside a project container",
	Args:  cobra.ExactArgs(1),
	RunE:  runEnter,
}

func runEnter(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}

	// Ensure container is running.
	if !podman.ContainerIsRunning(containerName) {
		fmt.Printf("Starting %s ...\n", containerName)
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

	// Open Claude interactively.
	return podman.ExecInteractive(containerName, "claude")
}

// checkStaleness prints a non-blocking warning if this project's base image
// is behind the newest available version, throttled via a freely-deletable
// cache marker — never a live lookup on every `devsys enter`, the single
// most frequently run command in the system.
func checkStaleness(projectName string) {
	if !checkThrottled("base-image-" + projectName) {
		warnIfBaseImageOutdated(projectName)
		markChecked("base-image-" + projectName)
	}
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

// warnIfBaseImageOutdated prints a warning if this project's devsys-base
// version is behind the newest available at its own recorded location.
// Silently skipped on any failure — same courtesy-notice reasoning as
// warnIfCLIOutdated.
func warnIfBaseImageOutdated(projectName string) {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return
	}
	containerfilePath := filepath.Join(projectPath, ".devsys", "Containerfile")
	currentRef, err := containerfileFromLine(containerfilePath)
	if err != nil {
		return
	}
	newest, err := registry.LatestReference(locationFromReference(currentRef))
	if err != nil || newest == currentRef {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: a newer devsys-base is available for '%s' (%s). Run 'devsys update %s' to upgrade.\n",
		projectName, newest, projectName)
}

// checkTokenExpiry warns if the project's GitLab token expires within 30 days.
func checkTokenExpiry(projectName string) {
	secretName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
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
	if daysLeft <= 30 {
		fmt.Fprintf(os.Stderr, "Warning: GitLab token for '%s' expires in %d day(s) (%s). Run 'devsys secret rotate %s' to renew.\n",
			projectName, daysLeft, expiresAtStr, projectName)
	}
}
