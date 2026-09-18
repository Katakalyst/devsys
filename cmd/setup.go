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

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Initial machine-level setup for devsys",
	Long: `Checks Podman is present, pulls the devsys-base image, optionally seeds
Claude/Codex auth volumes from the host, prompts for a bootstrap GitLab PAT,
and stores it as a Podman secret. Runs doctor checks at the end.`,
	RunE: runSetup,
}

func runSetup(cmd *cobra.Command, args []string) error {
	// Step 1: Check Podman.
	fmt.Println("Checking Podman...")
	out, err := podman.RunPodman("info", "--format", "{{.Host.OS}}")
	if err != nil {
		return fmt.Errorf("Podman is not available or not working: %w", err)
	}
	fmt.Printf("  Podman OK (%s)\n", strings.TrimSpace(out))

	// Step 2: Look up the current newest devsys-base version and pull it
	// (devsysBaseImage is a location only, never itself pullable — devsys
	// CLI Spec, Section 12.3).
	fmt.Printf("Checking %s for the newest version...\n", devsysBaseImage)
	baseRef, err := registry.LatestReference(devsysBaseImage)
	if err != nil {
		return fmt.Errorf("cannot look up newest devsys-base version: %w", err)
	}
	fmt.Printf("Pulling %s ...\n", baseRef)
	if err := podman.RunPodmanLive("pull", baseRef); err != nil {
		return fmt.Errorf("cannot pull devsys-base image: %w", err)
	}
	fmt.Println("  Image pulled.")

	// Step 3: Optionally seed Claude/Codex auth volumes.
	home, _ := os.UserHomeDir()
	reader := bufio.NewReader(os.Stdin)
	if err := seedAuthVolume(baseRef, reader, "devsys-claude-auth", filepath.Join(home, ".claude")); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
	if err := seedAuthVolume(baseRef, reader, "devsys-codex-auth", filepath.Join(home, ".codex")); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}

	// Step 4: Prompt for bootstrap GitLab PAT.
	const bootstrapSecretName = "devsys-bootstrap-gitlab-token"
	if podman.SecretExists(bootstrapSecretName) {
		fmt.Println("Bootstrap GitLab PAT secret already exists — skipping.")
	} else {
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
	}

	// Step 5: Run doctor checks.
	fmt.Println()
	return runDoctor(cmd, args)
}

// seedAuthVolume copies a host directory into a named volume, but only on
// first use (i.e. the volume did not already exist). Re-seeding an existing
// volume would silently discard any refreshed credentials the container has
// written back since the last seed.
func seedAuthVolume(baseRef string, reader *bufio.Reader, volumeName, hostDir string) error {
	info, err := os.Stat(hostDir)
	if os.IsNotExist(err) || (err == nil && !info.IsDir()) {
		// Nothing on the host to seed from; volume will be created (labelled)
		// by createProjectContainer on first init.
		return nil
	}
	if err != nil {
		// Unexpected stat error — warn but don't abort setup.
		fmt.Fprintf(os.Stderr, "Warning: cannot stat %s: %v\n", hostDir, err)
		return nil
	}

	// Ask the user.
	if !confirm(reader, fmt.Sprintf("Found %s on host. Seed into volume %s?", hostDir, volumeName)) {
		fmt.Printf("  Skipping %s.\n", volumeName)
		// Still ensure the volume exists with the correct label so it shows up
		// in `devsys list` and isn't auto-created unlabelled later.
		if !podman.VolumeExists(volumeName) {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", volumeName); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", volumeName, err)
			}
		}
		return nil
	}

	// If the volume already exists it has content from a previous seed — skip
	// rather than overwrite, to avoid discarding refreshed credentials.
	if podman.VolumeExists(volumeName) {
		fmt.Printf("  Volume %s already exists — skipping to avoid overwrite.\n", volumeName)
		return nil
	}

	// Volume doesn't exist yet: create it with the label, then seed.
	if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", volumeName); err != nil {
		return fmt.Errorf("cannot create volume %s: %w", volumeName, err)
	}

	fmt.Printf("  Seeding %s into %s ...\n", hostDir, volumeName)
	if err := podman.RunPodmanLive(
		"run", "--rm",
		"--volume", hostDir+":/src:ro",
		"--volume", volumeName+":/dst",
		"--entrypoint", "sh",
		baseRef,
		"-c", "cp -a /src/. /dst/",
	); err != nil {
		return fmt.Errorf("cannot seed volume %s: %w", volumeName, err)
	}
	fmt.Printf("  Seeded %s.\n", volumeName)
	return nil
}
