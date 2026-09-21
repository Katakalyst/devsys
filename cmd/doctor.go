package cmd

import (
	"fmt"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Verify the devsys installation and environment",
	RunE:  runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	allOK := true
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("  [OK]   %s\n", name)
		} else {
			fmt.Printf("  [FAIL] %s: %s\n", name, detail)
			allOK = false
		}
	}

	fmt.Println("Running doctor checks...")
	fmt.Println()

	// Check 1: Podman is present and working.
	podmanOut, err := podman.RunPodman("--version")
	if err != nil {
		check("Podman present", false, err.Error())
	} else {
		check("Podman present", true, strings.TrimSpace(podmanOut))
	}

	// Check 2: Bootstrap PAT secret exists.
	bootstrapExists := podman.SecretExists("devsys-bootstrap-gitlab-token")
	check("Bootstrap GitLab PAT secret", bootstrapExists, "run 'devsys auth gitlab' to set it")

	// Check 3: Bootstrap PAT is readable (valid format check).
	if bootstrapExists {
		pat, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
		patValid := err == nil && len(pat) > 10
		if err != nil {
			check("Bootstrap PAT readable", false, err.Error())
		} else {
			check("Bootstrap PAT readable", patValid, "secret appears empty or malformed")
		}
	}

	// Check 4: Claude/Codex auth volumes actually hold credentials, not just
	// exist — a volume can exist empty (e.g. auto-created by
	// createProjectContainer) without ever having been authenticated.
	claudeAuthed := podman.VolumeExists("devsys-claude-auth") && volumeHasContent("devsys-claude-auth")
	check("Claude Code auth", claudeAuthed, "run 'devsys auth claude', or log in from inside 'devsys enter'")
	codexAuthed := podman.VolumeExists("devsys-codex-auth") && volumeHasContent("devsys-codex-auth")
	check("Codex auth", codexAuthed, "run 'devsys auth codex', or log in from inside 'devsys enter'")

	// Check 5: devsys-base's registry location is reachable and publishes at
	// least one discoverable version. devsysBaseImage has no tag (Section
	// 12.3), so this is a live registry reachability check, not a local
	// image-presence check — a more meaningful signal anyway, since each
	// project pins its own version rather than sharing one cached tag.
	_, err = registry.LatestTag(devsysBaseImage)
	check("devsys-base reachable", err == nil, fmt.Sprintf("cannot reach %s (%v)", devsysBaseImage, err))

	fmt.Println()
	if allOK {
		fmt.Println("All checks passed.")
	} else {
		fmt.Println("Some checks failed — see above for details.")
	}
	return nil
}
