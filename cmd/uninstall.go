package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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
	if err := verifyPodmanAccessible(); err != nil {
		return err
	}

	inv, err := gatherUninstallInventory()
	if err != nil {
		return err
	}

	displayRemovalSummary(inv)

	reader := bufio.NewReader(os.Stdin)
	if !confirm(reader, "Remove everything listed above?") {
		fmt.Println("Aborted.")
		return nil
	}
	if !confirmExactly(reader, "uninstall") {
		fmt.Println("Aborted.")
		return nil
	}
	fmt.Println()

	// Revoke credentials before removing secrets — the bootstrap PAT is still
	// available here. Failures are non-fatal warnings (Git Remote & Credential
	// Spec §7: a network issue or missing PAT should not prevent uninstall).
	revokeAllProjectRepoCredentials(inv)
	removeAllContainers(inv.Containers)
	removeAllImages(inv.Images)
	removeAllVolumes(inv.Volumes)
	removeAllSecrets(inv.Secrets)
	removeCacheDirectory()
	removeInstallerPathEntry()

	// removeBinary calls os.Exit on Windows (cannot delete a running .exe),
	// so the completion message must come before it.
	fmt.Printf("Removing binary %s ...\n", inv.BinaryPath)
	fmt.Println("\ndevsys uninstalled.")
	removeBinary(inv.BinaryPath)
	return nil
}

// verifyPodmanAccessible confirms Podman is running and reachable. Hard
// failure — uninstall cannot remove containers, images, or volumes without it.
func verifyPodmanAccessible() error {
	if _, err := podman.RunPodman("info", "--format", "{{.Host.OS}}"); err != nil {
		return fmt.Errorf(
			"podman is not accessible — make sure Podman is running and try again\n" +
				"  (on Windows: ensure the WSL2 Podman machine is started)")
	}
	return nil
}

// uninstallInventory holds everything that will be removed during uninstall.
// ProjectPaths is captured before containers are deleted — credential
// revocation (step 7.5) needs workspace paths that are only discoverable
// while the containers still exist.
type uninstallInventory struct {
	Containers   []map[string]interface{}
	Images       []string
	Volumes      []map[string]interface{}
	Secrets      []string
	BinaryPath   string
	ProjectPaths map[string]string // containerName → workspace path
}

// gatherUninstallInventory lists all devsys resources on the machine and
// pre-captures each container's project workspace path. Hard failures on any
// list error — partial inventories would silently skip resources.
func gatherUninstallInventory() (*uninstallInventory, error) {
	containers, err := podman.ListDevsysContainers()
	if err != nil {
		return nil, fmt.Errorf("cannot list containers: %w", err)
	}

	// Capture project paths while containers still exist.
	projectPaths := make(map[string]string)
	for _, c := range containers {
		name := containerField(c, "Names")
		if path, err := getProjectPath(name); err == nil {
			projectPaths[containerToProject(name)] = path
		}
	}

	images, err := podman.ListDevsysImages()
	if err != nil {
		return nil, fmt.Errorf("cannot list images: %w", err)
	}
	volumes, err := podman.ListDevsysVolumes()
	if err != nil {
		return nil, fmt.Errorf("cannot list volumes: %w", err)
	}
	secrets, err := podman.ListDevsysSecrets()
	if err != nil {
		return nil, fmt.Errorf("cannot list secrets: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot determine binary path: %w", err)
	}

	return &uninstallInventory{
		Containers:   containers,
		Images:       images,
		Volumes:      volumes,
		Secrets:      secrets,
		BinaryPath:   execPath,
		ProjectPaths: projectPaths,
	}, nil
}

// displayRemovalSummary prints the formatted list of everything about to be
// deleted so the user can review before confirming.
func displayRemovalSummary(inv *uninstallInventory) {
	fmt.Println("The following will be permanently removed:")
	fmt.Println()

	fmt.Printf("  Containers (%d):\n", len(inv.Containers))
	for _, c := range inv.Containers {
		fmt.Printf("    - %s\n", containerField(c, "Names"))
	}

	fmt.Printf("\n  Images (%d):\n", len(inv.Images))
	for _, name := range inv.Images {
		fmt.Printf("    - %s\n", name)
	}

	fmt.Printf("\n  Volumes (%d):\n", len(inv.Volumes))
	for _, v := range inv.Volumes {
		fmt.Printf("    - %s\n", containerField(v, "Name"))
	}

	fmt.Printf("\n  Secrets (%d):\n", len(inv.Secrets))
	for _, name := range inv.Secrets {
		fmt.Printf("    - %s\n", name)
	}

	fmt.Printf("\n  Binary: %s\n\n", inv.BinaryPath)
}

// confirmExactly prompts the user to type requiredText verbatim. Returns true
// only on an exact match — used as a second safety gate for irreversible
// operations like uninstall.
func confirmExactly(reader *bufio.Reader, requiredText string) bool {
	fmt.Printf("\nThis cannot be undone. Type %q to confirm: ", requiredText)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line) == requiredText
}

// revokeAllProjectRepoCredentials discovers every repo remote across all
// projects in the inventory and best-effort revokes their credentials: GitLab
// tokens via API, GitHub tokens via a manual-revoke reminder. Reuses the same
// gatherRepoStatuses discovery that devsys auth and devsys rm use. Non-fatal
// — a network failure or missing bootstrap PAT warns and continues so the rest
// of uninstall is not blocked (Git Remote & Credential Spec §7).
func revokeAllProjectRepoCredentials(inv *uninstallInventory) {
	for _, c := range inv.Containers {
		name := containerField(c, "Names")
		projectName := containerToProject(name)
		projectPath := inv.ProjectPaths[projectName]
		if projectPath == "" {
			continue
		}
		statuses, err := gatherRepoStatuses(projectName, projectPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: cannot discover repos for %s: %v\n", projectName, err)
			continue
		}
		for _, st := range statuses {
			if !st.HasToken {
				continue
			}
			switch st.Platform {
			case "gitlab":
				fmt.Printf("Revoking GitLab token for %s (%s) ...\n", projectName, st.SecretName)
				if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
					fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
				}
			case "github":
				fmt.Printf("  %s (%s): %s\n", projectName, st.SecretName, strings.TrimSpace(githubManualRevokeReminder(st.RemoteURL)))
			}
		}
	}
}

// removeAllContainers stops (if running) and removes every container in the
// list. Non-fatal — warns on each error and continues.
func removeAllContainers(containers []map[string]interface{}) {
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
}

// removeAllImages force-removes every image in the list. Non-fatal.
func removeAllImages(images []string) {
	for _, name := range images {
		fmt.Printf("Removing image %s ...\n", name)
		if _, err := podman.RunPodman("image", "rm", "--force", name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}
}

// removeAllVolumes removes every volume in the list. Non-fatal.
func removeAllVolumes(volumes []map[string]interface{}) {
	for _, v := range volumes {
		name := containerField(v, "Name")
		fmt.Printf("Removing volume %s ...\n", name)
		if _, err := podman.RunPodman("volume", "rm", name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}
}

// removeAllSecrets deletes every Podman secret in the list. Non-fatal.
func removeAllSecrets(secrets []string) {
	for _, name := range secrets {
		fmt.Printf("Removing secret %s ...\n", name)
		if err := podman.DeleteSecret(name); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
		}
	}
}

// removeCacheDirectory removes the devsys staleness-check cache directory.
// Suppresses "not found"; warns on other errors. Non-fatal.
func removeCacheDirectory() {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	devsysCacheDir := filepath.Join(cacheDir, "devsys")
	fmt.Printf("Removing cache directory %s ...\n", devsysCacheDir)
	if err := os.RemoveAll(devsysCacheDir); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot remove cache directory: %v\n", err)
	}
}
