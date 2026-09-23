package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// updateCLI
// ---------------------------------------------------------------------------

func TestUpdateCLI_DevBuild_Skips(t *testing.T) {
	orig := currentVersion
	currentVersion = "dev"
	t.Cleanup(func() { currentVersion = orig })

	// No HTTP server — any network call would fail, proving the function bails early.
	origAPI := releasesAPIURL
	releasesAPIURL = "http://127.0.0.1:0/should-not-be-called"
	t.Cleanup(func() { releasesAPIURL = origAPI })

	if err := updateCLI(); err != nil {
		t.Fatalf("expected no error for dev build, got: %v", err)
	}
}

func TestUpdateCLI_AlreadyUpToDate(t *testing.T) {
	orig := currentVersion
	currentVersion = "1.0.0"
	t.Cleanup(func() { currentVersion = orig })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"tag_name": "v1.0.0"})
	}))
	t.Cleanup(ts.Close)
	origAPI := releasesAPIURL
	releasesAPIURL = ts.URL
	t.Cleanup(func() { releasesAPIURL = origAPI })

	if err := updateCLI(); err != nil {
		t.Fatalf("expected no error when already up to date, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// warnIfCLIOutdated — dev build early return
// ---------------------------------------------------------------------------

func TestWarnIfCLIOutdated_DevBuild_NoOutput(t *testing.T) {
	orig := currentVersion
	currentVersion = "dev"
	t.Cleanup(func() { currentVersion = orig })

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	warnIfCLIOutdated()
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if buf.Len() > 0 {
		t.Errorf("expected no stderr output for dev build, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// downloadFile
// ---------------------------------------------------------------------------

func TestDownloadFile_Success(t *testing.T) {
	want := []byte("fake binary content")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want) //nolint:errcheck
	}))
	t.Cleanup(ts.Close)

	dest := filepath.Join(t.TempDir(), "output")
	if err := downloadFile(ts.URL, dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(want) {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestDownloadFile_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	dest := filepath.Join(t.TempDir(), "output")
	if err := downloadFile(ts.URL, dest); err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

// ---------------------------------------------------------------------------
// replaceBinary
// ---------------------------------------------------------------------------

func TestReplaceBinary_Success(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "binary")
	if err := os.WriteFile(dest, []byte("old content"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	newContent := "new content"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, newContent) //nolint:errcheck
	}))
	t.Cleanup(ts.Close)

	if err := replaceBinary(ts.URL, dest); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, _ := os.ReadFile(dest)
	if string(got) != newContent {
		t.Errorf("want %q, got %q", newContent, got)
	}
	// .old leftover should have been cleaned up.
	if _, err := os.Stat(dest + ".old"); !os.IsNotExist(err) {
		t.Error("expected .old file to be removed after successful replace")
	}
}

func TestReplaceBinary_DownloadFails_LeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "binary")
	original := []byte("original content")
	if err := os.WriteFile(dest, original, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	if err := replaceBinary(ts.URL, dest); err == nil {
		t.Fatal("expected error when download fails")
	}

	// Original binary must still be intact.
	got, _ := os.ReadFile(dest)
	if !strings.Contains(string(got), "original") {
		t.Errorf("original binary should be intact after failed replace, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// releaseAssetURL — current platform (linux/amd64 in CI)
// ---------------------------------------------------------------------------

func TestReleaseAssetURL_CurrentPlatform(t *testing.T) {
	url, err := releaseAssetURL()
	if err != nil {
		// Only expected to fail on an unsupported OS/arch.
		t.Skipf("releaseAssetURL unsupported on this platform: %v", err)
	}
	if !strings.Contains(url, "devsys_") {
		t.Errorf("expected asset URL to contain 'devsys_', got %q", url)
	}
}
