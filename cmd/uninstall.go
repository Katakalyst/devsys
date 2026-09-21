package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove all devsys containers, images, volumes, secrets, and the binary",
	Long: `Permanently removes everything devsys installed on this machine:
all project containers, all devsys images, all shared volumes (Claude/Codex
auth, trivy databases), all secrets (GitLab tokens), and the devsys binary.

Project workspaces and .devsys/Containerfile files are not touched.

Requires two confirmations to prevent accidental removal.`,
	RunE: runUninstall,
}

func runUninstall(cmd *cobra.Command, args []string) error {
	// Step 1: Check Podman is accessible — needed to remove containers,
	// images, volumes, and secrets.
	if _, err := podman.RunPodman("info", "--format", "{{.Host.OS}}"); err != nil {
		return fmt.Errorf(
			"Podman is not accessible — make sure Podman is running and try again\n" +
				"  (on Windows: ensure the WSL2 Podman machine is started)")
	}

	// Gather everything that will be removed.
	containers, err := podman.ListDevsysContainers()
	if err != nil {
		return fmt.Errorf("cannot list containers: %w", err)
	}
	images, err := podman.ListDevsysImages()
	if err != nil {
		return fmt.Errorf("cannot list images: %w", err)
	}
	volumes, err := podman.ListDevsysVolumes()
	if err != nil {
		return fmt.Errorf("cannot list volumes: %w", err)
	}
	secrets, err := podman.ListDevsysSecrets()
	if err != nil {
		return fmt.Errorf("cannot list secrets: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine binary path: %w", err)
	}

	// Step 2: Show what will be removed.
	fmt.Println("The following will be permanently removed:")
	fmt.Println()

	fmt.Printf("  Containers (%d):\n", len(containers))
	for _, c := range containers {
		fmt.Printf("    - %s\n", containerField(c, "Names"))
	}

	fmt.Printf("\n  Images (%d):\n", len(images))
	for _, name := range images {
		fmt.Printf("    - %s\n", name)
	}

	fmt.Printf("\n  Volumes (%d):\n", len(volumes))
	for _, v := range volumes {
		fmt.Printf("    - %s\n", containerField(v, "Name"))
	}

	fmt.Printf("\n  Secrets (%d):\n", len(secrets))
	for _, name := range secrets {
		fmt.Printf("    - %s\n", name)
	}

	fmt.Printf("\n  Binary: %s\n\n", execPath)

	reader := bufio.NewReader(os.Stdin)

	// Step 3: First confirmation.
	if !confirm(reader, "Remove everything listed above?") {
		fmt.Println("Aborted.")
		return nil
	}

	// Step 4: Second confirmation — must type "uninstall" exactly.
	fmt.Print("\nThis cannot be undone. Type 'uninstall' to confirm: ")
	line, _ := reader.ReadString('\n')
	if strings.TrimSpace(line) != "uninstall" {
		fmt.Println("Aborted.")
		return nil
	}
	fmt.Println()

	// Step 5: Remove containers.
	for _, c := range containers {
		name := containerField(c, "Names")
		if podman.ContainerIsRunning(name) {
			fmt.Printf("Stopping %s ...\n", name)
			if _, err := podman.RunPodman("stop", name); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
			}
		}
		fmt.Printf("Removing container %s ...\n", name)
		if _, err := podman.RunPodman("rm", name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}

	// Step 6: Remove images.
	for _, name := range images {
		fmt.Printf("Removing image %s ...\n", name)
		if _, err := podman.RunPodman("image", "rm", "--force", name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}

	// Step 7: Remove volumes.
	for _, v := range volumes {
		name := containerField(v, "Name")
		fmt.Printf("Removing volume %s ...\n", name)
		if _, err := podman.RunPodman("volume", "rm", name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}

	// Step 8: Remove secrets.
	for _, name := range secrets {
		fmt.Printf("Removing secret %s ...\n", name)
		if err := podman.DeleteSecret(name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}

	// Step 9: Remove the binary. On Unix, deleting a running binary is safe —
	// the process continues from the already-loaded image; the file is simply
	// unlinked. The user's terminal session continues normally after this exits.
	fmt.Printf("Removing binary %s ...\n", execPath)
	if err := os.Remove(execPath); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot remove binary: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Remove it manually: rm %s\n", execPath)
	}

	fmt.Println("\ndevsys uninstalled.")
	return nil
}
