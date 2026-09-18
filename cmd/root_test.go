package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
)

// TestRootCmd_PersistentPostRun_ChecksCLIVersionOnAnyCommand exercises real
// Cobra command dispatch (rootCmd.Execute()), not a direct RunE call, since
// PersistentPostRun only fires through that path — confirming the "check on
// every command" behavior (devsys CLI Spec, Section 12.5) is actually wired
// up, not just present as an untriggered closure.
func TestRootCmd_PersistentPostRun_ChecksCLIVersionOnAnyCommand(t *testing.T) {
	redirectCacheDir(t)
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

	if checkThrottled("cli-version") {
		t.Fatal("test setup: cli-version marker should not already be throttled")
	}

	rootCmd.SetArgs([]string{"list"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute(): %v", err)
	}

	if !checkThrottled("cli-version") {
		t.Error("expected PersistentPostRun to have run the CLI-version check and marked it, after `devsys list`")
	}
}

// TestRootCmd_PersistentPostRun_SkipsUpdateCommand confirms the CLI-version
// check does not run again immediately after `devsys update` itself, which
// would otherwise print a confusing "outdated" notice about the binary that
// command may have just replaced.
func TestRootCmd_PersistentPostRun_SkipsUpdateCommand(t *testing.T) {
	redirectCacheDir(t)
	origVersion := currentVersion
	currentVersion = "dev" // dev build: updateCLI() itself is a no-op, safe to actually run
	t.Cleanup(func() { currentVersion = origVersion })

	rootCmd.SetArgs([]string{"update"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute(): %v", err)
	}

	if checkThrottled("cli-version") {
		t.Error("expected PersistentPostRun to skip the CLI-version check after `devsys update` itself")
	}
}
