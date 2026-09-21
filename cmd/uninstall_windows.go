//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// removeBinary schedules deletion of the running binary via a detached
// cmd.exe command that waits for this process to exit before deleting.
// Directly removing a running .exe on Windows fails — the OS locks it.
// After spawning the command this function calls os.Exit(0); the caller
// must print any final output before calling it.
func removeBinary(path string) {
	// "ping -n 2" waits ~1 second — enough for this process to fully exit
	// before "del" runs. DETACHED_PROCESS / HideWindow prevents a console
	// window from briefly appearing.
	cmd := exec.Command("cmd", "/c", fmt.Sprintf(`ping -n 2 127.0.0.1 >nul & del /f /q "%s"`, path))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot schedule binary removal: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Remove it manually: del %q\n", path)
		return
	}
	os.Exit(0)
}
