//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// removeBinary schedules deletion of the running binary via a detached
// cmd.exe command that waits for this process to exit before deleting.
// Directly removing a running .exe on Windows fails — the OS locks it.
// After spawning the command this function calls os.Exit(0); the caller
// must print any final output before calling it.
func removeBinary(path string) {
	// "ping -n 2" waits ~1 second — enough for this process to fully exit
	// before "del" runs. HideWindow prevents a console window from flashing.
	cmd := exec.Command("cmd", "/c", fmt.Sprintf(`ping -n 2 127.0.0.1 >nul & del /f /q "%s"`, path))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot schedule binary removal: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Remove it manually: del %q\n", path)
		return
	}
	os.Exit(0)
}

// removeInstallerPathEntry removes the %LOCALAPPDATA%\Programs\devsys entry
// the installer added to the user PATH in the Windows registry (HKCU\Environment).
func removeInstallerPathEntry() {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		fmt.Fprintf(os.Stderr, "  Warning: LOCALAPPDATA is not set — cannot locate install directory\n")
		return
	}
	installDir := filepath.Join(localAppData, "Programs", "devsys")

	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot open user environment registry key: %v\n", err)
		return
	}
	defer key.Close()

	currentPath, valtype, err := key.GetStringValue("Path")
	if err != nil {
		if err == registry.ErrNotExist {
			return // no Path entry — nothing to do
		}
		fmt.Fprintf(os.Stderr, "  Warning: cannot read user PATH from registry: %v\n", err)
		return
	}

	entries := strings.Split(currentPath, ";")
	filtered := entries[:0]
	found := false
	for _, e := range entries {
		if strings.EqualFold(strings.TrimSpace(e), installDir) {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		return // not present — nothing to do
	}

	newPath := strings.Join(filtered, ";")
	var writeErr error
	switch valtype {
	case registry.EXPAND_SZ:
		writeErr = key.SetExpandStringValue("Path", newPath)
	default:
		writeErr = key.SetStringValue("Path", newPath)
	}
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot update user PATH in registry: %v\n", writeErr)
		fmt.Fprintf(os.Stderr, "  Remove manually: delete %q from your user PATH in System Properties.\n", installDir)
		return
	}
	fmt.Printf("Removed %s from user PATH.\n", installDir)
}
