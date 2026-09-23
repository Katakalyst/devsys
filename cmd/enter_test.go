package cmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
)

// containerfileHash returns the hex SHA256 of data, matching buildImage's label value.
func containerfileHash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// TestCheckContainerfileStale_BlocksWhenChanged verifies that checkContainerfileStale
// returns an error when the Containerfile on disk differs from the image label.
func TestCheckContainerfileStale_BlocksWhenChanged(t *testing.T) {
	projectDir := t.TempDir()
	devsysDir := filepath.Join(projectDir, ".devsys")
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Write a Containerfile that was used at build time.
	original := []byte("FROM devsys-base:1.0.0\n")
	buildTimeHash := containerfileHash(original)

	// Write a *different* Containerfile on disk (simulating an edit since last build).
	modified := []byte("FROM devsys-base:1.0.0\nRUN apt-get install -y git\n")
	if err := os.WriteFile(filepath.Join(devsysDir, "Containerfile"), modified, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists:        true,
		ImagePresent:           true,
		ProjectPath:            projectDir,
		ImageContainerfileHash: buildTimeHash,
	})

	err := checkContainerfileStale("myproject")
	if err == nil {
		t.Fatal("expected error for stale Containerfile, got nil")
	}
}

// TestCheckContainerfileStale_PassesWhenUnchanged verifies that checkContainerfileStale
// returns nil when the Containerfile matches the image label.
func TestCheckContainerfileStale_PassesWhenUnchanged(t *testing.T) {
	projectDir := t.TempDir()
	devsysDir := filepath.Join(projectDir, ".devsys")
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	contents := []byte("FROM devsys-base:1.0.0\n")
	if err := os.WriteFile(filepath.Join(devsysDir, "Containerfile"), contents, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists:        true,
		ImagePresent:           true,
		ProjectPath:            projectDir,
		ImageContainerfileHash: containerfileHash(contents),
	})

	if err := checkContainerfileStale("myproject"); err != nil {
		t.Fatalf("unexpected error for up-to-date Containerfile: %v", err)
	}
}

// TestCheckContainerfileStale_BlocksWhenNoLabel verifies that an image without
// the devsys.containerfile-hash label blocks entry — every image must have a hash.
func TestCheckContainerfileStale_BlocksWhenNoLabel(t *testing.T) {
	projectDir := t.TempDir()
	devsysDir := filepath.Join(projectDir, ".devsys")
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devsysDir, "Containerfile"), []byte("FROM devsys-base:1.0.0\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// ImageContainerfileHash left empty → label absent.
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
		ProjectPath:     projectDir,
	})

	if err := checkContainerfileStale("myproject"); err == nil {
		t.Fatal("expected error for image with no containerfile hash label, got nil")
	}
}

// TestCheckContainerfileStale_BlocksWhenNoProjectPath verifies that a missing
// workspace mount is an error, not a silent skip.
func TestCheckContainerfileStale_BlocksWhenNoProjectPath(t *testing.T) {
	// ProjectPath left empty → getProjectPath returns "no mount found" error.
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
	})

	if err := checkContainerfileStale("myproject"); err == nil {
		t.Fatal("expected error when project path unavailable, got nil")
	}
}
