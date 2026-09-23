package cmd

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	"github.com/katakalyst/devsys/internal/github"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
)

// --- GitHub-specific setup --------------------------------------------------

// githubHost returns the hostname of the configured GitHub instance —
// "github.com" for standard GitHub, or the GHE hostname when authGitHubURL
// has been set to a self-hosted instance via --github-url or loaded from the
// bootstrap secret by loadGitHubHostFromBootstrap.
func githubHost() string {
	return githubHostFromURL(authGitHubURL)
}

// githubTokensPageURL returns the fine-grained token management page URL for
// the configured GitHub instance — github.com/settings/tokens?type=beta for
// standard GitHub, or <ghe-host>/settings/tokens?type=beta for GHE (GHES
// uses the same path under its own hostname).
func githubTokensPageURL() string {
	return githubHost() + "/settings/tokens?type=beta"
}

// githubManualRevokeReminder formats the standard reminder shown wherever a
// GitHub fine-grained PAT is replaced or removed. devsys never mints these
// (user-created, pasted into `--attach`), so it has no API to revoke them
// either, and GitHub never hands back an ID devsys could deep-link to
// directly — the best it can do is name the exact repo the token was scoped
// to, which is what a fine-grained token's own row on githubTokensPageURL's
// list shows, so the user can pick out the right one without guessing by name.
func githubManualRevokeReminder(remoteURL string) string {
	ownerRepo, err := workspace.PathFromURL(remoteURL)
	if err != nil {
		return "  Remember to revoke the old fine-grained PAT yourself: " + githubTokensPageURL()
	}
	return fmt.Sprintf("  Remember to revoke the old fine-grained PAT for %s yourself: %s", ownerRepo, githubTokensPageURL())
}

// githubBootstrapClient builds a GitHub client from the bootstrap PAT —
// used only for --create's repo-creation step (Spec §7: "For --attach, no
// bootstrap PAT is needed"). Uses the configured GitHub instance (github.com
// or GHE) via authGitHubURL.
func githubBootstrapClient() (*github.Client, error) {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-github-token")
	if err != nil {
		return nil, fmt.Errorf("cannot read bootstrap GitHub PAT (run 'devsys auth github' first): %w", err)
	}
	return github.NewClientWithBase(github.APIBaseForHost(githubHost()), bootstrapPAT), nil
}

func setupGitHubRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	var ownerRepo string
	if mode == "create" {
		bootstrapClient, err := githubBootstrapClient()
		if err != nil {
			return "", "", nil, err
		}

		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Repo name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		_, fullName, err := bootstrapClient.CreateRepo(name)
		if err != nil {
			return "", "", nil, fmt.Errorf("cannot create GitHub repo: %w", err)
		}
		fmt.Printf("  -> created %s/%s\n", strings.TrimRight(authGitHubURL, "/"), fullName)
		ownerRepo = fullName
	} else {
		fmt.Print("  owner/repo: ")
		line, _ := reader.ReadString('\n')
		ownerRepo = strings.TrimSpace(line)
		if ownerRepo == "" {
			return "", "", nil, fmt.Errorf("owner/repo cannot be empty")
		}
	}

	tokenValue, labels, err = promptAndVerifyGitHubPAT(reader, ownerRepo)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(strings.TrimRight(authGitHubURL, "/")+"/"+ownerRepo, "x-access-token", tokenValue)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	fmt.Println("  -> attached, remote wired")
	return remoteURL, tokenValue, labels, nil
}

// setupGitHubRemoteScriptable is setupGitHubRemote's non-interactive
// counterpart: nameOrTarget comes from --name/--attach, the PAT from
// --token, and its expiration from --expires-at — never prompted (Spec §9:
// the scriptable form never prompts).
func setupGitHubRemoteScriptable(projectName string, st *repoAuthStatus, mode, nameOrTarget, token, expiresAt string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	var ownerRepo string
	if mode == "create" {
		bootstrapClient, err := githubBootstrapClient()
		if err != nil {
			return "", "", nil, err
		}
		name := nameOrTarget
		if name == "" {
			name = repoDefaultName(projectName, st.Repo.RelPath)
		}
		_, fullName, err := bootstrapClient.CreateRepo(name)
		if err != nil {
			return "", "", nil, fmt.Errorf("cannot create GitHub repo: %w", err)
		}
		ownerRepo = fullName
	} else {
		ownerRepo = nameOrTarget
		if ownerRepo == "" {
			return "", "", nil, fmt.Errorf("--attach requires a target (owner/repo)")
		}
	}

	if token == "" {
		return "", "", nil, fmt.Errorf("--token is required for GitHub in non-interactive mode")
	}
	if err := verifyGitHubPAT(ownerRepo, token); err != nil {
		return "", "", nil, fmt.Errorf("cannot verify %s with this PAT: %w", ownerRepo, err)
	}
	labels, err = githubExpiresAtLabels(expiresAt)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(strings.TrimRight(authGitHubURL, "/")+"/"+ownerRepo, "x-access-token", token)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, token, labels, nil
}

// verifyGitHubPAT checks a fine-grained PAT against a specific owner/repo —
// the shared core of promptAndVerifyGitHubPAT (interactive) and the
// scriptable GitHub paths, which get the PAT from --token instead of a
// prompt but still need the same verification (Spec §9's error cases).
// Uses the configured GitHub instance (github.com or GHE) via authGitHubURL.
func verifyGitHubPAT(ownerRepo, token string) error {
	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok {
		return fmt.Errorf("expected owner/repo, got %q", ownerRepo)
	}
	return github.NewClientWithBase(github.APIBaseForHost(githubHost()), token).GetRepo(owner, repo)
}

// promptAndVerifyGitHubPAT collects a fine-grained PAT for a specific
// owner/repo and verifies it against that repo before accepting it — this
// is what surfaces a REST error directly for a typo'd --attach target or a
// PAT with the wrong scope (Spec §9's error cases), since GitHub gives
// devsys no other way to validate either one ahead of time. It also asks
// what expiration the user set when creating it on github.com — GitHub's
// API never exposes that back to devsys, so this is self-reported, not
// verified, but it's what lets the existing 30-day expiry warning (already
// working for GitLab, whose expiry devsys mints and therefore knows for
// certain) start working for GitHub too, instead of always silently
// showing "token ok" regardless of how close the real expiry actually is.
func promptAndVerifyGitHubPAT(reader *bufio.Reader, ownerRepo string) (token string, labels map[string]string, err error) {
	fmt.Printf("  Create a fine-grained PAT scoped to %s at %s\n", ownerRepo, githubTokensPageURL())
	fmt.Println("  Repository permissions needed (Metadata: Read-only is auto-selected with these):")
	fmt.Println("    Contents:      Read and write  (git push/pull, releases, tags)")
	fmt.Println("    Issues:        Read and write  (issues, comments, milestones)")
	fmt.Println("    Pull requests: Read and write  (create, review, merge)")
	fmt.Println("    Actions:       Read-only        (CI status and logs)")
	fmt.Println("  Do not grant Administration — that's repo settings/collaborators, not covered by any devsys workflow.")
	fmt.Print("  Enter PAT: ")
	line, readErr := reader.ReadString('\n')
	if readErr != nil && line == "" {
		return "", nil, fmt.Errorf("cannot read PAT: %w", readErr)
	}
	pat := strings.TrimSpace(line)
	if pat == "" {
		return "", nil, fmt.Errorf("PAT cannot be empty")
	}
	if err := verifyGitHubPAT(ownerRepo, pat); err != nil {
		return "", nil, fmt.Errorf("cannot verify %s with this PAT: %w", ownerRepo, err)
	}
	labels, err = promptGitHubExpiresAt(reader)
	if err != nil {
		return "", nil, err
	}
	return pat, labels, nil
}

// promptGitHubExpiresAt asks what expiration the user picked on github.com
// when they created the PAT just entered, and returns it as the same
// devsys.expires-at label GitLab's own minted tokens already carry — one
// label key, one warning threshold, one display path (tokenExpiryText),
// regardless of platform. Blank is accepted and means either "No
// expiration" was chosen on github.com, or the user doesn't know/didn't
// say — either way, devsys still shows "token ok" with no warning for it,
// exactly like before this existed, rather than treating a blank answer as
// an error.
func promptGitHubExpiresAt(reader *bufio.Reader) (map[string]string, error) {
	for {
		fmt.Print("  What expiration did you set for this token on github.com? (YYYY-MM-DD, or blank for \"No expiration\"/unknown): ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("cannot read input: %w", err)
		}
		s := strings.TrimSpace(line)
		if s == "" {
			return nil, nil
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			fmt.Println("  Please enter a date as YYYY-MM-DD, or leave blank.")
			continue
		}
		return map[string]string{"devsys.expires-at": s}, nil
	}
}
