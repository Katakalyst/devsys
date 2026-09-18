package cmd

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
)

var initGitLabURL string

var initCmd = &cobra.Command{
	Use:   "init <path>",
	Short: "Initialise a project directory as a devsys project",
	Args:  cobra.ExactArgs(1),
	RunE:  runInit,
}

func init() {
	initCmd.Flags().StringVar(&initGitLabURL, "gitlab-url", "https://gitlab.com",
		"GitLab base URL to create/attach the project against (self-hosted instances supported)")
}

func runInit(cmd *cobra.Command, args []string) error {
	projectPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("cannot resolve path: %w", err)
	}
	projectName := filepath.Base(projectPath)

	// Step 1: Create directory.
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", projectPath, err)
	}
	fmt.Printf("Project path: %s\n", projectPath)

	// Step 2: Initialise git repo.
	repo, err := gogit.PlainOpen(projectPath)
	if err == gogit.ErrRepositoryNotExists {
		fmt.Println("Initialising git repository...")
		repo, err = gogit.PlainInit(projectPath, false)
		if err != nil {
			return fmt.Errorf("cannot initialise git repository: %w", err)
		}
		fmt.Println("  Git repository initialised.")
	} else if err != nil {
		return fmt.Errorf("cannot open git repository: %w", err)
	} else {
		fmt.Println("  Git repository already exists.")
	}

	// Step 3: Detect / configure remote.
	var gitlabRemoteURL string
	var gitlabProjectID int

	// Read bootstrap PAT for GitLab API calls.
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return fmt.Errorf("cannot read bootstrap GitLab PAT (run 'devsys setup' first): %w", err)
	}
	glClient := gitlab.NewClient(initGitLabURL, bootstrapPAT)

	remotes, err := repo.Remotes()
	if err != nil {
		return fmt.Errorf("cannot list remotes: %w", err)
	}

	gitlabHost := hostFromURL(initGitLabURL)
	originIsGitLab := false
	hasOrigin := false
	for _, r := range remotes {
		if r.Config().Name == "origin" {
			hasOrigin = true
			for _, u := range r.Config().URLs {
				if strings.Contains(u, gitlabHost) {
					originIsGitLab = true
					gitlabRemoteURL = u
				}
			}
		}
	}

	switch {
	case originIsGitLab:
		fmt.Printf("  GitLab remote found: %s\n", gitlabRemoteURL)
		gitlabProjectID, err = glClient.GetProject(gitlabRemoteURL)
		if err != nil {
			return fmt.Errorf("cannot look up GitLab project: %w", err)
		}

	case hasOrigin && !originIsGitLab:
		// Non-GitLab origin: rename it to a host-derived name, create GitLab as new origin.
		repoCfg, err := repo.Config()
		if err != nil {
			return fmt.Errorf("cannot read repo config: %w", err)
		}
		oldCfg := repoCfg.Remotes["origin"]
		renameTo := "origin-backup"
		if oldCfg != nil && len(oldCfg.URLs) > 0 {
			renameTo = remoteNameFromURL(oldCfg.URLs[0])
		}
		fmt.Printf("  Non-GitLab origin detected. Renaming to %q and creating GitLab origin...\n", renameTo)
		if oldCfg != nil {
			repoCfg.Remotes[renameTo] = &gitconfig.RemoteConfig{
				Name: renameTo,
				URLs: oldCfg.URLs,
			}
			delete(repoCfg.Remotes, "origin")
			if err := repo.SetConfig(repoCfg); err != nil {
				return fmt.Errorf("cannot rename remote: %w", err)
			}
		}
		gitlabRemoteURL, gitlabProjectID, err = createGitLabProject(glClient, repo, projectName, initGitLabURL)
		if err != nil {
			return err
		}

	default:
		// No origin: prompt for GitLab URL or create new.
		gitlabRemoteURL, gitlabProjectID, err = promptOrCreateGitLab(glClient, repo, projectName, initGitLabURL)
		if err != nil {
			return err
		}
	}

	// Step 4: Create project access token (90-day expiry).
	tokenName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	expiresAt := time.Now().AddDate(0, 0, 90).Format("2006-01-02")
	fmt.Printf("Creating GitLab project access token %s (expires %s)...\n", tokenName, expiresAt)
	tokenID, tokenValue, err := glClient.CreateProjectToken(gitlabProjectID, tokenName, []string{"api"}, 40, expiresAt)
	if err != nil {
		return fmt.Errorf("cannot create GitLab project token: %w", err)
	}
	fmt.Printf("  Token created (ID %d).\n", tokenID)
	_ = gitlabRemoteURL // used for context above

	// Step 5: Store token as Podman secret.
	secretName := tokenName
	if podman.SecretExists(secretName) {
		fmt.Printf("  Secret %s already exists — removing old version.\n", secretName)
		if err := podman.DeleteSecret(secretName); err != nil {
			return fmt.Errorf("cannot remove existing secret: %w", err)
		}
	}
	labels := map[string]string{
		"devsys":             "true",
		"devsys.expires-at": expiresAt,
	}
	if err := podman.CreateSecretFromStdin(secretName, tokenValue, labels); err != nil {
		return fmt.Errorf("cannot store project token secret: %w", err)
	}
	fmt.Printf("  Stored secret %s.\n", secretName)

	// Step 6: Generate .devsys/Containerfile if absent, pinned to the
	// current newest devsys-base version (devsys CLI Spec, Section 12.4) —
	// never a floating tag.
	containerfilePath := filepath.Join(projectPath, ".devsys", "Containerfile")
	if _, err := os.Stat(containerfilePath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(containerfilePath), 0o755); err != nil {
			return fmt.Errorf("cannot create .devsys directory: %w", err)
		}
		baseRef, err := registry.LatestReference(devsysBaseImage)
		if err != nil {
			return fmt.Errorf("cannot look up newest devsys-base version: %w", err)
		}
		containerfileContent := fmt.Sprintf("FROM %s\n", baseRef)
		if err := os.WriteFile(containerfilePath, []byte(containerfileContent), 0o644); err != nil {
			return fmt.Errorf("cannot write Containerfile: %w", err)
		}
		fmt.Printf("  Generated %s (FROM %s).\n", containerfilePath, baseRef)
	} else {
		fmt.Printf("  %s already exists — skipping.\n", containerfilePath)
	}

	// Step 7: Build per-project image.
	imageTag := fmt.Sprintf("devsys-%s", projectName)
	fmt.Printf("Building image %s ...\n", imageTag)
	if err := buildImage(imageTag, containerfilePath, projectPath); err != nil {
		return fmt.Errorf("cannot build image: %w", err)
	}
	fmt.Printf("  Image %s built.\n", imageTag)

	// Step 8: Create persistent container.
	containerName := fmt.Sprintf("devsys-%s", projectName)
	if podman.ContainerExists(containerName) {
		fmt.Printf("Container %s already exists — skipping creation.\n", containerName)
	} else {
		fmt.Printf("Creating container %s ...\n", containerName)
		if err := createProjectContainer(containerName, projectName, projectPath, imageTag); err != nil {
			return fmt.Errorf("cannot create container: %w", err)
		}
		fmt.Printf("  Container %s created.\n", containerName)
	}

	fmt.Printf("\nProject '%s' is ready. Run 'devsys enter %s' to open Claude.\n", projectName, projectName)
	return nil
}

func createGitLabProject(glClient *gitlab.Client, repo *gogit.Repository, projectName, baseURL string) (string, int, error) {
	fmt.Printf("Creating GitLab project '%s'...\n", projectName)
	projectID, webURL, err := glClient.CreateProject(projectName)
	if err != nil {
		return "", 0, fmt.Errorf("cannot create GitLab project: %w", err)
	}
	// Derive SSH remote URL from web URL.
	sshURL := webURLToSSH(webURL, baseURL)
	repoCfg, err := repo.Config()
	if err != nil {
		return "", 0, fmt.Errorf("cannot read repo config: %w", err)
	}
	repoCfg.Remotes["origin"] = &gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{sshURL},
	}
	if err := repo.SetConfig(repoCfg); err != nil {
		return "", 0, fmt.Errorf("cannot set origin remote: %w", err)
	}
	fmt.Printf("  GitLab project created: %s\n", webURL)
	return sshURL, projectID, nil
}

func promptOrCreateGitLab(glClient *gitlab.Client, repo *gogit.Repository, projectName, baseURL string) (string, int, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter GitLab project URL (blank to create new): ")
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)

	if line == "" {
		return createGitLabProject(glClient, repo, projectName, baseURL)
	}

	// Use provided URL.
	projectID, err := glClient.GetProject(line)
	if err != nil {
		return "", 0, fmt.Errorf("cannot look up GitLab project %s: %w", line, err)
	}
	repoCfg, err := repo.Config()
	if err != nil {
		return "", 0, fmt.Errorf("cannot read repo config: %w", err)
	}
	repoCfg.Remotes["origin"] = &gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{line},
	}
	if err := repo.SetConfig(repoCfg); err != nil {
		return "", 0, fmt.Errorf("cannot set origin remote: %w", err)
	}
	return line, projectID, nil
}

func webURLToSSH(webURL, baseURL string) string {
	// https://gitlab.com/namespace/project → git@gitlab.com:namespace/project.git
	host := strings.TrimPrefix(baseURL, "https://")
	host = strings.TrimPrefix(host, "http://")
	path := strings.TrimPrefix(webURL, "https://"+host)
	path = strings.TrimPrefix(path, "http://"+host)
	path = strings.TrimPrefix(path, "/")
	return fmt.Sprintf("git@%s:%s.git", host, path)
}

// hostFromURL extracts the hostname from a URL string (e.g. "https://gitlab.com" → "gitlab.com").
func hostFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		// Fallback: strip scheme manually.
		s := strings.TrimPrefix(rawURL, "https://")
		s = strings.TrimPrefix(s, "http://")
		return strings.SplitN(s, "/", 2)[0]
	}
	return parsed.Host
}

// remoteNameFromURL derives a short remote name from a remote URL so a
// non-GitLab origin can be renamed to something descriptive rather than a
// hardcoded "github".
// Examples:
//
//	https://github.com/…       → "github"
//	git@bitbucket.org:…        → "bitbucket"
//	https://dev.azure.com/…    → "azure"
func remoteNameFromURL(remoteURL string) string {
	var host string
	if strings.HasPrefix(remoteURL, "git@") {
		// git@github.com:ns/proj.git → github.com
		parts := strings.SplitN(remoteURL, ":", 2)
		host = strings.TrimPrefix(parts[0], "git@")
	} else {
		parsed, err := url.Parse(remoteURL)
		if err == nil {
			host = parsed.Hostname()
		}
	}
	if host == "" {
		return "origin-backup"
	}
	// "github.com" → "github", "dev.azure.com" → "azure" (last meaningful segment)
	parts := strings.Split(strings.ToLower(host), ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return parts[0]
}

func buildImage(tag, containerfile, contextPath string) error {
	_, err := podman.RunPodman("build", "-t", tag, "-f", containerfile, contextPath)
	return err
}

// defaultWorkspaceDest is the in-container path the project workspace is
// bind-mounted to. The container always runs as root — no project
// Containerfile changes that — so this is a fixed constant, not something
// derived per-project (devsys CLI Spec, Section 7; KNOWN_ISSUES.md Issue 9).
const defaultWorkspaceDest = "/root/workspace"

// createProjectContainer creates the persistent devsys container.
// All named volumes are explicitly created with devsys=true before the
// container is created, so they appear in `devsys list` even if Podman
// would otherwise auto-create them without the label.
func createProjectContainer(containerName, projectName, projectPath, imageTag string) error {
	secretName := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	// Ensure all named volumes carry the devsys label.
	for _, vol := range []string{"devsys-claude-auth", "devsys-codex-auth", trivyVolume} {
		if !podman.VolumeExists(vol) {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", vol); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", vol, err)
			}
		}
	}

	// Seed skills into the claude-auth volume. The volume is mounted at /root/.claude
	// inside the project container, which hides the image layer at that path. We copy
	// the commands out of the image into the volume so Claude Code can find them.
	if _, err := podman.RunPodman(
		"run", "--rm",
		"--volume", "devsys-claude-auth:/dst",
		imageTag,
		"sh", "-c", "mkdir -p /dst/commands && cp -a /root/.claude/commands/. /dst/commands/",
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not seed skills into claude auth volume: %v\n", err)
	}

	_, err := podman.RunPodman(
		"create",
		"--name", containerName,
		"--label", "devsys=true",
		"--volume", projectPath+":"+defaultWorkspaceDest+":Z",
		"--volume", "devsys-claude-auth:/root/.claude",
		"--volume", "devsys-codex-auth:/root/.codex",
		"--volume", trivyVolume+":/root/.cache/trivy",
		"--secret", fmt.Sprintf("%s,type=env,target=GITLAB_TOKEN", secretName),
		"--env", "CLAUDE_CONFIG_DIR=/root/.claude",
		"--env", "CODEX_HOME=/root/.codex",
		"--env", "TRIVY_CACHE_DIR=/root/.cache/trivy",
		"--env", "IS_SANDBOX=1",
		imageTag,
		"sleep", "infinity",
	)
	return err
}
