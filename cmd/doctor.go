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

	// Check 4: each project's own Claude/Codex auth volumes actually hold
	// credentials, not just exist — a volume can exist empty (e.g.
	// auto-created by createProjectContainerWithRepoSecrets) without ever
	// having been authenticated. Per-project (documents/TODO.md's
	// per-project agent credential item), so this iterates every discovered
	// project rather than checking one machine-wide pair of volumes.
	containers, err := podman.ListDevsysContainers()
	if err != nil {
		check("Claude/Codex auth", false, fmt.Sprintf("cannot list projects: %v", err))
	} else if len(containers) == 0 {
		fmt.Println("  [--]   Claude/Codex auth: no projects yet")
	} else {
		for _, c := range containers {
			projectName := containerToProject(containerField(c, "Names"))
			for _, agent := range []string{"claude", "codex"} {
				volumeName := agentAuthVolumeName(projectName, agent)
				authed := podman.VolumeExists(volumeName) && volumeHasContent(volumeName)
				check(fmt.Sprintf("%s auth (%s)", agent, projectName), authed,
					fmt.Sprintf("run 'devsys auth %s %s', or log in from inside 'devsys enter %s'", agent, projectName, projectName))
			}
		}
	}

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
