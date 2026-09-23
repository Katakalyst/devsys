package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/github"
	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
	"github.com/spf13/cobra"
)

// authGitLabURL is the base URL a brand-new GitLab project is created
// against, or an attach target's project is looked up against when the user
// gives a bare owner/path instead of a full URL. Mirrors devsys init's own
// --gitlab-url flag (self-hosted instances supported).
var authGitLabURL string

// tokenExpiryWarnDays is the same 30-day threshold devsys enter's
// checkTokenExpiry already uses — one shared threshold for the whole CLI,
// not a second number to keep in sync (Git Remote & Credential Spec §9).
const tokenExpiryWarnDays = 30

// repoAuthStatus is one discovered repo's current credential state, as shown
// in `devsys auth <project>`'s interactive listing (Git Remote & Credential
// Spec §8/§9).
type repoAuthStatus struct {
	Repo       workspace.Repo
	HasRemote  bool
	RemoteURL  string
	Platform   string // "" until a remote exists
	SecretName string // "" until a token has been minted/stored
	HasToken   bool
	ExpiresAt  string // "" when unknown (GitHub PATs never expose this)
}

// runAuthProject is authCmd's own RunE, invoked whenever the first argument
// to `devsys auth` doesn't match one of the claude/codex/gitlab/github
// bootstrap subcommands — i.e. it's a project name (Git Remote & Credential
// Spec §9's command list). Only the bare interactive-listing form is
// implemented here; the scriptable --create/--attach/--rotate/--remove
// flags are a separate phase (documents/Git Remote & Credential
// Implementation Plan.md, Phase 5), built on this same per-repo logic.
func runAuthProject(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devsys auth <project> | devsys auth gitlab|github|claude|codex")
	}
	if len(args) > 1 {
		return fmt.Errorf("devsys auth <project>: scriptable single-repo flags are not implemented yet — run without extra arguments for the interactive listing")
	}
	return runAuthProjectInteractive(args[0])
}

func runAuthProjectInteractive(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}
	workspaceRoot, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}

	reader := bufio.NewReader(os.Stdin)
	changed := false

	for {
		statuses, err := gatherRepoStatuses(projectName, workspaceRoot)
		if err != nil {
			return err
		}
		if len(statuses) == 0 {
			fmt.Println("No repos found in this project's workspace.")
			break
		}

		fmt.Println()
		printRepoListing(statuses)
		fmt.Print("\n  [1,2,...] select   [d] done\n\n> ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "d") {
			break
		}

		indices, err := parseSelection(line, len(statuses))
		if err != nil {
			fmt.Printf("  %v\n", err)
			continue
		}
		for _, idx := range indices {
			if err := configureRepo(reader, projectName, &statuses[idx]); err != nil {
				fmt.Printf("  Error: %v\n", err)
				continue
			}
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return recreateContainerForAuth(projectName)
}

// gatherRepoStatuses runs the recursive discovery walk and derives each
// repo's current credential state — purely by reading git config and
// checking which Podman secrets already exist, never from a stored config
// file (R15).
func gatherRepoStatuses(projectName, workspaceRoot string) ([]repoAuthStatus, error) {
	repos, err := workspace.DiscoverRepos(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot scan workspace for repos: %w", err)
	}

	statuses := make([]repoAuthStatus, 0, len(repos))
	for _, r := range repos {
		st := repoAuthStatus{Repo: r}

		originURL, err := r.OriginURL()
		if err != nil {
			if errors.Is(err, workspace.ErrNoOrigin) {
				statuses = append(statuses, st)
				continue
			}
			return nil, fmt.Errorf("cannot read remote for %s: %w", r.RelPath, err)
		}
		st.HasRemote = true
		st.RemoteURL = originURL

		platform, err := workspace.PlatformFromURL(originURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine platform for %s: %w", r.RelPath, err)
		}
		st.Platform = platform

		repoID, err := workspace.RepoIDFromURL(originURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine repo id for %s: %w", r.RelPath, err)
		}
		secretName := workspace.SecretName(projectName, repoID, platform)
		if podman.SecretExists(secretName) {
			st.SecretName = secretName
			st.HasToken = true
			if labels, err := podman.GetSecretLabels(secretName); err == nil {
				st.ExpiresAt = labels["devsys.expires-at"]
			}
		}

		statuses = append(statuses, st)
	}
	return statuses, nil
}

func printRepoListing(statuses []repoAuthStatus) {
	for i, st := range statuses {
		switch {
		case !st.HasRemote:
			fmt.Printf("  %d. %-15s no remote\n", i+1, st.Repo.RelPath)
		case !st.HasToken:
			fmt.Printf("  %d. %-15s %-7s no token\n", i+1, st.Repo.RelPath, st.Platform)
		default:
			fmt.Printf("  %d. %-15s %-7s %s\n", i+1, st.Repo.RelPath, st.Platform, tokenExpiryText(st.ExpiresAt))
		}
	}
}

// tokenExpiryText formats a token's remaining lifetime, or "token ok" when
// the platform doesn't expose an expiry (GitHub fine-grained PATs — Git
// Remote & Credential Implementation Plan.md's open question on this is
// resolved here: no expiry is shown when none is known, rather than guessing).
func tokenExpiryText(expiresAtStr string) string {
	if expiresAtStr == "" {
		return "token ok"
	}
	expiresAt, err := time.Parse("2006-01-02", expiresAtStr)
	if err != nil {
		return "token ok"
	}
	daysLeft := int(time.Until(expiresAt).Hours() / 24)
	warn := ""
	if daysLeft <= tokenExpiryWarnDays {
		warn = "   ⚠"
	}
	return fmt.Sprintf("token ok, expires in %d days%s", daysLeft, warn)
}

// parseSelection parses a multi-select line of 1-based indices, separated by
// comma or space — never inferred from concatenated digits ("15" is index
// fifteen, never "1" and "5") (Git Remote & Credential Spec §9). Duplicate
// indices are deduplicated, first-occurrence order preserved.
func parseSelection(input string, max int) ([]int, error) {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' '
	})
	var indices []int
	seen := make(map[int]bool)
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("invalid selection %q", f)
		}
		if n < 1 || n > max {
			return nil, fmt.Errorf("selection %d out of range (1-%d)", n, max)
		}
		if !seen[n] {
			seen[n] = true
			indices = append(indices, n-1)
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("no selection")
	}
	return indices, nil
}

func configureRepo(reader *bufio.Reader, projectName string, st *repoAuthStatus) error {
	switch {
	case !st.HasRemote:
		return configureNoRemote(reader, projectName, st)
	case !st.HasToken:
		return configureRemoteNoToken(reader, projectName, st)
	default:
		return configureAlreadySet(reader, projectName, st)
	}
}

// configureNoRemote runs the create-or-attach wizard for a repo with no
// remote yet — the only branch that ever writes a remote (Git Remote &
// Credential Spec §7: "auth never rewrites a remote it didn't create").
func configureNoRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus) error {
	fmt.Printf("Configuring %q (no remote):\n", st.Repo.RelPath)
	platform, err := promptPlatform(reader)
	if err != nil {
		return err
	}
	mode, err := promptCreateOrAttach(reader)
	if err != nil {
		return err
	}

	var remoteURL, tokenValue string
	var labels map[string]string
	switch platform {
	case "gitlab":
		remoteURL, tokenValue, labels, err = setupGitLabRemote(reader, projectName, st, mode)
	case "github":
		remoteURL, tokenValue, err = setupGitHubRemote(reader, projectName, st, mode)
	}
	if err != nil {
		return err
	}

	if err := writeOriginRemote(st.Repo.AbsPath, remoteURL); err != nil {
		return fmt.Errorf("cannot write remote: %w", err)
	}

	repoID, err := workspace.RepoIDFromURL(remoteURL)
	if err != nil {
		return fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, platform)
	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return err
	}

	fmt.Printf("  -> remote wired, token stored (%s)\n", secretName)
	st.HasRemote = true
	st.RemoteURL = remoteURL
	st.Platform = platform
	st.SecretName = secretName
	st.HasToken = true
	st.ExpiresAt = labels["devsys.expires-at"]
	return nil
}

// configureRemoteNoToken mints/stores a token for a repo whose remote the
// agent already set itself. Platform is derived from the existing URL; no
// create/attach question, no remote rewrite (Git Remote & Credential Spec §7/§8).
func configureRemoteNoToken(reader *bufio.Reader, projectName string, st *repoAuthStatus) error {
	fmt.Printf("Configuring %q: remote already set -> %s\n", st.Repo.RelPath, st.RemoteURL)
	fmt.Printf("  Platform: %s (derived from remote)\n", st.Platform)

	repoID, err := workspace.RepoIDFromURL(st.RemoteURL)
	if err != nil {
		return fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, st.Platform)

	var tokenValue string
	var labels map[string]string
	switch st.Platform {
	case "gitlab":
		tokenValue, labels, err = mintGitLabTokenForRemote(st.RemoteURL)
	case "github":
		repoPath, pathErr := workspace.PathFromURL(st.RemoteURL)
		if pathErr != nil {
			return fmt.Errorf("cannot determine owner/repo: %w", pathErr)
		}
		tokenValue, err = promptAndVerifyGitHubPAT(reader, repoPath)
	}
	if err != nil {
		return err
	}

	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return err
	}
	fmt.Printf("  -> token stored (%s)\n", secretName)
	st.SecretName = secretName
	st.HasToken = true
	st.ExpiresAt = labels["devsys.expires-at"]
	return nil
}

// configureAlreadySet shows the change/rotate/remove/back menu for a fully
// configured repo (Git Remote & Credential Spec §9) — selecting one is never
// a no-op or an error.
func configureAlreadySet(reader *bufio.Reader, projectName string, st *repoAuthStatus) error {
	fmt.Printf("%q is already configured — %s, %s.\n", st.Repo.RelPath, st.Platform, tokenExpiryText(st.ExpiresAt))
	fmt.Println("  [c] Change platform/target")
	fmt.Println("  [x] Rotate token (same target, fresh token)")
	fmt.Println("  [r] Remove token (remote left as-is)")
	fmt.Println("  [b] Back")
	fmt.Print("\n> ")
	line, _ := reader.ReadString('\n')
	choice := strings.ToLower(strings.TrimSpace(line))

	switch choice {
	case "c":
		return changeRepoTarget(reader, projectName, st)
	case "x":
		return rotateRepoToken(reader, st)
	case "r":
		return removeRepoToken(st)
	case "b", "":
		return nil
	default:
		fmt.Println("  Unrecognized option.")
		return nil
	}
}

// changeRepoTarget is the [c] menu option: revoke/delete the current
// credential, clear the remote, and re-run the create/attach wizard — an
// explicit, user-requested rewrite. Doesn't conflict with "auth never
// rewrites a remote it didn't create" (Spec §7): that rule is about auth's
// own default behavior, not a menu the user deliberately opened (Spec §9).
func changeRepoTarget(reader *bufio.Reader, projectName string, st *repoAuthStatus) error {
	if err := revokeAndDeleteSecret(st); err != nil {
		return err
	}
	if err := clearOriginRemote(st.Repo.AbsPath); err != nil {
		return err
	}
	st.HasRemote = false
	st.HasToken = false
	st.RemoteURL = ""
	st.Platform = ""
	st.SecretName = ""
	st.ExpiresAt = ""
	return configureNoRemote(reader, projectName, st)
}

// rotateRepoToken is the [x] menu option: same target, fresh token value.
// GitLab is fully automatic (revoke via API, mint a new one). GitHub fine-
// grained PATs have no revoke/remint API (Git Remote & Credential
// Implementation Plan.md's open question on this — resolved here: prompt
// for a newly user-created PAT, same as the remote-no-token flow, with a
// reminder to revoke the old one manually).
func rotateRepoToken(reader *bufio.Reader, st *repoAuthStatus) error {
	fmt.Printf("This will revoke the current token and mint a fresh one for the same target (%s).\n", st.RemoteURL)
	if !confirm(reader, "Continue?") {
		fmt.Println("  Skipped.")
		return nil
	}

	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not revoke old GitLab token via API: %v\n", err)
		}
		tokenValue, labels, err := mintGitLabTokenForRemote(st.RemoteURL)
		if err != nil {
			return err
		}
		if err := storeRepoSecret(st.SecretName, tokenValue, labels); err != nil {
			return err
		}
		st.ExpiresAt = labels["devsys.expires-at"]
		fmt.Printf("  -> new token minted (expires %s)\n", st.ExpiresAt)
	case "github":
		fmt.Println("  GitHub fine-grained PATs can't be rotated via API — create a new one, then paste it below.")
		fmt.Println("  Remember to revoke the old PAT yourself at github.com/settings/tokens once the new one is confirmed working.")
		repoPath, err := workspace.PathFromURL(st.RemoteURL)
		if err != nil {
			return fmt.Errorf("cannot determine owner/repo: %w", err)
		}
		tokenValue, err := promptAndVerifyGitHubPAT(reader, repoPath)
		if err != nil {
			return err
		}
		if err := storeRepoSecret(st.SecretName, tokenValue, nil); err != nil {
			return err
		}
		st.ExpiresAt = ""
		fmt.Println("  -> new token stored")
	}
	st.HasToken = true
	return nil
}

// removeRepoToken is the [r] menu option: revoke and unmount, remote left
// as-is (R12.5).
func removeRepoToken(st *repoAuthStatus) error {
	if err := revokeAndDeleteSecret(st); err != nil {
		return err
	}
	st.HasToken = false
	st.SecretName = ""
	st.ExpiresAt = ""
	fmt.Println("  -> token removed, remote left as-is")
	return nil
}

func revokeAndDeleteSecret(st *repoAuthStatus) error {
	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not revoke GitLab token via API: %v\n", err)
		}
	case "github":
		fmt.Println("  Remember to revoke the old fine-grained PAT yourself at github.com/settings/tokens.")
	}
	if st.SecretName != "" && podman.SecretExists(st.SecretName) {
		if err := podman.DeleteSecret(st.SecretName); err != nil {
			return fmt.Errorf("cannot delete secret %s: %w", st.SecretName, err)
		}
	}
	return nil
}

// --- Prompts ---------------------------------------------------------------

func promptPlatform(reader *bufio.Reader) (string, error) {
	for {
		fmt.Print("  Platform? [gitlab/github]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("cannot read input: %w", err)
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "gitlab" || line == "github" {
			return line, nil
		}
		fmt.Println("  Please answer 'gitlab' or 'github'.")
	}
}

func promptCreateOrAttach(reader *bufio.Reader) (string, error) {
	for {
		fmt.Print("  Create a new project, or attach to an existing one? [create/attach]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("cannot read input: %w", err)
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "create" || line == "attach" {
			return line, nil
		}
		fmt.Println("  Please answer 'create' or 'attach'.")
	}
}

// promptAndVerifyGitHubPAT collects a fine-grained PAT for a specific
// owner/repo and verifies it against that repo before accepting it — this
// is what surfaces a REST error directly for a typo'd --attach target or a
// PAT with the wrong scope (Spec §9's error cases), since GitHub gives
// devsys no other way to validate either one ahead of time.
func promptAndVerifyGitHubPAT(reader *bufio.Reader, ownerRepo string) (string, error) {
	fmt.Printf("  Create a fine-grained PAT scoped to %s at github.com/settings/tokens?type=beta\n", ownerRepo)
	fmt.Println("  Repository permissions needed (Metadata: Read-only is auto-selected with these):")
	fmt.Println("    Contents:      Read and write  (git push/pull, releases, tags)")
	fmt.Println("    Issues:        Read and write  (issues, comments, milestones)")
	fmt.Println("    Pull requests: Read and write  (create, review, merge)")
	fmt.Println("    Actions:       Read-only        (CI status and logs)")
	fmt.Println("  Do not grant Administration — that's repo settings/collaborators, not covered by any devsys workflow.")
	fmt.Print("  Enter PAT: ")
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("cannot read PAT: %w", err)
	}
	pat := strings.TrimSpace(line)
	if pat == "" {
		return "", fmt.Errorf("PAT cannot be empty")
	}

	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok {
		return "", fmt.Errorf("expected owner/repo, got %q", ownerRepo)
	}
	client := github.NewClient(pat)
	if err := client.GetRepo(owner, repo); err != nil {
		return "", fmt.Errorf("cannot verify %s with this PAT: %w", ownerRepo, err)
	}
	return pat, nil
}

// --- GitLab-specific setup --------------------------------------------------

func setupGitLabRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot read bootstrap GitLab PAT (run 'devsys auth gitlab' first): %w", err)
	}
	glClient := gitlab.NewClient(authGitLabURL, bootstrapPAT)

	var webURL string
	var projectID int
	if mode == "create" {
		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Project name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		projectID, webURL, err = glClient.CreateProject(name)
		if err != nil {
			return "", "", nil, fmt.Errorf("cannot create GitLab project: %w", err)
		}
		fmt.Printf("  -> created %s\n", webURL)
	} else {
		fmt.Print("  GitLab project (owner/repo or URL): ")
		line, _ := reader.ReadString('\n')
		target := strings.TrimSpace(line)
		if target == "" {
			return "", "", nil, fmt.Errorf("GitLab project target cannot be empty")
		}
		projectID, err = glClient.GetProject(target)
		if err != nil {
			return "", "", nil, fmt.Errorf("cannot find GitLab project %s: %w", target, err)
		}
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			webURL = target
		} else {
			webURL = strings.TrimRight(authGitLabURL, "/") + "/" + target
		}
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
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return "", nil, fmt.Errorf("cannot read bootstrap GitLab PAT (run 'devsys auth gitlab' first): %w", err)
	}
	glClient := gitlab.NewClient(authGitLabURL, bootstrapPAT)
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
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return err
	}
	glClient := gitlab.NewClient(authGitLabURL, bootstrapPAT)
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

// --- GitHub-specific setup --------------------------------------------------

func setupGitHubRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, err error) {
	var ownerRepo string
	if mode == "create" {
		bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-github-token")
		if err != nil {
			return "", "", fmt.Errorf("cannot read bootstrap GitHub PAT (run 'devsys auth github' first): %w", err)
		}
		bootstrapClient := github.NewClient(bootstrapPAT)

		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Repo name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		_, fullName, err := bootstrapClient.CreateRepo(name)
		if err != nil {
			return "", "", fmt.Errorf("cannot create GitHub repo: %w", err)
		}
		fmt.Printf("  -> created https://github.com/%s\n", fullName)
		ownerRepo = fullName
	} else {
		fmt.Print("  owner/repo: ")
		line, _ := reader.ReadString('\n')
		ownerRepo = strings.TrimSpace(line)
		if ownerRepo == "" {
			return "", "", fmt.Errorf("owner/repo cannot be empty")
		}
	}

	tokenValue, err = promptAndVerifyGitHubPAT(reader, ownerRepo)
	if err != nil {
		return "", "", err
	}
	remoteURL, err = embedTokenInHTTPSURL("https://github.com/"+ownerRepo, "x-access-token", tokenValue)
	if err != nil {
		return "", "", fmt.Errorf("cannot build remote URL: %w", err)
	}
	fmt.Println("  -> attached, remote wired")
	return remoteURL, tokenValue, nil
}

// --- Shared helpers ---------------------------------------------------------

// embedTokenInHTTPSURL builds an HTTPS remote URL with the bearer token
// embedded as userinfo, e.g. https://oauth2:<TOKEN>@gitlab.com/owner/repo.git
// — the mechanism both platforms now use identically (Git Remote &
// Credential Spec §7's corrected GitHub decision).
func embedTokenInHTTPSURL(rawURL, user, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("cannot parse %q: %w", rawURL, err)
	}
	u.User = url.UserPassword(user, token)
	if !strings.HasSuffix(u.Path, ".git") {
		u.Path += ".git"
	}
	return u.String(), nil
}

// repoDefaultName suggests a platform project/repo name for "create" mode:
// the project name itself for the workspace-root repo, or
// "<project>-<relpath>" for any other repo in a multi-repo project.
func repoDefaultName(projectName, relPath string) string {
	if relPath == "." {
		return projectName
	}
	slug := strings.ReplaceAll(strings.ReplaceAll(relPath, "/", "-"), "\\", "-")
	return projectName + "-" + slug
}

func storeRepoSecret(secretName, value string, labels map[string]string) error {
	if podman.SecretExists(secretName) {
		if err := podman.DeleteSecret(secretName); err != nil {
			return fmt.Errorf("cannot remove existing secret %s: %w", secretName, err)
		}
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels["devsys"] = "true"
	return podman.CreateSecretFromStdin(secretName, value, labels)
}

func writeOriginRemote(repoPath, remoteURL string) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("cannot open repo at %s: %w", repoPath, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("cannot read repo config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = map[string]*gitconfig.RemoteConfig{}
	}
	cfg.Remotes["origin"] = &gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}
	return repo.SetConfig(cfg)
}

func clearOriginRemote(repoPath string) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("cannot open repo at %s: %w", repoPath, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("cannot read repo config: %w", err)
	}
	delete(cfg.Remotes, "origin")
	return repo.SetConfig(cfg)
}

// --- Container recreation ----------------------------------------------------

// recreateContainerForAuth removes and recreates a project's container so
// its newly minted/changed per-repo secrets get mounted — Podman secret
// attachment is creation-time-only (Git Remote & Credential Spec §7).
// Recreation happens once per auth session (the caller only calls this
// after the whole interactive loop finishes), and only asks for
// confirmation if the container is actually running (Spec §9).
func recreateContainerForAuth(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if podman.ContainerIsRunning(containerName) {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("\nThis will recreate %s's container to mount new/changed credentials.\n", containerName)
		fmt.Printf("%s is currently running (someone is inside it via 'devsys enter') — continuing\nwill end that session.", projectName)
		if !confirm(reader, " Proceed?") {
			fmt.Printf("  Skipped — credentials stored but not yet mounted. Run 'devsys auth %s' again, or 'devsys enter %s', to pick them up.\n", projectName, projectName)
			return nil
		}
		if _, err := podman.RunPodman("stop", containerName); err != nil {
			return fmt.Errorf("cannot stop container: %w", err)
		}
	}

	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path: %w", err)
	}
	imageTag := fmt.Sprintf("devsys-%s", projectName)

	if podman.ContainerExists(containerName) {
		if _, err := podman.RunPodman("rm", containerName); err != nil {
			return fmt.Errorf("cannot remove container: %w", err)
		}
	}
	if err := createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag); err != nil {
		return fmt.Errorf("cannot recreate container: %w", err)
	}
	fmt.Printf("  Container %s recreated.\n", containerName)
	return nil
}

// createProjectContainerWithRepoSecrets creates the persistent container
// mounting every per-repo secret this project actually has as a plain file
// (Podman's default mount, /run/secrets/<secret-name>) — not the single
// type=env,target=GITLAB_TOKEN mount cmd/init.go's createProjectContainer
// still uses. Fixes the env-var collision a project with more than one
// per-repo token would otherwise hit (Git Remote & Credential Spec §7's
// multi-token fix). cmd/init.go's own container-creation path is Phase 6's
// job to switch over to this, not yet done.
func createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag string) error {
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	for _, vol := range []string{"devsys-claude-auth", "devsys-codex-auth", trivyVolume} {
		if !podman.VolumeExists(vol) {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", vol); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", vol, err)
			}
		}
	}

	args := []string{
		"create",
		"--name", containerName,
		"--label", "devsys=true",
		"--volume", projectPath + ":" + defaultWorkspaceDest + ":Z",
		"--volume", "devsys-claude-auth:/root/.claude",
		"--volume", "devsys-codex-auth:/root/.codex",
		"--volume", trivyVolume + ":/root/.cache/trivy",
		"--env", "CLAUDE_CONFIG_DIR=/root/.claude",
		"--env", "CODEX_HOME=/root/.codex",
		"--env", "TRIVY_CACHE_DIR=/root/.cache/trivy",
		"--env", "IS_SANDBOX=1",
	}

	secretNames, err := projectRepoSecretNames(projectName)
	if err != nil {
		return fmt.Errorf("cannot list project secrets: %w", err)
	}
	for _, name := range secretNames {
		args = append(args, "--secret", name) // default: mounted as a file at /run/secrets/<name>
	}
	args = append(args, imageTag)

	_, err = podman.RunPodman(args...)
	return err
}

// projectRepoSecretNames returns every per-repo credential secret belonging
// to projectName, matching devsys-<project>-<repo-id>-<platform>-token
// (Git Remote & Credential Spec §7). Excludes the legacy single-secret name
// cmd/init.go still creates (devsys-<project>-gitlab-token /
// devsys-<project>-github-token, no repo-id component) — that one is
// mounted by cmd/init.go's own, still-unreworked container-creation path,
// not this one.
func projectRepoSecretNames(projectName string) ([]string, error) {
	all, err := podman.ListDevsysSecrets()
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("devsys-%s-", projectName)
	legacyGitLab := fmt.Sprintf("devsys-%s-gitlab-token", projectName)
	legacyGitHub := fmt.Sprintf("devsys-%s-github-token", projectName)

	var matched []string
	for _, name := range all {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, "-token") {
			continue
		}
		if name == legacyGitLab || name == legacyGitHub {
			continue
		}
		matched = append(matched, name)
	}
	return matched, nil
}
