package cmd

import (
	"fmt"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop <project>",
	Short: "Stop a project container",
	Args:  cobra.ExactArgs(1),
	RunE:  runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist", containerName)
	}
	if !podman.ContainerIsRunning(containerName) {
		fmt.Printf("Container %s is already stopped.\n", containerName)
		return nil
	}

	fmt.Printf("Stopping %s ...\n", containerName)
	if _, err := podman.RunPodman("stop", containerName); err != nil {
		return fmt.Errorf("cannot stop container: %w", err)
	}
	fmt.Printf("  %s stopped.\n", containerName)
	return nil
}
