package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var rmCmd = &cobra.Command{
	Use:   "rm <project>",
	Short: "Remove a devsys project (container, secrets, volumes)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRm,
}

func runRm(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)
	legacySecretName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	reader := bufio.NewReader(os.Stdin)

	// Capture the project path now, before the container is removed — step 4
	// below needs to read the workspace's own repos/remotes from the host
	// bind mount, which is only discoverable via the running/existing
	// container's inspect data (getProjectPath), not after it's gone.
	projectPath, _ := getProjectPath(containerName)

	// Step 1: Stop and remove container.
	if podman.ContainerExists(containerName) {
		if !confirm(reader, fmt.Sprintf("Stop and remove container %s?", containerName)) {
			fmt.Println("Skipping container removal.")
		} else {
			if podman.ContainerIsRunning(containerName) {
				fmt.Printf("Stopping %s ...\n", containerName)
				if _, err := podman.RunPodman("stop", containerName); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: cannot stop container: %v\n", err)
				}
			}
			fmt.Printf("Removing container %s ...\n", containerName)
			if _, err := podman.RunPodman("rm", containerName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot remove container: %v\n", err)
			} else {
				fmt.Printf("  Container %s removed.\n", containerName)
			}
		}
	} else {
		fmt.Printf("Container %s not found — skipping.\n", containerName)
	}

	// Step 2: Remove the legacy single project-wide secret, if this project
	// predates the Git Remote & Credential Spec's per-repo rework and was
	// never re-authed since (the same legacy case devsys enter's
	// checkTokenExpiry still checks for). A project created/authed under the
	// current scheme never has this secret — step 4 below covers it instead.
	if podman.SecretExists(legacySecretName) {
		if !confirm(reader, fmt.Sprintf("Remove legacy secret %s?", legacySecretName)) {
			fmt.Println("Skipping legacy secret removal.")
		} else {
			if err := podman.DeleteSecret(legacySecretName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot remove secret: %v\n", err)
			} else {
				fmt.Printf("  Secret %s removed.\n", legacySecretName)
			}
		}
	}

	// Step 3: Remove trivy-db volume.
	if podman.VolumeExists(trivyVolume) {
		if !confirm(reader, fmt.Sprintf("Remove volume %s?", trivyVolume)) {
			fmt.Println("Skipping volume removal.")
		} else {
			if _, err := podman.RunPodman("volume", "rm", trivyVolume); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot remove volume: %v\n", err)
			} else {
				fmt.Printf("  Volume %s removed.\n", trivyVolume)
			}
		}
	} else {
		fmt.Printf("Volume %s not found — skipping.\n", trivyVolume)
	}

	// Step 4: Discover this project's actual per-repo, per-remote git
	// credentials (Git Remote & Credential Spec §7's multi-remote decision —
	// a project can have any number of repos, each with any number of
	// remotes, GitLab or GitHub) and offer to revoke/remove each one
	// individually. Reuses the exact same discovery and revoke/remove logic
	// devsys auth's own listing and [r] Remove menu option use
	// (gatherRepoStatuses, revokeAndDeleteSecret) rather than a second,
	// narrower implementation — GitLab tokens are revoked via API, GitHub
	// fine-grained PATs get a manual-revoke reminder (no revoke API exists
	// for those), and the Podman secret is deleted either way.
	revokeProjectRepoCredentials(reader, projectName, projectPath)

	return nil
}

func revokeProjectRepoCredentials(reader *bufio.Reader, projectName, projectPath string) {
	if projectPath == "" {
		fmt.Println("Cannot determine this project's workspace path — skipping per-repo credential revocation. Revoke any remaining GitLab/GitHub tokens manually.")
		return
	}
	statuses, err := gatherRepoStatuses(projectName, projectPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot discover this project's repos for credential revocation: %v\n", err)
		return
	}

	remoteCount := make(map[string]int)
	for _, st := range statuses {
		remoteCount[st.Repo.RelPath]++
	}

	found := false
	for i := range statuses {
		st := &statuses[i]
		if !st.HasToken {
			continue
		}
		found = true

		label := st.Repo.RelPath
		if remoteCount[st.Repo.RelPath] > 1 {
			label = fmt.Sprintf("%s (%s)", label, st.RemoteName)
		}
		if !confirm(reader, fmt.Sprintf("Revoke %s credential for %s (%s)?", st.Platform, label, st.SecretName)) {
			fmt.Printf("Skipping %s.\n", st.SecretName)
			continue
		}
		if err := revokeAndDeleteSecret(st); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot revoke/remove %s: %v\n", st.SecretName, err)
		} else {
			fmt.Printf("  %s revoked and removed.\n", st.SecretName)
		}
	}
	if !found {
		fmt.Println("No per-repo git credentials found for this project.")
	}
}

// confirm prints a prompt and returns true only if the user types "y" or "Y".
func confirm(reader *bufio.Reader, prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := reader.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
