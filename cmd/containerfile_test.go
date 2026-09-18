package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func writeContainerfile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestContainerfileFromLine(t *testing.T) {
	path := writeContainerfile(t, t.TempDir(), "FROM ghcr.io/katakalyst/devsys-base:1.4.0\nRUN apt-get update\n")
	got, err := containerfileFromLine(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghcr.io/katakalyst/devsys-base:1.4.0" {
		t.Errorf("containerfileFromLine() = %q; want %q", got, "ghcr.io/katakalyst/devsys-base:1.4.0")
	}
}

func TestContainerfileFromLine_NoFromLine(t *testing.T) {
	path := writeContainerfile(t, t.TempDir(), "RUN echo hello\n")
	_, err := containerfileFromLine(path)
	if err == nil {
		t.Error("expected error when no FROM line is present")
	}
}

func TestContainerfileFromLine_MissingFile(t *testing.T) {
	_, err := containerfileFromLine(filepath.Join(t.TempDir(), "nonexistent", "Containerfile"))
	if err == nil {
		t.Error("expected error for a missing Containerfile")
	}
}

func TestWriteContainerfileFromLine_ReplacesOnlyFromLine(t *testing.T) {
	path := writeContainerfile(t, t.TempDir(), "FROM ghcr.io/katakalyst/devsys-base:1.0.0\nRUN apt-get update\nRUN useradd -m dev\n")

	if err := writeContainerfileFromLine(path, "ghcr.io/katakalyst/devsys-base:2.0.0"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "FROM ghcr.io/katakalyst/devsys-base:2.0.0\nRUN apt-get update\nRUN useradd -m dev\n"
	if string(data) != want {
		t.Errorf("Containerfile after write:\n%s\nwant:\n%s", data, want)
	}
}

func TestWriteContainerfileFromLine_NoFromLine_ReturnsError(t *testing.T) {
	path := writeContainerfile(t, t.TempDir(), "RUN echo hello\n")
	err := writeContainerfileFromLine(path, "ghcr.io/katakalyst/devsys-base:2.0.0")
	if err == nil {
		t.Error("expected error when no FROM line is present to replace")
	}
}

func TestLocationFromReference(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"ghcr.io/katakalyst/devsys-base:1.4.0", "ghcr.io/katakalyst/devsys-base"},
		{"ghcr.io/katakalyst/devsys-base", "ghcr.io/katakalyst/devsys-base"},
		{"localhost:5000/ns/repo:1.0.0", "localhost:5000/ns/repo"},
		{"localhost:5000/ns/repo", "localhost:5000/ns/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := locationFromReference(tc.in)
			if got != tc.want {
				t.Errorf("locationFromReference(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
