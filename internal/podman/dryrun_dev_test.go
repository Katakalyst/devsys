//go:build dev

package podman_test

// This file only compiles under `go test -tags dev ./...`. It exists to
// automatically prove the build-tag wiring in dryrun_dev.go itself still
// works — the one thing a plain `go test ./...` run can never catch, since
// it never compiles dryrun_dev.go at all. See DEVELOPMENT.md: dry-run is
// verified by two separate automated `go test` runs, not a runtime flag.

import (
	"testing"

	"github.com/katakalyst/devsys/internal/podman"
)

// TestDryRunDefaultsTrueUnderDevTag runs only in the `-tags dev` test run.
// dryrun_dev.go's init() must have already set podman.DryRun = true before
// any test in this package executes; this is the only place a broken or
// removed build tag / init() would actually be caught automatically.
func TestDryRunDefaultsTrueUnderDevTag(t *testing.T) {
	if !podman.DryRun {
		t.Fatal("expected podman.DryRun to default true when built with -tags dev")
	}
}
