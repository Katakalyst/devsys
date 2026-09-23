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

// ---------------------------------------------------------------------------
// checkPortsStale
// ---------------------------------------------------------------------------

// portsHash returns the SHA256 hex of the given data, matching portsFileHash.
func portsHash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// TestCheckPortsStale_BlocksWhenChanged verifies that checkPortsStale returns
// an error when .devsys/ports on disk has changed since the image was built.
func TestCheckPortsStale_BlocksWhenChanged(t *testing.T) {
	projectDir := t.TempDir()
	devsysDir := filepath.Join(projectDir, ".devsys")
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Hash baked at build time was over the original ports file.
	buildTimeHash := portsHash([]byte("3000:3000\n"))

	// On disk the file has since changed.
	if err := os.WriteFile(filepath.Join(devsysDir, "ports"), []byte("3000:3000\n5173:5173\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
		ProjectPath:     projectDir,
		ImagePortsHash:  buildTimeHash,
	})

	if err := checkPortsStale("myproject"); err == nil {
		t.Fatal("expected error for stale ports file, got nil")
	}
}

// TestCheckPortsStale_PassesWhenUnchanged verifies that checkPortsStale returns
// nil when .devsys/ports matches the image label.
func TestCheckPortsStale_PassesWhenUnchanged(t *testing.T) {
	projectDir := t.TempDir()
	// No .devsys/ports file — portsFileHash hashes empty bytes.
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
		ProjectPath:     projectDir,
		ImagePortsHash:  portsFileHash(projectDir),
	})

	if err := checkPortsStale("myproject"); err != nil {
		t.Fatalf("unexpected error for up-to-date ports: %v", err)
	}
}

// TestCheckPortsStale_PassesWhenNoFile verifies that absent .devsys/ports is a
// stable state: its hash (over empty bytes) matches whatever the image was
// built with when no ports file existed then either.
func TestCheckPortsStale_PassesWhenNoFile(t *testing.T) {
	projectDir := t.TempDir()
	// portsFileHash of a dir with no ports file = sha256("").
	emptyHash := portsFileHash(projectDir)

	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
		ProjectPath:     projectDir,
		ImagePortsHash:  emptyHash,
	})

	if err := checkPortsStale("myproject"); err != nil {
		t.Fatalf("expected no error when ports file absent and image hash matches: %v", err)
	}
}

// TestCheckPortsStale_BlocksWhenNoLabel verifies that an image without the
// devsys.ports-hash label blocks entry — every image must have a hash.
func TestCheckPortsStale_BlocksWhenNoLabel(t *testing.T) {
	projectDir := t.TempDir()
	podmanfake.Install(t, podmanfake.Options{
		ContainerExists: true,
		ImagePresent:    true,
		ProjectPath:     projectDir,
		// ImagePortsHash left empty → label absent.
	})

	if err := checkPortsStale("myproject"); err == nil {
		t.Fatal("expected error for image with no ports hash label, got nil")
	}
}
