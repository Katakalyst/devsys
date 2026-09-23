package cmd

// Tests for devsys rm's per-repo, per-remote credential revocation
// (revokeProjectRepoCredentials) — the fix for its previously stale
// GitLab-only, single-remote assumption (documents/TODO.md). Uses GitHub
// secrets throughout: GitHub revocation is a local print-reminder + secret
// delete, so these tests exercise the real discovery/confirm/delete path
// without needing a fake GitLab API server.

import (
	"bufio"
	"os"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/podmanfake"
)

func TestRevokeProjectRepoCredentials_NoProjectPath(t *testing.T) {
	// Should degrade gracefully (print a message, no panic) rather than
	// error when the project's workspace path could not be determined —
	// e.g. the container was already removed before this step runs.
	reader := bufio.NewReader(strings.NewReader(""))
	revokeProjectRepoCredentials(reader, "testproject", "")
}

func TestRevokeProjectRepoCredentials_NoCredentials(t *testing.T) {
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	podmanfake.Install(t, podmanfake.Options{SecretExists: false})
	reader := bufio.NewReader(strings.NewReader(""))
	// No remote at all -> gatherRepoStatuses yields a no-token row; nothing
	// to revoke, no confirmation prompt should be needed (empty stdin is
	// enough to prove no read happened).
	revokeProjectRepoCredentials(reader, "testproject", root)
}

func TestRevokeProjectRepoCredentials_GitHubSecret_ConfirmedRemoves(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/app.git"}}); err != nil {
		t.Fatalf("create origin: %v", err)
	}

	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	injectStdin(t, "y\n")
	reader := bufio.NewReader(os.Stdin)

	revokeProjectRepoCredentials(reader, "foo", root)

	if !rec.HasSubcommand("secret") {
		t.Errorf("expected a podman secret call (delete), calls: %v", rec.Calls())
	}
}

func TestRevokeProjectRepoCredentials_GitHubSecret_DeclinedKeepsSecret(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/app.git"}}); err != nil {
		t.Fatalf("create origin: %v", err)
	}

	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	injectStdin(t, "n\n")
	reader := bufio.NewReader(os.Stdin)

	revokeProjectRepoCredentials(reader, "foo", root)

	for _, call := range rec.Calls() {
		if len(call) >= 2 && call[0] == "secret" && call[1] == "rm" {
			t.Errorf("declining revocation must not delete the secret; calls: %v", rec.Calls())
		}
	}
}

func TestRevokeProjectRepoCredentials_MultipleRemotesPromptsForEach(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/owner/app.git"}}); err != nil {
		t.Fatalf("create origin: %v", err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "mirror", URLs: []string{"https://github.com/owner/app-mirror.git"}}); err != nil {
		t.Fatalf("create mirror: %v", err)
	}

	rec := podmanfake.Install(t, podmanfake.Options{SecretExists: true})
	// Confirm both prompts.
	injectStdin(t, "y\ny\n")
	reader := bufio.NewReader(os.Stdin)

	revokeProjectRepoCredentials(reader, "foo", root)

	deletes := 0
	for _, call := range rec.Calls() {
		if len(call) >= 2 && call[0] == "secret" && call[1] == "rm" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("want 2 secret deletions (one per remote), got %d; calls: %v", deletes, rec.Calls())
	}
}
