package cmd

import (
	"os"
	"testing"
	"time"
)

// redirectCacheDir points os.UserCacheDir() at a fresh temp dir for the
// duration of t, regardless of platform (only one of these env vars is
// actually consulted by the running OS; setting both is harmless).
func redirectCacheDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("LocalAppData", dir)
}

func TestCheckThrottled_NoMarker_NotThrottled(t *testing.T) {
	redirectCacheDir(t)
	if checkThrottled("some-check") {
		t.Error("expected not throttled when no marker file exists yet")
	}
}

func TestMarkChecked_ThenThrottled(t *testing.T) {
	redirectCacheDir(t)
	markChecked("some-check")
	if !checkThrottled("some-check") {
		t.Error("expected throttled immediately after markChecked")
	}
}

func TestCheckThrottled_StaleMarker_NotThrottled(t *testing.T) {
	redirectCacheDir(t)
	markChecked("some-check")

	path, err := checkMarkerPath("some-check")
	if err != nil {
		t.Fatalf("checkMarkerPath: %v", err)
	}
	old := time.Now().Add(-2 * staleCheckInterval)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("os.Chtimes: %v", err)
	}

	if checkThrottled("some-check") {
		t.Error("expected not throttled once the marker is older than staleCheckInterval")
	}
}

func TestMarkChecked_DifferentNames_Independent(t *testing.T) {
	redirectCacheDir(t)
	markChecked("check-a")
	if checkThrottled("check-b") {
		t.Error("expected check-b to be unaffected by check-a's marker")
	}
}
