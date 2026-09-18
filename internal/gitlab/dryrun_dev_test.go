//go:build dev

package gitlab_test

// This file only compiles under `go test -tags dev ./...`. It exists to
// automatically prove the build-tag wiring in dryrun_dev.go itself still
// works — the one thing a plain `go test ./...` run can never catch, since
// it never compiles that file at all. See DEVELOPMENT.md: dry-run is
// verified by two separate automated `go test` runs, not a runtime flag.
//
// Unlike internal/podman, no defensive reset was needed in this package's
// existing tests: every test in gitlab_test.go already constructs its own
// Client via NewClient and explicitly sets c.DryRun = false itself, so none
// of them depend on (or are broken by) whatever defaultDryRun happens to be.

import (
	"testing"

	"github.com/katakalyst/devsys/internal/gitlab"
)

// TestNewClient_DryRunDefaultsTrueUnderDevTag runs only in the `-tags dev`
// test run. A freshly constructed Client must default to DryRun = true —
// the whole point of dryrun_dev.go's defaultDryRun var — without the caller
// having to set it explicitly.
func TestNewClient_DryRunDefaultsTrueUnderDevTag(t *testing.T) {
	c := gitlab.NewClient("https://gitlab.example.com", "test-token")
	if !c.DryRun {
		t.Fatal("expected a freshly constructed Client to default DryRun = true when built with -tags dev")
	}
}
