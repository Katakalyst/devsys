//go:build !windows

package cmd

import (
	"fmt"
	"os"
)

// removeBinary deletes the running binary. On Unix this is safe — the process
// continues from its already-loaded image; the file is simply unlinked.
func removeBinary(path string) {
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: cannot remove binary: %v\n", err)
	}
}
