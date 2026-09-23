package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func writePortsFile(t *testing.T, dir, content string) {
	t.Helper()
	devsysDir := filepath.Join(dir, ".devsys")
	if err := os.MkdirAll(devsysDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devsysDir, "ports"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestReadPortsFile_Absent(t *testing.T) {
	mappings, err := readPortsFile(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mappings != nil {
		t.Errorf("expected nil for absent file, got %v", mappings)
	}
}

func TestReadPortsFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	writePortsFile(t, dir, "")
	mappings, err := readPortsFile(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mappings) != 0 {
		t.Errorf("expected no mappings for empty file, got %v", mappings)
	}
}

func TestReadPortsFile_CommentsAndBlanksSkipped(t *testing.T) {
	dir := t.TempDir()
	writePortsFile(t, dir, "# comment\n\n3000:3000\n  # indented comment\n\n")
	mappings, err := readPortsFile(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mappings) != 1 || mappings[0] != "3000:3000" {
		t.Errorf("want [3000:3000], got %v", mappings)
	}
}

func TestReadPortsFile_MultipleMappings(t *testing.T) {
	dir := t.TempDir()
	writePortsFile(t, dir, "3000:3000\n5173:5173\n0.0.0.0:8080:8080\n")
	mappings, err := readPortsFile(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"3000:3000", "5173:5173", "0.0.0.0:8080:8080"}
	if len(mappings) != len(want) {
		t.Fatalf("want %v, got %v", want, mappings)
	}
	for i, w := range want {
		if mappings[i] != w {
			t.Errorf("[%d]: want %q, got %q", i, w, mappings[i])
		}
	}
}
