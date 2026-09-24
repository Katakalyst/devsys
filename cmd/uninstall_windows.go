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
//
// If the binary lives in the expected install directory
// (%LOCALAPPDATA%\Programs\devsys), the directory itself is also removed
// after the binary is deleted.
//
// The delete commands are written to a temporary .bat file and run via
// "cmd /c call <path>" rather than passed as one inline string. An earlier
// version built the whole "ping & del "path" & rmdir "path"" line as a
// single exec.Command argument; since that argument contains both spaces
// and embedded quotes, Go's Windows argument escaping (syscall.EscapeArg)
// wraps the entire thing in an outer quote pair and backslash-escapes the
// inner quotes — the CRT/CommandLineToArgvW convention. cmd.exe does not
// understand backslash-escaped quotes: its own /c quote-stripping rule
// (see `cmd /?`) only preserves quotes when there are exactly two of them
// with no special characters (like &) between them, so it instead falls
// back to stripping just the first and last quote characters of the whole
// line, leaving stray backslash-quote sequences glued onto the path
// tokens. del/rmdir then silently fail to match the real paths — with
// HideWindow set, that failure is invisible, and the binary/install
// directory are left behind even though "devsys uninstall" reports
// success. A bare file path (the .bat file) has no embedded quotes or
// special characters, so it round-trips through both escaping schemes
// intact regardless of spaces in the path.
func removeBinary(path string) {
	dir := filepath.Dir(path)
	localAppData := os.Getenv("LOCALAPPDATA")
	installDir := ""
	if localAppData != "" {
		installDir = filepath.Join(localAppData, "Programs", "devsys")
	}

	// Only remove the install directory when the binary is actually in the
	// expected location — never rmdir an arbitrary directory.
	// "ping -n 2" waits ~1 second — enough for this process to fully exit
	// before del/rmdir run.
	lines := []string{
		"@echo off",
		"ping -n 2 127.0.0.1 >nul",
		fmt.Sprintf(`del /f /q "%s"`, path),
	}
	if installDir != "" && strings.EqualFold(dir, installDir) {
		lines = append(lines, fmt.Sprintf(`rmdir /s /q "%s"`, dir))
	}
	batPath := filepath.Join(os.TempDir(), "devsys-uninstall.bat")
	// Self-delete last so the temp file doesn't linger.
	lines = append(lines, fmt.Sprintf(`del /f /q "%s"`, batPath))

	if err := os.WriteFile(batPath, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot schedule binary removal: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Remove it manually: rmdir /s /q %q\n", dir)
		return
	}

	// HideWindow prevents a console window from flashing.
	cmd := exec.Command("cmd", "/c", "call", batPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot schedule binary removal: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Remove it manually: rmdir /s /q %q\n", dir)
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
