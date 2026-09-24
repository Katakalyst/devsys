package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
)

// TestRootCmd_PersistentPostRun_ChecksCLIVersionOnAnyCommand exercises real
// Cobra command dispatch (rootCmd.Execute()), not a direct RunE call, since
// PersistentPostRun only fires through that path — confirming the "check on
// every command" behavior (devsys CLI Spec, Section 12.5) is actually wired
// up, not just present as an untriggered closure. Checks live every call
// now (no cache marker to assert on — a cached "up to date" answer would
// mask a version that shipped minutes ago), so this asserts the warning
// itself was printed, twice in a row, rather than a throttle marker.
func TestRootCmd_PersistentPostRun_ChecksCLIVersionOnAnyCommand(t *testing.T) {
	origVersion := currentVersion
	currentVersion = "1.0.0"
	t.Cleanup(func() { currentVersion = origVersion })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v2.0.0"})
	}))
	t.Cleanup(ts.Close)
	origAPI := releasesAPIURL
	releasesAPIURL = ts.URL
	t.Cleanup(func() { releasesAPIURL = origAPI })

	podmanfake.Install(t, podmanfake.Options{}) // `list` needs podman ps/volume ls to succeed

	runList := func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		origStderr := os.Stderr
		os.Stderr = w
		rootCmd.SetArgs([]string{"list"})
		execErr := rootCmd.Execute()
		w.Close()
		os.Stderr = origStderr
		if execErr != nil {
			t.Fatalf("rootCmd.Execute(): %v", execErr)
		}
		var buf bytes.Buffer
		buf.ReadFrom(r)
		return buf.String()
	}

	for i := 0; i < 2; i++ {
		out := runList()
		if !strings.Contains(out, "devsys 2.0.0 is available") {
			t.Errorf("call %d: expected PersistentPostRun to print the CLI-version warning, got: %q", i+1, out)
		}
	}
}

// TestRootCmd_PersistentPostRun_SkipsUpdateCommand confirms the CLI-version
// check does not run for `devsys update` itself, which would otherwise print
// a confusing "outdated" notice about the binary that command may have just
// replaced. Calls PersistentPostRun directly with updateCmd rather than
// executing the real `update` command — there's no cache marker left to
// inspect afterward (checked live every call now), and running a real
// update with a non-dev currentVersion would attempt an actual self-replace.
func TestRootCmd_PersistentPostRun_SkipsUpdateCommand(t *testing.T) {
	origVersion := currentVersion
	currentVersion = "1.0.0"
	t.Cleanup(func() { currentVersion = origVersion })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v2.0.0"})
	}))
	t.Cleanup(ts.Close)
	origAPI := releasesAPIURL
	releasesAPIURL = ts.URL
	t.Cleanup(func() { releasesAPIURL = origAPI })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	rootCmd.PersistentPostRun(updateCmd, nil)
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if strings.Contains(buf.String(), "is available") {
		t.Errorf("expected PersistentPostRun to skip the CLI-version check for `devsys update` itself, got: %q", buf.String())
	}
}
