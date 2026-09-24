package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// authCmd groups every credential-provisioning command that used to be
// bundled, one-shot, inside `devsys setup`. Splitting these out makes each
// one independently rerunnable. Claude/Codex auth is intentionally not managed
// here — each agent authenticates itself inside its container on first run, and
// the per-project volumes (devsys-<project>-claude-auth / -codex-auth) persist
// that state (chats, settings, credentials) across container restarts. No host
// seeding needed.
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Bootstrap platform PATs and manage per-repo git credentials",
	Long: `Bootstrap platform PATs and manage per-repo git credentials.

  devsys auth gitlab|github [--force]                          bootstrap subcommands (machine-wide)
  devsys auth <project>                                       interactive per-repo credential listing for a project
  devsys auth <project> [repo] [remote] [platform] --create [--name <name>] [--force]
  devsys auth <project> [repo] [remote] [platform] --attach <owner/repo-or-url> [--force]
  devsys auth <project> [repo] [remote] --rotate              scriptable rotate
  devsys auth <project> [repo] [remote] --remove              scriptable remove
  devsys auth <project> [repo] [remote]                       scriptable ensure — only valid if the repo/remote already has a remote

  [remote] is required only once the selected repo has more than one remote — a repo with just one
  (the common case) never needs it, same optionality rule as [repo] one level up.

  --token is required for any GitHub operation above (create/attach/rotate/ensure) — GitHub fine-grained
  PATs can't be created via API, so scriptable mode can't prompt for one the way the interactive form does.
  --expires-at (optional) records the expiration you set for that PAT on github.com, since GitHub never
  exposes it back to devsys — without it, devsys enter's 30-day expiry warning has nothing to warn from.`,
	// RunE handles the "devsys auth <project>" form — anything whose first
	// argument isn't one of the bootstrap subcommand names above (Git Remote
	// & Credential Spec §9).
	RunE: runAuthProject,
}

var authGitLabForce bool
var authGitHubForce bool

// authGitHubURL is the base URL used for all GitHub API operations and remote
// URL construction. Defaults to https://github.com; set to a GHE instance URL
// via --github-url on `devsys auth github` (stored as a secret label so
// subsequent commands pick it up automatically) or on `devsys auth <project>`
// for per-invocation override. Populated at startup by loadGitHubHostFromBootstrap.
var authGitHubURL = "https://github.com"

// agentAuthVolumeName returns the name of a project's per-agent data volume
// (devsys-<project>-<agent>-auth). Each project gets its own volume so chats,
// settings, and credentials are isolated per project and persist across
// container restarts. The agent authenticates itself inside the container on
// first run — no host seeding.
func agentAuthVolumeName(projectName, agent string) string {
	return fmt.Sprintf("devsys-%s-%s-auth", projectName, agent)
}

var authGitLabCmd = &cobra.Command{
	Use:   "gitlab",
	Short: "Set or replace the bootstrap GitLab PAT devsys uses to create projects and mint tokens",
	RunE:  runAuthGitLab,
}

var authGitHubCmd = &cobra.Command{
	Use:   "github",
	Short: "Set or replace the bootstrap GitHub PAT devsys uses to create repos",
	RunE:  runAuthGitHub,
}

func init() {
	authGitLabCmd.Flags().BoolVar(&authGitLabForce, "force", false, "Replace the existing bootstrap PAT secret")
	authGitHubCmd.Flags().BoolVar(&authGitHubForce, "force", false, "Replace the existing bootstrap PAT secret")
	// --github-url is a PersistentFlag on authCmd so it is available to all
	// auth subcommands — in particular `auth github` (to store the GHE URL
	// as a secret label) and `auth <project>` (to use it for API/remote ops).
	// Note: cobra only calls the innermost PersistentPreRun in the chain; if a
	// subcommand ever adds its own PersistentPreRun it must call
	// loadGitHubHostFromBootstrap() itself.
	authCmd.PersistentFlags().StringVar(&authGitHubURL, "github-url", "https://github.com",
		"GitHub base URL for repo operations (self-hosted GHE supported; stored when passed to 'auth github')")
	authCmd.Flags().StringVar(&authGitLabURL, "gitlab-url", "https://gitlab.com",
		"GitLab base URL to create/attach repos against (self-hosted instances supported)")
	authCmd.Flags().BoolVar(&authScriptCreate, "create", false, "Create a new platform repo for the given/only repo and wire it (scriptable)")
	authCmd.Flags().StringVar(&authScriptAttach, "attach", "", "Attach the given/only repo to an existing owner/repo or URL (scriptable)")
	authCmd.Flags().BoolVar(&authScriptRotate, "rotate", false, "Revoke and mint a fresh token for the given/only repo (scriptable)")
	authCmd.Flags().BoolVar(&authScriptRemove, "remove", false, "Revoke and unmount the token for the given/only repo, remote left as-is (scriptable)")
	authCmd.Flags().StringVar(&authScriptName, "name", "", "Platform project/repo name for --create (default: derived from the project/repo path)")
	authCmd.Flags().StringVar(&authScriptToken, "token", "", "GitHub fine-grained PAT — required for any GitHub operation in scriptable mode, never prompted")
	authCmd.Flags().StringVar(&authScriptExpiresAt, "expires-at", "", "Expiration (YYYY-MM-DD) the --token PAT was given on github.com, self-reported since GitHub's API never exposes it — optional, blank means \"No expiration\"/unknown; GitLab ignores this, its expiry is always known from minting")
	authCmd.Flags().BoolVar(&authScriptForce, "force", false, "Allow --create/--attach to replace an already-configured repo's credential")

	authCmd.AddCommand(authGitLabCmd)
	authCmd.AddCommand(authGitHubCmd)
}

// loadGitHubHostFromBootstrap reads the GitHub base URL stored as a label on
// the bootstrap PAT secret (written there by `devsys auth github --github-url`)
// and applies it to authGitHubURL if the flag was not explicitly overridden,
// then registers the resolved host with workspace so PlatformFromURL
// classifies GHE remote URLs correctly everywhere (init, rebuild, enter, auth).
//
// Called from rootCmd.PersistentPreRun — runs before every command.
// Silently skips when the bootstrap secret doesn't exist yet.
func loadGitHubHostFromBootstrap() {
	labels, err := podman.GetSecretLabels("devsys-bootstrap-github-token")
	if err != nil {
		return // secret not set yet, or podman unavailable — skip
	}
	storedURL := labels["devsys.github-url"]
	if storedURL == "" {
		return
	}
	// Apply only when the flag was left at its default value; an explicit
	// --github-url flag on the command line always takes precedence.
	if authGitHubURL == "https://github.com" {
		authGitHubURL = storedURL
	}
	// Register whatever host won — a no-op for github.com (already in the set).
	if h := githubHostFromURL(authGitHubURL); h != "github.com" {
		workspace.RegisterGitHubHost(h)
	}
}

// githubHostFromURL extracts the hostname from a GitHub base URL.
// Returns "github.com" on any parse failure.
func githubHostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "github.com"
	}
	return strings.ToLower(u.Hostname())
}

func runAuthGitHub(cmd *cobra.Command, args []string) error {
	const bootstrapSecretName = "devsys-bootstrap-github-token"

	if podman.SecretExists(bootstrapSecretName) {
		if !authGitHubForce {
			fmt.Println("Bootstrap GitHub PAT secret already exists. Pass --force to replace it.")
			return nil
		}
		fmt.Println("Replacing existing bootstrap GitHub PAT...")
		if err := podman.DeleteSecret(bootstrapSecretName); err != nil {
			return fmt.Errorf("cannot remove existing secret: %w", err)
		}
	}

	fmt.Print("Enter bootstrap GitHub PAT (input hidden): ")
	patBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("cannot read PAT: %w", err)
	}
	pat := strings.TrimSpace(string(patBytes))
	if pat == "" {
		return fmt.Errorf("bootstrap PAT cannot be empty")
	}

	// Store the configured GitHub URL as a label so every subsequent devsys
	// command can load the GHE host automatically via loadGitHubHostFromBootstrap,
	// without requiring --github-url on every invocation.
	labels := map[string]string{
		"devsys":            "true",
		"devsys.github-url": authGitHubURL,
	}
	if err := podman.CreateSecretFromStdin(bootstrapSecretName, pat, labels); err != nil {
		return fmt.Errorf("cannot store bootstrap PAT: %w", err)
	}
	fmt.Printf("  Stored secret %s.\n", bootstrapSecretName)
	return nil
}

func runAuthGitLab(cmd *cobra.Command, args []string) error {
	const bootstrapSecretName = "devsys-bootstrap-gitlab-token"

	if podman.SecretExists(bootstrapSecretName) {
		if !authGitLabForce {
			fmt.Println("Bootstrap GitLab PAT secret already exists. Pass --force to replace it.")
			return nil
		}
		fmt.Println("Replacing existing bootstrap GitLab PAT...")
		if err := podman.DeleteSecret(bootstrapSecretName); err != nil {
			return fmt.Errorf("cannot remove existing secret: %w", err)
		}
	}

	fmt.Print("Enter bootstrap GitLab PAT (input hidden): ")
	patBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("cannot read PAT: %w", err)
	}
	pat := strings.TrimSpace(string(patBytes))
	if pat == "" {
		return fmt.Errorf("bootstrap PAT cannot be empty")
	}

	labels := map[string]string{"devsys": "true"}
	if err := podman.CreateSecretFromStdin(bootstrapSecretName, pat, labels); err != nil {
		return fmt.Errorf("cannot store bootstrap PAT: %w", err)
	}
	fmt.Printf("  Stored secret %s.\n", bootstrapSecretName)
	return nil
}
