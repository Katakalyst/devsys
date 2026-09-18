package cmd

import (
	"fmt"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start <project>",
	Short: "Start a project container",
	Args:  cobra.ExactArgs(1),
	RunE:  runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}
	if podman.ContainerIsRunning(containerName) {
		fmt.Printf("Container %s is already running.\n", containerName)
		return nil
	}

	fmt.Printf("Starting %s ...\n", containerName)
	if _, err := podman.RunPodman("start", containerName); err != nil {
		return fmt.Errorf("cannot start container: %w", err)
	}
	fmt.Printf("  %s started.\n", containerName)
	return nil
}
