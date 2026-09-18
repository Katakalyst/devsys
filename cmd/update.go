package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/registry"
	"github.com/spf13/cobra"
)

// currentVersion is the devsys CLI's own version. Overridden at release-build
// time via:
//
//	go build -ldflags "-X github.com/katakalyst/devsys/cmd.currentVersion=1.2.3"
//
// "dev" is the value in any build that skips that flag (go run, a plain
// go build) and is treated as unversioned rather than compared against a
// real release tag.
var currentVersion = "dev"

// releaseRepo is the single source of truth for where devsys itself is
// released from (devsys CLI Spec, Section 12.6 — hosting is a confirmed
// decision: GitHub, github.com/katakalyst/devsys). releasesAPIURL and every
// release-asset download URL are derived from it.
var releaseRepo = "katakalyst/devsys"

// releasesAPIURL is a var, not a const, so tests can point it at a local
// httptest server instead of hitting the real GitHub API.
var releasesAPIURL = "https://api.github.com/repos/" + releaseRepo + "/releases/latest"

// releaseDownloadBaseURL is a var for the same reason — tests point it at a
// local httptest server instead of github.com.
var releaseDownloadBaseURL = "https://github.com/" + releaseRepo + "/releases/latest/download"

var updateAll bool

var updateCmd = &cobra.Command{
	Use:   "update [project]",
	Short: "Update devsys itself, or a project's devsys-base version",
	Long: `With no argument: updates devsys itself in place.
With a project name (or --all): updates that project's (or every project's)
devsys-base version. These are two entirely separate behaviors — never both
at once (devsys CLI Spec, Section 12.2).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateAll, "all", false, "Update every project's devsys-base version")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	if len(args) == 0 && !updateAll {
		return updateCLI()
	}
	return updateBaseImages(args, updateAll)
}

// ---------------------------------------------------------------------------
// devsys CLI self-update (devsys CLI Spec, Section 12.2 — no argument)
// ---------------------------------------------------------------------------

func updateCLI() error {
	fmt.Printf("CLI version:    %s\n", currentVersion)
	if currentVersion == "dev" {
		fmt.Println("  Running a development build — self-update skipped.")
		return nil
	}

	latest, err := latestReleaseVersion()
	if err != nil {
		return fmt.Errorf("cannot check latest devsys version: %w", err)
	}
	fmt.Printf("Latest version: %s\n", latest)
	if currentVersion == latest {
		fmt.Println("  devsys is already up to date.")
		return nil
	}

	fmt.Printf("Updating devsys %s -> %s ...\n", currentVersion, latest)
	if err := selfReplace(); err != nil {
		return fmt.Errorf("cannot update devsys: %w", err)
	}
	fmt.Printf("  Updated to %s.\n", latest)
	return nil
}

// latestReleaseVersion queries the GitHub releases API for the latest
// published devsys release tag, with any leading "v" stripped.
func latestReleaseVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, releasesAPIURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach GitHub releases API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub releases API returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cannot parse GitHub releases response: %w", err)
	}
	if result.TagName == "" {
		return "", fmt.Errorf("GitHub releases response had no tag_name")
	}
	return strings.TrimPrefix(result.TagName, "v"), nil
}

// selfReplace downloads the release asset matching the current OS/arch and
// atomically swaps it in for the currently running devsys binary.
func selfReplace() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine current executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	assetURL, err := releaseAssetURL()
	if err != nil {
		return err
	}

	return replaceBinary(assetURL, execPath)
}

// releaseAssetURL builds the GitHub "latest release" asset download URL for
// the current platform, matching the naming convention install.sh/
// install.ps1 already use (devsys_<OS>_<ARCH>[.exe]). Using the
// releases/latest/download/ redirect means no release tag needs to be known
// or constructed here at all.
func releaseAssetURL() (string, error) {
	var osName string
	switch runtime.GOOS {
	case "linux":
		osName = "Linux"
	case "darwin":
		osName = "Darwin"
	case "windows":
		osName = "Windows"
	default:
		return "", fmt.Errorf("unsupported OS for self-update: %s", runtime.GOOS)
	}

	var archName string
	switch runtime.GOARCH {
	case "amd64":
		archName = "x86_64"
	case "arm64":
		archName = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture for self-update: %s", runtime.GOARCH)
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s/devsys_%s_%s%s", releaseDownloadBaseURL, osName, archName, ext), nil
}

// replaceBinary downloads assetURL and atomically swaps it in for the file
// at destPath, using the rename-aside/rename-into-place/best-effort-cleanup
// pattern that works safely on both Unix (where a running binary's path can
// be freely renamed or overwritten) and Windows (where the running binary's
// own file is typically locked against direct overwrite, but not against
// being renamed aside). Exposed separately from selfReplace so tests can
// exercise the actual file-replace mechanics against a temp directory
// instead of the real running executable.
func replaceBinary(assetURL, destPath string) error {
	newPath := destPath + ".new"
	oldPath := destPath + ".old"

	if err := downloadFile(assetURL, newPath); err != nil {
		return fmt.Errorf("cannot download new version: %w", err)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("cannot make new binary executable: %w", err)
	}

	// Clean up a leftover .old from a previous run (e.g. one Windows
	// couldn't remove while it was still locked) — best-effort, fine if it
	// doesn't exist or still can't be removed.
	_ = os.Remove(oldPath)

	if err := os.Rename(destPath, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("cannot move aside current binary: %w", err)
	}
	if err := os.Rename(newPath, destPath); err != nil {
		// Best-effort restore of the original binary.
		os.Rename(oldPath, destPath)
		return fmt.Errorf("cannot install new binary: %w", err)
	}
	// Best-effort cleanup; a still-locked .old on Windows just gets
	// removed on the next successful update instead.
	_ = os.Remove(oldPath)
	return nil
}

// downloadFile fetches url and writes it to destPath.
func downloadFile(url, destPath string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// ---------------------------------------------------------------------------
// Base image updates (devsys CLI Spec, Section 12.2/12.4 — project argument
// or --all)
// ---------------------------------------------------------------------------

func updateBaseImages(args []string, all bool) error {
	var projectNames []string
	if all {
		containers, err := podman.ListDevsysContainers()
		if err != nil {
			return fmt.Errorf("cannot list containers: %w", err)
		}
		for _, c := range containers {
			name := containerField(c, "Names")
			projectNames = append(projectNames, containerToProject(name))
		}
		if len(projectNames) == 0 {
			fmt.Println("No devsys project containers found.")
			return nil
		}
	} else {
		projectNames = []string{args[0]}
	}

	for _, name := range projectNames {
		if err := updateProjectBaseImage(name); err != nil {
			fmt.Fprintf(os.Stderr, "  Error updating %s: %v\n", name, err)
		}
	}
	return nil
}

// updateProjectBaseImage looks up the current newest devsys-base version at
// the project's own existing location (no comparison against what it's
// currently on — rebuilding is cheap and idempotent), unconditionally
// rewrites .devsys/Containerfile's FROM line, and rebuilds.
func updateProjectBaseImage(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path — is the project initialised? %w", err)
	}
	containerfilePath := filepath.Join(projectPath, ".devsys", "Containerfile")

	currentRef, err := containerfileFromLine(containerfilePath)
	if err != nil {
		return err
	}
	location := locationFromReference(currentRef)

	newRef, err := registry.LatestReference(location)
	if err != nil {
		return fmt.Errorf("cannot look up newest devsys-base version: %w", err)
	}

	fmt.Printf("Updating %s: %s -> %s ...\n", projectName, currentRef, newRef)
	if err := writeContainerfileFromLine(containerfilePath, newRef); err != nil {
		return fmt.Errorf("cannot update Containerfile: %w", err)
	}

	return rebuildProject(projectName)
}
