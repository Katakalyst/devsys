package cmd

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init <path> <name>",
	Short: "Initialise a project directory as a devsys project",
	Args:  cobra.ExactArgs(2),
	RunE:  runInit,
}

// runInit implements Git Remote & Credential Spec §8's "devsys init <path>
// <name>": no forced platform, no forced remote detection — path and name
// are two separate, explicit inputs rather than the name being derived from
// the folder's basename (spec §7's decided bullet). Repo discovery,
// creation, and per-repo auth all reuse the exact same listing/selection
// mechanism `devsys auth <project>` uses (cmd/auth_project.go), not a
// separate implementation.
func runInit(cmd *cobra.Command, args []string) error {
	projectPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("cannot resolve path: %w", err)
	}
	projectName := args[1]

	// Step 1: Create the workspace folder.
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", projectPath, err)
	}
	fmt.Printf("Project path: %s\n", projectPath)

	// Steps 2-4: discovery, repeatable "create a new repo here", and
	// inline per-repo auth — before the container ever exists, so
	// everything gets wired in from the start with no recreation cost.
	reader := bufio.NewReader(os.Stdin)
	repoSetupChanged, err := runInitRepoSetup(reader, projectName, projectPath)
	if err != nil {
		return err
	}

	// Step 5: Generate .devsys/Containerfile if absent, tracking
	// devsys-base:latest — a floating tag, not a pinned version. buildImage's
	// --pull=newer re-pulls it whenever the registry has something newer, so
	// a project picks up new base releases automatically on its next
	// rebuild, with no Containerfile commit needed (devsys CLI Spec, Section
	// 12.4, corrected: this used to pin to the current newest version and
	// explicitly never float).
	containerfilePath := filepath.Join(projectPath, ".devsys", "Containerfile")
	if _, err := os.Stat(containerfilePath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(containerfilePath), 0o755); err != nil {
			return fmt.Errorf("cannot create .devsys directory: %w", err)
		}
		baseRef := devsysBaseImage + ":latest"
		containerfileContent := fmt.Sprintf("FROM %s\n", baseRef)
		if err := os.WriteFile(containerfilePath, []byte(containerfileContent), 0o644); err != nil {
			return fmt.Errorf("cannot write Containerfile: %w", err)
		}
		fmt.Printf("  Generated %s (FROM %s).\n", containerfilePath, baseRef)
	} else {
		fmt.Printf("  %s already exists — skipping.\n", containerfilePath)
	}

	// Step 6: Build the per-project image and create the persistent
	// container, mounting whatever per-repo credentials steps 2-4 wired up
	// (createProjectContainerWithRepoSecrets attempts every secret this
	// project actually has and mounts each as a file — R14's "attempt
	// everything, skip what's missing").
	imageTag := fmt.Sprintf("devsys-%s", projectName)
	fmt.Printf("Building image %s ...\n", imageTag)
	if err := buildImage(imageTag, containerfilePath, projectPath); err != nil {
		return fmt.Errorf("cannot build image: %w", err)
	}
	fmt.Printf("  Image %s built.\n", imageTag)

	containerName := fmt.Sprintf("devsys-%s", projectName)
	if podman.ContainerExists(containerName) {
		fmt.Printf("Container %s already exists — skipping creation.\n", containerName)
		if repoSetupChanged {
			// Podman secret attachment is creation-time-only (Git Remote &
			// Credential Spec §7) — credentials configured just now went
			// into Podman secrets, but this run never touches the existing
			// container, so they aren't mounted yet. `auth` is the rerunnable
			// command for this, not re-running `init`.
			fmt.Printf("  New/changed credentials were stored but not yet mounted — run 'devsys auth %s' to pick them up.\n", projectName)
		}
	} else {
		fmt.Printf("Creating container %s ...\n", containerName)
		if err := createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag); err != nil {
			return fmt.Errorf("cannot create container: %w", err)
		}
		fmt.Printf("  Container %s created.\n", containerName)
	}

	fmt.Printf("\nProject '%s' is ready. Run 'devsys enter %s' to start.\n", projectName, projectName)
	return nil
}

// runInitRepoSetup is init's own front-loaded proactive-setup loop (Spec
// §7's proactive/reactive split): the same repo listing and per-repo
// configure/menu logic `devsys auth <project>` uses (gatherRepoStatuses,
// printRepoListing, parseSelection, configureRepo — all in
// cmd/auth_project.go), plus the one thing only `init` offers: creating a
// brand-new local repo, repeatable, any number, each at its own path (Spec
// §7/§8/§9's "[n] create a new repo here" — a genuinely brand-new project
// has nothing for discovery to find, which is exactly the gap this fills).
func runInitRepoSetup(reader *bufio.Reader, projectName, workspaceRoot string) (changed bool, err error) {
	fmt.Println("Scanning for existing repos...")
	for {
		statuses, err := gatherRepoStatuses(projectName, workspaceRoot)
		if err != nil {
			return changed, err
		}

		if len(statuses) == 0 {
			fmt.Println("  none found.")
			fmt.Println()
			fmt.Println("  (none yet)")
		} else {
			fmt.Println()
			printRepoListing(statuses)
		}
		fmt.Print("\n  [1,2,...] select to configure   [n] create a new repo here   [d] done, continue\n\n> ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		switch {
		case line == "" || strings.EqualFold(line, "d"):
			return changed, nil
		case strings.EqualFold(line, "n"):
			if err := createLocalRepo(reader, workspaceRoot); err != nil {
				fmt.Printf("  Error: %v\n", err)
			}
		default:
			indices, parseErr := parseSelection(line, len(statuses))
			if parseErr != nil {
				fmt.Printf("  %v\n", parseErr)
				continue
			}
			for _, idx := range indices {
				didChange, configErr := configureRepo(reader, projectName, &statuses[idx])
				if configErr != nil {
					fmt.Printf("  Error: %v\n", configErr)
					continue
				}
				if didChange {
					changed = true
				}
			}
		}
	}
}

// createLocalRepo does a purely local, host-side `git init` at a
// user-chosen path within the workspace — no credentials needed at all
// (Spec §7: "local repo creation... needs no credentials at all... just a
// default" for `init` to also do it as a convenience). A blank answer means
// the workspace root itself, matching the spec's own mockup.
func createLocalRepo(reader *bufio.Reader, workspaceRoot string) error {
	fmt.Print("  Path within workspace (blank = root): ")
	line, _ := reader.ReadString('\n')
	relPath := strings.TrimSpace(line)
	if relPath == "" {
		relPath = "."
	}

	repoPath, err := resolveWorkspacePath(workspaceRoot, relPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return fmt.Errorf("cannot create directory: %w", err)
	}
	if _, err := gogit.PlainInit(repoPath, false); err != nil {
		if err == gogit.ErrRepositoryAlreadyExists {
			return fmt.Errorf("a repo already exists at %s", relPath)
		}
		return fmt.Errorf("cannot initialise git repository: %w", err)
	}
	fmt.Printf("Created empty repo at %s\n", relPath)
	return nil
}

// resolveWorkspacePath validates that relPath, joined onto workspaceRoot,
// actually stays within workspaceRoot — the "Path within workspace" prompt
// promises this, but filepath.Join alone doesn't enforce it: it cleans ".."
// segments syntactically without checking where the cleaned result actually
// lands, so relPath could still walk the result outside workspaceRoot
// (documents/TODO.md). Rejects an absolute relPath outright — the prompt is
// for a path *within* the workspace, never an arbitrary host path — and any
// relative path that escapes workspaceRoot once cleaned.
func resolveWorkspacePath(workspaceRoot, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("path must be relative to the workspace, not absolute: %q", relPath)
	}

	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("cannot resolve workspace root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	joined := filepath.Join(absRoot, relPath)
	if joined != absRoot && !strings.HasPrefix(joined, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", relPath)
	}
	return joined, nil
}

func buildImage(tag, containerfile, contextPath string) error {
	data, err := os.ReadFile(containerfile)
	if err != nil {
		return fmt.Errorf("cannot read Containerfile: %w", err)
	}
	cfHash := fmt.Sprintf("%x", sha256.Sum256(data))
	portsHash := portsFileHash(contextPath)
	// RunPodmanLiveCapturingStderr streams build output to the terminal in
	// real time while still capturing stderr for the auth hint check on failure.
	stderr, err := podman.RunPodmanLiveCapturingStderr("build", "-t", tag,
		"--pull=newer",
		"--label", "devsys=true",
		"--label", "devsys.containerfile-hash="+cfHash,
		"--label", "devsys.ports-hash="+portsHash,
		"-f", containerfile, contextPath)
	if err != nil {
		// Every project Containerfile starts "FROM ghcr.io/.../devsys-base:...",
		// so a build can fail the same way a direct pull can: a stale stored
		// ghcr.io login blocking the base layer's otherwise-anonymous pull.
		if hint := podman.RegistryAuthHint(imageHost(devsysBaseImage), stderr); hint != "" {
			return fmt.Errorf("podman build: %w\n%s", err, hint)
		}
		return fmt.Errorf("podman build: %w", err)
	}
	return nil
}

// defaultWorkspaceDest is the in-container path the project workspace is
// bind-mounted to. The container always runs as root — no project
// Containerfile changes that — so this is a fixed constant, not something
// derived per-project (devsys CLI Spec, Section 7; KNOWN_ISSUES.md Issue 9).
const defaultWorkspaceDest = "/root/workspace"
