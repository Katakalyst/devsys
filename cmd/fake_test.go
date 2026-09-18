package cmd

import (
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
)

// TestFakePodman is the TestHelperProcess entry point for the cmd package.
// When this test binary is re-invoked as a fake podman subprocess by
// podmanfake.Install, Handle detects GO_WANT_FAKE_PODMAN=1 and exits.
// In normal test runs the call is a no-op and the test passes immediately.
func TestFakePodman(t *testing.T) { podmanfake.Handle() }
