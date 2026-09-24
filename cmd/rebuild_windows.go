//go:build windows

package cmd

import (
	"regexp"
	"strings"
)

// wslMountSourcePattern matches the WSL-style path a Podman machine on
// Windows reports as a bind mount's Source in `podman inspect` — e.g.
// "/mnt/c/Users/ebre/project" — regardless of what path was actually passed
// to `podman create -v` at container-creation time (a native Windows path,
// "C:\Users\ebre\project", per createProjectContainerWithRepoSecrets).
// Podman for Windows runs its machine inside WSL2, and `podman inspect`
// reflects the mount as the machine's own (Linux) view of it, not the
// original host string.
var wslMountSourcePattern = regexp.MustCompile(`^/mnt/([a-zA-Z])(/.*)?$`)

// normalizeMountSource converts a podman-inspect-reported mount source back
// into a native Windows path when it's in WSL's "/mnt/<drive>/..." form.
// Left unchanged if it doesn't match that shape (e.g. already a Windows
// path, on a non-WSL podman machine backend).
func normalizeMountSource(source string) string {
	m := wslMountSourcePattern.FindStringSubmatch(source)
	if m == nil {
		return source
	}
	drive := strings.ToUpper(m[1])
	rest := strings.ReplaceAll(m[2], "/", `\`)
	return drive + ":" + rest
}
