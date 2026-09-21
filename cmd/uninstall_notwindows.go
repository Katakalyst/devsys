//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// removeBinary deletes the running binary. On Unix this is safe — the process
// continues from its already-loaded image; the file is simply unlinked.
func removeBinary(path string) {
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot remove binary: %v\n", err)
	}
}

// installerPathBlock is the exact text the macOS installer appends to shell
// profile files. Searching for this literal string is safe because the
// installer checks for ".local/bin" before appending, so it appears at most once.
const installerPathBlock = "\n# Added by devsys installer\nexport PATH=\"${HOME}/.local/bin:${PATH}\"\n"

// removeInstallerPathEntry removes the PATH block the installer added to
// .zshrc and .profile on macOS. On Linux the installer never modifies profile
// files, so this is a no-op there.
func removeInstallerPathEntry() {
	if runtime.GOOS != "darwin" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot determine home directory: %v\n", err)
		return
	}
	for _, profile := range []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".profile"),
	} {
		removeFromProfile(profile)
	}
}

func removeFromProfile(profilePath string) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "  Warning: cannot read %s: %v\n", profilePath, err)
		}
		return
	}
	updated := strings.Replace(string(data), installerPathBlock, "", 1)
	if updated == string(data) {
		return // block not present — nothing to do
	}
	if err := os.WriteFile(profilePath, []byte(updated), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot update %s: %v\n", profilePath, err)
		fmt.Fprintf(os.Stderr, "  Remove manually: the '# Added by devsys installer' block in %s\n", profilePath)
		return
	}
	fmt.Printf("Removed PATH entry from %s\n", profilePath)
}
