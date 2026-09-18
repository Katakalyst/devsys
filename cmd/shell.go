package cmd

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var shellCmd = &cobra.Command{
	Use:   "shell <project>",
	Short: "Open a bash shell inside a project container",
	Args:  cobra.ExactArgs(1),
	RunE:  runShell,
}

func runShell(cmd *cobra.Command, args []string) error {
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

	// Check token expiry (same as enter).
	checkTokenExpiry(projectName)

	// Try bash; fall back to sh if bash is not present in the image.
	err := podman.ExecInteractive(containerName, "bash")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 127 {
			fmt.Printf("bash not found in %s, falling back to sh\n", containerName)
			return podman.ExecInteractive(containerName, "sh")
		}
	}
	return err
}
