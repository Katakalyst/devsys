package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var rebuildCmd = &cobra.Command{
	Use:   "rebuild <project>",
	Short: "Rebuild the image and recreate the container for a project",
	Long:  `Rebuilds the project image from .devsys/Containerfile then removes and recreates the persistent container with the same flags.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runRebuild,
}

func runRebuild(cmd *cobra.Command, args []string) error {
	return rebuildProject(args[0])
}

func rebuildProject(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)

	// Refuse to rebuild while the container is running — there may be active
	// shell sessions inside, and force-removing a live container would disrupt
	// them. Exit all shells first; the container stops automatically on last exit.
	if podman.ContainerIsRunning(containerName) {
		return fmt.Errorf("container %s is running — exit all shells first (the container stops automatically on last exit)", containerName)
	}

	// Determine project path from container inspect.
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}

	containerfile := filepath.Join(projectPath, ".devsys", "Containerfile")
	imageTag := fmt.Sprintf("devsys-%s", projectName)

	fmt.Printf("Building image %s ...\n", imageTag)
	if err := buildImage(imageTag, containerfile, projectPath); err != nil {
		return fmt.Errorf("cannot build image: %w", err)
	}
	fmt.Printf("  Image %s built.\n", imageTag)

	// Remove existing container.
	if podman.ContainerExists(containerName) {
		fmt.Printf("Removing container %s ...\n", containerName)
		if _, err := podman.RunPodman("rm", containerName); err != nil {
			return fmt.Errorf("cannot remove container: %w", err)
		}
	}

	// Recreate container.
	fmt.Printf("Creating container %s ...\n", containerName)
	if err := createProjectContainer(containerName, projectName, projectPath, imageTag); err != nil {
		return fmt.Errorf("cannot recreate container: %w", err)
	}
	fmt.Printf("  Container %s recreated.\n", containerName)
	return nil
}

// getProjectPath reads the workspace mount path from a container's configuration.
func getProjectPath(containerName string) (string, error) {
	out, err := podman.RunPodman(
		"inspect", "--format",
		fmt.Sprintf(`{{range .Mounts}}{{if eq .Destination %q}}{{.Source}}{{end}}{{end}}`, defaultWorkspaceDest),
		containerName,
	)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("no %s mount found on container %s", defaultWorkspaceDest, containerName)
	}
	return out, nil
}

// containerToProject strips the "devsys-" prefix from a container name.
func containerToProject(name string) string {
	if len(name) > len("devsys-") && name[:len("devsys-")] == "devsys-" {
		return name[len("devsys-"):]
	}
	return name
}
