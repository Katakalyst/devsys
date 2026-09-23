package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readPortsFile parses .devsys/ports and returns the list of port mappings to
// publish. Each non-blank, non-comment line is passed verbatim as a -p flag
// to podman create — format is "host:container" or "ip:host:container"
// (e.g. "3000:3000" or "0.0.0.0:5173:5173"). Returns nil without error when
// the file does not exist.
func readPortsFile(projectPath string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, ".devsys", "ports"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read .devsys/ports: %w", err)
	}
	var mappings []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mappings = append(mappings, line)
	}
	return mappings, nil
}

// portsFileHash returns the SHA256 hex digest of .devsys/ports. When the file
// does not exist the hash is over empty bytes — the same value that will be
// computed at enter-time when the file is absent, so a missing file is a
// stable, matchable state rather than an error.
func portsFileHash(projectPath string) string {
	data, err := os.ReadFile(filepath.Join(projectPath, ".devsys", "ports"))
	if err != nil {
		data = []byte{}
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
