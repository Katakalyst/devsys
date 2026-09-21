package cmd

import (
	"fmt"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage devsys secrets",
}

var secretRotateCmd = &cobra.Command{
	Use:   "rotate <project> [credential]",
	Short: "Rotate the GitLab project access token for a project",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSecretRotate,
}

func init() {
	secretCmd.AddCommand(secretRotateCmd)
}

func runSecretRotate(cmd *cobra.Command, args []string) error {
	projectName := args[0]
	// credential arg is reserved for future credential types; currently only gitlab-token.
	containerName := fmt.Sprintf("devsys-%s", projectName)
	secretName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	tokenName := secretName

	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return fmt.Errorf("cannot read bootstrap PAT (run 'devsys auth gitlab' first): %w", err)
	}

	// Determine project ID from git remote.
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path — is the project initialised? %w", err)
	}
	glClient, err := gitlabClientForProject(projectPath, bootstrapPAT)
	if err != nil {
		return fmt.Errorf("cannot determine GitLab remote for project: %w", err)
	}
	projectID, err := getProjectIDFromGit(glClient, projectPath)
	if err != nil {
		return fmt.Errorf("cannot determine GitLab project ID: %w", err)
	}

	// Find old token ID.
	tokens, err := glClient.GetProjectTokens(projectID)
	if err != nil {
		return fmt.Errorf("cannot list project tokens: %w", err)
	}
	oldTokenID := 0
	for _, t := range tokens {
		if t.Name == tokenName && !t.Revoked {
			oldTokenID = t.ID
			break
		}
	}

	// Step 1: Create new token.
	expiresAt := time.Now().AddDate(0, 0, 90).Format("2006-01-02")
	fmt.Printf("Creating new GitLab token for project %d (expires %s)...\n", projectID, expiresAt)
	newTokenID, newTokenValue, err := glClient.CreateProjectToken(projectID, tokenName, []string{"api"}, 40, expiresAt)
	if err != nil {
		return fmt.Errorf("cannot create new GitLab token: %w", err)
	}
	fmt.Printf("  New token created (ID %d).\n", newTokenID)

	// Step 2: Revoke old token.
	if oldTokenID != 0 {
		fmt.Printf("Revoking old token (ID %d)...\n", oldTokenID)
		if err := glClient.RevokeProjectToken(projectID, oldTokenID); err != nil {
			fmt.Printf("  Warning: cannot revoke old token: %v\n", err)
		} else {
			fmt.Println("  Old token revoked.")
		}
	}

	// Step 3: Replace Podman secret.
	if podman.SecretExists(secretName) {
		fmt.Printf("Removing old secret %s ...\n", secretName)
		if err := podman.DeleteSecret(secretName); err != nil {
			return fmt.Errorf("cannot delete old secret: %w", err)
		}
	}
	labels := map[string]string{
		"devsys":             "true",
		"devsys.expires-at": expiresAt,
	}
	if err := podman.CreateSecretFromStdin(secretName, newTokenValue, labels); err != nil {
		return fmt.Errorf("cannot store new secret: %w", err)
	}
	fmt.Printf("  Secret %s updated.\n", secretName)

	// Step 4: Recreate container.
	imageTag := fmt.Sprintf("devsys-%s", projectName)
	if podman.ContainerExists(containerName) {
		fmt.Printf("Recreating container %s ...\n", containerName)
		if podman.ContainerIsRunning(containerName) {
			if _, err := podman.RunPodman("stop", containerName); err != nil {
				return fmt.Errorf("cannot stop container: %w", err)
			}
		}
		if _, err := podman.RunPodman("rm", containerName); err != nil {
			return fmt.Errorf("cannot remove container: %w", err)
		}
		if err := createProjectContainer(containerName, projectName, projectPath, imageTag); err != nil {
			return fmt.Errorf("cannot recreate container: %w", err)
		}
		fmt.Printf("  Container %s recreated.\n", containerName)
	}

	fmt.Printf("\nSecret rotation complete for project '%s'.\n", projectName)
	return nil
}
