package cmd

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
)

// authGitLabURL is the base URL a brand-new GitLab project is created
// against, or an attach target's project is looked up against when the user
// gives a bare owner/path instead of a full URL. Mirrors devsys init's own
// --gitlab-url flag (self-hosted instances supported).
var authGitLabURL string

// --- GitLab-specific setup --------------------------------------------------

// gitLabClient builds a GitLab client from the bootstrap PAT. Shared by
// every GitLab code path so failure behaves identically everywhere:
// returning an error here, rather than prompting, is what makes the
// scriptable form's "bootstrap PAT missing -> fails immediately, never
// prompts" rule (Spec §9) automatic rather than something each caller has
// to remember to implement.
func gitLabClient() (*gitlab.Client, error) {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return nil, fmt.Errorf("cannot read bootstrap GitLab PAT (run 'devsys auth gitlab' first): %w", err)
	}
	return gitlab.NewClient(authGitLabURL, bootstrapPAT), nil
}

// resolveGitLabProject creates ("create" mode, nameOrTarget is the new
// project's name) or looks up ("attach" mode, nameOrTarget is an
// owner/repo or full URL) a GitLab project. Shared by the interactive and
// scriptable forms — the only difference between them is how nameOrTarget
// gets collected (prompted vs. --name/--attach flags).
func resolveGitLabProject(glClient *gitlab.Client, mode, nameOrTarget string) (projectID int, webURL string, err error) {
	if mode == "create" {
		projectID, webURL, err = glClient.CreateProject(nameOrTarget)
		if err != nil {
			return 0, "", fmt.Errorf("cannot create GitLab project: %w", err)
		}
		return projectID, webURL, nil
	}
	projectID, err = glClient.GetProject(nameOrTarget)
	if err != nil {
		return 0, "", fmt.Errorf("cannot find GitLab project %s: %w", nameOrTarget, err)
	}
	if strings.HasPrefix(nameOrTarget, "http://") || strings.HasPrefix(nameOrTarget, "https://") {
		webURL = nameOrTarget
	} else {
		webURL = strings.TrimRight(authGitLabURL, "/") + "/" + nameOrTarget
	}
	return projectID, webURL, nil
}

func setupGitLabRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", "", nil, err
	}

	var nameOrTarget string
	if mode == "create" {
		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Project name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		nameOrTarget = name
	} else {
		fmt.Print("  GitLab project (owner/repo or URL): ")
		line, _ := reader.ReadString('\n')
		target := strings.TrimSpace(line)
		if target == "" {
			return "", "", nil, fmt.Errorf("GitLab project target cannot be empty")
		}
		nameOrTarget = target
	}

	projectID, webURL, err := resolveGitLabProject(glClient, mode, nameOrTarget)
	if err != nil {
		return "", "", nil, err
	}
	if mode == "create" {
		fmt.Printf("  -> created %s\n", webURL)
	}

	tokenValue, labels, err = mintGitLabToken(glClient, projectID, webURL)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(webURL, "oauth2", tokenValue)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, tokenValue, labels, nil
}

// setupGitLabRemoteScriptable is setupGitLabRemote's non-interactive
// counterpart: nameOrTarget comes from --name/--attach, never prompted
// (Git Remote & Credential Spec §9: the scriptable form never prompts).
func setupGitLabRemoteScriptable(projectName string, st *repoAuthStatus, mode, nameOrTarget string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", "", nil, err
	}
	if mode == "create" && nameOrTarget == "" {
		nameOrTarget = repoDefaultName(projectName, st.Repo.RelPath)
	}
	if mode == "attach" && nameOrTarget == "" {
		return "", "", nil, fmt.Errorf("--attach requires a target (owner/repo or URL)")
	}

	projectID, webURL, err := resolveGitLabProject(glClient, mode, nameOrTarget)
	if err != nil {
		return "", "", nil, err
	}
	tokenValue, labels, err = mintGitLabToken(glClient, projectID, webURL)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(webURL, "oauth2", tokenValue)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, tokenValue, labels, nil
}

// mintGitLabTokenForRemote mints a fresh project access token for whatever
// GitLab project remoteURL (bare or token-embedded) points at, looking up
// the project ID first. Used by the remote-already-exists and rotate
// branches, which only have a URL, not an ID already in hand.
func mintGitLabTokenForRemote(remoteURL string) (tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", nil, err
	}
	projectID, err := glClient.GetProject(remoteURL)
	if err != nil {
		return "", nil, fmt.Errorf("cannot find GitLab project for %s: %w", remoteURL, err)
	}
	return mintGitLabToken(glClient, projectID, remoteURL)
}

// mintGitLabToken mints a fresh project access token for an already-known
// projectID, named deterministically from repoURL's repo-id so
// revokeGitLabRepoToken can find it again later by name alone.
func mintGitLabToken(glClient *gitlab.Client, projectID int, repoURL string) (tokenValue string, labels map[string]string, err error) {
	repoID, err := workspace.RepoIDFromURL(repoURL)
	if err != nil {
		return "", nil, fmt.Errorf("cannot determine repo id: %w", err)
	}
	tokenName := fmt.Sprintf("devsys-%s-token", repoID)
	expiresAt := time.Now().AddDate(0, 0, 90).Format("2006-01-02")

	_, tokenValue, err = glClient.CreateProjectToken(projectID, tokenName, []string{"api"}, 40, expiresAt)
	if err != nil {
		return "", nil, fmt.Errorf("cannot mint GitLab token: %w", err)
	}
	return tokenValue, map[string]string{"devsys": "true", "devsys.expires-at": expiresAt}, nil
}

// revokeGitLabRepoToken best-effort revokes the devsys-minted token for
// remoteURL, matched by the deterministic name mintGitLabTokenForRemote
// gives it. Finding nothing matching is not an error — there may be
// nothing to revoke (e.g. an agent-set remote whose token was minted before
// this naming convention, or already revoked).
func revokeGitLabRepoToken(remoteURL string) error {
	glClient, err := gitLabClient()
	if err != nil {
		return err
	}
	projectID, err := glClient.GetProject(remoteURL)
	if err != nil {
		return err
	}
	repoID, err := workspace.RepoIDFromURL(remoteURL)
	if err != nil {
		return err
	}
	wantName := fmt.Sprintf("devsys-%s-token", repoID)

	tokens, err := glClient.GetProjectTokens(projectID)
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if t.Name == wantName && !t.Revoked {
			return glClient.RevokeProjectToken(projectID, t.ID)
		}
	}
	return nil
}
