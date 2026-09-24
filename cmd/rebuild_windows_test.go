//go:build windows

package cmd

import "testing"

func TestNormalizeMountSource_WSLPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`/mnt/c/Users/ebre/Documents/Projekte/private/local-llm`, `C:\Users\ebre\Documents\Projekte\private\local-llm`},
		{`/mnt/d/projects/foo`, `D:\projects\foo`},
		{`/mnt/c`, `C:`},
		{`/mnt/C/Users/ebre`, `C:\Users\ebre`},
	}
	for _, c := range cases {
		if got := normalizeMountSource(c.in); got != c.want {
			t.Errorf("normalizeMountSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeMountSource_AlreadyWindowsPath_Unchanged(t *testing.T) {
	in := `C:\Users\ebre\Documents\Projekte\private\local-llm`
	if got := normalizeMountSource(in); got != in {
		t.Errorf("normalizeMountSource(%q) = %q, want unchanged", in, got)
	}
}

func TestNormalizeMountSource_UnrelatedPath_Unchanged(t *testing.T) {
	in := `/home/user/project`
	if got := normalizeMountSource(in); got != in {
		t.Errorf("normalizeMountSource(%q) = %q, want unchanged", in, got)
	}
}
