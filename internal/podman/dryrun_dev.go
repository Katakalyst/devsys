//go:build dev

package podman

import (
	"fmt"
	"os"
)

func init() {
	DryRun = true
	// Diagnostic banner, not command output — must go to stderr. RunPodman
	// captures a real podman invocation's stdout and parses it (e.g.
	// ContainerIsRunning expects exactly "true"/"false"); printing this
	// banner to stdout would corrupt that for every single call, including
	// inside the podmanfake subprocess itself (a fresh OS process that
	// re-runs this init() too, since it re-invokes the same test binary).
	fmt.Fprintln(os.Stderr, "[dev] dry-run mode active — no podman commands will be executed")
}
