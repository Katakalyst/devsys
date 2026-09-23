package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// authCmd groups every credential-provisioning command that used to be
// bundled, one-shot, inside `devsys setup`. Splitting these out makes each
// one independently rerunnable — most importantly for Claude/Codex, where
// there was previously no way back into the shared auth volume once it had
// been seeded once (e.g. to switch from a subscription login to an API key,
// or to reauthenticate after a logout).
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage shared Claude/Codex credentials, bootstrap platform PATs, and per-repo git credentials",
	Long: `Manage shared Claude/Codex credentials, bootstrap platform PATs, and per-repo git credentials.

  devsys auth claude|codex|gitlab|github [--force]   bootstrap subcommands, above
  devsys auth <project>                              interactive per-repo credential listing for a project
  devsys auth <project> [repo] [platform] --create [--name <name>] [--force]
  devsys auth <project> [repo] [platform] --attach <owner/repo-or-url> [--force]
  devsys auth <project> [repo] --rotate              scriptable rotate
  devsys auth <project> [repo] --remove              scriptable remove
  devsys auth <project> [repo]                       scriptable ensure — only valid if repo already has a remote

  --token is required for any GitHub operation above (create/attach/rotate/ensure) — GitHub fine-grained
  PATs can't be created via API, so scriptable mode can't prompt for one the way the interactive form does.`,
	// RunE handles the "devsys auth <project>" form — anything whose first
	// argument isn't one of the bootstrap subcommand names above (Git Remote
	// & Credential Spec §9).
	RunE: runAuthProject,
}

var authClaudeForce bool
var authCodexForce bool
var authGitLabForce bool
var authGitHubForce bool

var authClaudeCmd = &cobra.Command{
	Use:   "claude",
	Short: "Seed or reseed the shared Claude Code credential volume",
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		return authSeedAgentVolume("devsys-claude-auth", filepath.Join(home, ".claude"), authClaudeForce)
	},
}

var authCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Seed or reseed the shared Codex credential volume",
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		return authSeedAgentVolume("devsys-codex-auth", filepath.Join(home, ".codex"), authCodexForce)
	},
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
	authClaudeCmd.Flags().BoolVar(&authClaudeForce, "force", false, "Reseed even if the volume already has credentials, overwriting them")
	authCodexCmd.Flags().BoolVar(&authCodexForce, "force", false, "Reseed even if the volume already has credentials, overwriting them")
	authGitLabCmd.Flags().BoolVar(&authGitLabForce, "force", false, "Replace the existing bootstrap PAT secret")
	authGitHubCmd.Flags().BoolVar(&authGitHubForce, "force", false, "Replace the existing bootstrap PAT secret")
	authCmd.Flags().StringVar(&authGitLabURL, "gitlab-url", "https://gitlab.com",
		"GitLab base URL to create/attach repos against (self-hosted instances supported)")
	authCmd.Flags().BoolVar(&authScriptCreate, "create", false, "Create a new platform repo for the given/only repo and wire it (scriptable)")
	authCmd.Flags().StringVar(&authScriptAttach, "attach", "", "Attach the given/only repo to an existing owner/repo or URL (scriptable)")
	authCmd.Flags().BoolVar(&authScriptRotate, "rotate", false, "Revoke and mint a fresh token for the given/only repo (scriptable)")
	authCmd.Flags().BoolVar(&authScriptRemove, "remove", false, "Revoke and unmount the token for the given/only repo, remote left as-is (scriptable)")
	authCmd.Flags().StringVar(&authScriptName, "name", "", "Platform project/repo name for --create (default: derived from the project/repo path)")
	authCmd.Flags().StringVar(&authScriptToken, "token", "", "GitHub fine-grained PAT — required for any GitHub operation in scriptable mode, never prompted")
	authCmd.Flags().BoolVar(&authScriptForce, "force", false, "Allow --create/--attach to replace an already-configured repo's credential")

	authCmd.AddCommand(authClaudeCmd)
	authCmd.AddCommand(authCodexCmd)
	authCmd.AddCommand(authGitLabCmd)
	authCmd.AddCommand(authGitHubCmd)
}

// authSeedAgentVolume seeds volumeName from hostDir, the same one-time-copy
// mechanism `devsys setup` used to perform inline (Container Architecture
// Spec, Section 5.3 — a seed, never a live mount). Unlike the old inline
// version, this is meant to be rerun: with --force it clears and re-copies
// even if the volume already has content, which is the actual mechanism for
// switching credentials (e.g. subscription login -> API key) rather than
// only ever seeding once at machine bootstrap.
func authSeedAgentVolume(volumeName, hostDir string, force bool) error {
	reader := bufio.NewReader(os.Stdin)

	info, statErr := os.Stat(hostDir)
	hostHasCreds := statErr == nil && info.IsDir()

	exists := podman.VolumeExists(volumeName)
	hasContent := exists && volumeHasContent(volumeName)

	if hasContent && !force {
		fmt.Printf("%s already has credentials.\n", volumeName)
		fmt.Println("  Pass --force to reseed from the host, or log in again from inside any 'devsys enter' session.")
		return nil
	}

	if !hostHasCreds {
		if !exists {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", volumeName); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", volumeName, err)
			}
		}
		fmt.Printf("No existing credentials found at %s.\n", hostDir)
		fmt.Println("  Nothing to seed — log in from inside any 'devsys enter' session instead; the first run there does a normal in-container login.")
		return nil
	}

	prompt := fmt.Sprintf("Seed %s into volume %s?", hostDir, volumeName)
	if hasContent {
		prompt = fmt.Sprintf("Overwrite %s's existing credentials from %s?", volumeName, hostDir)
	}
	if !confirm(reader, prompt) {
		fmt.Println("  Skipped.")
		return nil
	}

	if !exists {
		if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", volumeName); err != nil {
			return fmt.Errorf("cannot create volume %s: %w", volumeName, err)
		}
	}

	baseRef, err := registry.LatestReference(devsysBaseImage)
	if err != nil {
		return fmt.Errorf("cannot look up newest devsys-base version: %w", err)
	}
	fmt.Printf("Pulling %s ...\n", baseRef)
	if err := podman.RunPodmanLive("pull", baseRef); err != nil {
		return fmt.Errorf("cannot pull devsys-base image: %w", err)
	}

	fmt.Printf("Seeding %s into %s ...\n", hostDir, volumeName)
	if err := podman.RunPodmanLive(
		"run", "--rm",
		"--volume", hostDir+":/src:ro",
		"--volume", volumeName+":/dst",
		"--entrypoint", "sh",
		baseRef,
		// Clear first so a --force reseed fully replaces stale credential
		// files rather than merging old and new (the actual mechanism for
		// "switch this volume to a different credential").
		"-c", "rm -rf /dst/.[!.]* /dst/* 2>/dev/null; cp -a /src/. /dst/",
	); err != nil {
		return fmt.Errorf("cannot seed volume %s: %w", volumeName, err)
	}
	fmt.Printf("  Seeded %s.\n", volumeName)
	return nil
}

// volumeHasContent reports whether volumeName already holds any files, by
// running a disposable container against the current devsys-base image.
// Podman auto-creates a named volume empty the first time it's referenced
// (e.g. by createProjectContainer), so VolumeExists alone can't distinguish
// "never authenticated" from "authenticated" — this can. Used by both the
// auth commands (to decide whether a reseed needs --force/confirmation) and
// doctor/enter's "no auth set" checks.
func volumeHasContent(volumeName string) bool {
	baseRef, err := registry.LatestReference(devsysBaseImage)
	if err != nil {
		// Can't check — assume content is present so callers don't
		// needlessly warn or overwrite on a transient registry failure.
		return true
	}
	out, err := podman.RunPodman(
		"run", "--rm",
		"--volume", volumeName+":/data:ro",
		"--entrypoint", "sh",
		baseRef,
		"-c", "ls -A /data 2>/dev/null",
	)
	return err == nil && strings.TrimSpace(out) != ""
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

	labels := map[string]string{"devsys": "true"}
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
