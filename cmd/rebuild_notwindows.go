//go:build !windows

package cmd

// normalizeMountSource is a no-op on non-Windows: podman inspect there
// reports mount sources in the host's own path form already (see the
// windows variant for why Windows needs a conversion).
func normalizeMountSource(source string) string {
	return source
}
