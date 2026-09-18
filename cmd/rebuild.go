package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var rebuildAll bool

var rebuildCmd = &cobra.Command{
	Use:   "rebuild <project>",
	Short: "Rebuild the image and recreate the container for a project",
	Long: `Rebuilds the project image from .devsys/Containerfile then removes and
recreates the persistent container with the same flags. Use --all to rebuild
every devsys-managed project.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRebuild,
}

func init() {
	rebuildCmd.Flags().BoolVar(&rebuildAll, "all", false, "Rebuild all devsys projects")
}

func runRebuild(cmd *cobra.Command, args []string) error {
	if rebuildAll {
		containers, err := podman.ListDevsysContainers()
		if err != nil {
			return fmt.Errorf("cannot list devsys containers: %w", err)
		}
		if len(containers) == 0 {
			fmt.Println("No devsys containers found.")
			return nil
		}
		for _, c := range containers {
			name, _ := c["Names"].(string)
			if name == "" {
				if names, ok := c["Names"].([]interface{}); ok && len(names) > 0 {
					name, _ = names[0].(string)
				}
			}
			projectName := containerToProject(name)
			fmt.Printf("Rebuilding %s...\n", projectName)
			if err := rebuildProject(projectName); err != nil {
				fmt.Printf("  Error rebuilding %s: %v\n", projectName, err)
			}
		}
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a project name or use --all")
	}
	return rebuildProject(args[0])
}

func rebuildProject(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)

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
		if _, err := podman.RunPodman("rm", "-f", containerName); err != nil {
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
