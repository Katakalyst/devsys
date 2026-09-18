package cmd

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var rmCmd = &cobra.Command{
	Use:   "rm <project>",
	Short: "Remove a devsys project (container, secret, volume)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRm,
}

func runRm(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	containerName := fmt.Sprintf("devsys-%s", projectName)
	secretName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	reader := bufio.NewReader(os.Stdin)

	// Capture the project path now, before the container is removed — it is
	// needed later for GitLab token revocation (step 4).
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

	// Step 2: Remove secret.
	if podman.SecretExists(secretName) {
		if !confirm(reader, fmt.Sprintf("Remove secret %s?", secretName)) {
			fmt.Println("Skipping secret removal.")
		} else {
			if err := podman.DeleteSecret(secretName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot remove secret: %v\n", err)
			} else {
				fmt.Printf("  Secret %s removed.\n", secretName)
			}
		}
	} else {
		fmt.Printf("Secret %s not found — skipping.\n", secretName)
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

	// Step 4: Optionally revoke GitLab token.
	if confirm(reader, fmt.Sprintf("Revoke GitLab access token for project '%s'?", projectName)) {
		if err := revokeGitLabToken(projectName, projectPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot revoke GitLab token: %v\n", err)
		} else {
			fmt.Println("  GitLab token revoked.")
		}
	}

	return nil
}

// revokeGitLabToken revokes the project access token on GitLab.
// projectPath is the host-side workspace directory; it is captured before the
// container is removed so it is still available at this step.
func revokeGitLabToken(projectName, projectPath string) error {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return fmt.Errorf("cannot read bootstrap PAT: %w", err)
	}

	if projectPath == "" {
		return fmt.Errorf("cannot determine GitLab project ID for '%s' — revoke token manually", projectName)
	}
	glClient, err := gitlabClientForProject(projectPath, bootstrapPAT)
	if err != nil {
		return fmt.Errorf("cannot determine GitLab project ID for '%s' — revoke token manually", projectName)
	}

	tokenName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	projectID, err := getProjectIDFromGit(glClient, projectPath)
	if err != nil || projectID == 0 {
		return fmt.Errorf("cannot determine GitLab project ID for '%s' — revoke token manually", projectName)
	}

	tokens, err := glClient.GetProjectTokens(projectID)
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if t.Name == tokenName && !t.Revoked {
			return glClient.RevokeProjectToken(projectID, t.ID)
		}
	}
	return fmt.Errorf("token %s not found on GitLab", tokenName)
}

// originRemoteURL reads .git/config directly and returns the URL configured
// for the "origin" remote specifically (not just any remote). devsys init
// always makes the GitLab remote "origin" (devsys CLI Spec, Section 4), so by
// the time a project reaches rm or secret rotate, origin is by construction
// the GitLab remote — there is nothing to search for or compare against a
// separately configured GitLab hostname (KNOWN_ISSUES.md Issue 11's original
// fix compared against a configured host; this removes the need for any
// configured host at all).
func originRemoteURL(projectPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, ".git", "config"))
	if err != nil {
		return "", err
	}
	inOrigin := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if inOrigin && strings.HasPrefix(line, "url = ") {
			return strings.TrimPrefix(line, "url = "), nil
		}
	}
	return "", fmt.Errorf("no origin remote found in %s", projectPath)
}

// apiBaseURLFromRemote derives a GitLab REST API base URL (scheme + host)
// from a remote URL, whether SSH (git@host:ns/proj.git) or HTTPS
// (https://host/ns/proj). The API is always reached over HTTPS regardless of
// which protocol the git remote itself uses.
func apiBaseURLFromRemote(remoteURL string) (string, error) {
	if strings.HasPrefix(remoteURL, "git@") {
		parts := strings.SplitN(remoteURL, ":", 2)
		host := strings.TrimPrefix(parts[0], "git@")
		if host == "" {
			return "", fmt.Errorf("cannot parse host from remote URL %q", remoteURL)
		}
		return "https://" + host, nil
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("cannot parse host from remote URL %q", remoteURL)
	}
	return "https://" + parsed.Host, nil
}

// gitlabClientForProject builds a GitLab client scoped to whatever GitLab
// instance a project's own "origin" remote actually points at, rather than
// any separately configured or passed-in GitLab host.
func gitlabClientForProject(projectPath, bootstrapPAT string) (*gitlab.Client, error) {
	originURL, err := originRemoteURL(projectPath)
	if err != nil {
		return nil, err
	}
	apiBaseURL, err := apiBaseURLFromRemote(originURL)
	if err != nil {
		return nil, err
	}
	return gitlab.NewClient(apiBaseURL, bootstrapPAT), nil
}

// getProjectIDFromGit reads the project's origin remote and looks up its
// GitLab project ID.
func getProjectIDFromGit(glClient *gitlab.Client, projectPath string) (int, error) {
	originURL, err := originRemoteURL(projectPath)
	if err != nil {
		return 0, err
	}
	return glClient.GetProject(originURL)
}

// confirm prints a prompt and returns true only if the user types "y" or "Y".
func confirm(reader *bufio.Reader, prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := reader.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
