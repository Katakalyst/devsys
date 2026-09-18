package cmd

import (
	"fmt"
	"os"
	"strings"
)

// containerfileFromLine reads a project's .devsys/Containerfile and returns
// the full reference on its FROM line (e.g.
// "ghcr.io/katakalyst/devsys-base:1.4.0") — the sole record of which
// devsys-base version that project uses (devsys CLI Spec, Section 12.3).
func containerfileFromLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FROM ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "FROM ")), nil
		}
	}
	return "", fmt.Errorf("no FROM line found in %s", path)
}

// writeContainerfileFromLine rewrites only the first FROM line in a
// project's .devsys/Containerfile to newRef, leaving every other line
// (including anything a project or agent added after the base image)
// untouched.
func writeContainerfileFromLine(path, newRef string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "FROM ") {
			lines[i] = "FROM " + newRef
			replaced = true
			break
		}
	}
	if !replaced {
		return fmt.Errorf("no FROM line found in %s", path)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// locationFromReference strips the tag from an image reference, returning
// just the registry location (e.g. "ghcr.io/katakalyst/devsys-base:1.4.0"
// -> "ghcr.io/katakalyst/devsys-base"). The tag-colon is distinguished from
// a registry port-colon (e.g. "localhost:5000/repo:tag") by only looking
// after the last "/".
func locationFromReference(ref string) string {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon]
	}
	return ref
}
