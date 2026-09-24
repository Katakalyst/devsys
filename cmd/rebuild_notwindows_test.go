//go:build !windows

package cmd

import "testing"

func TestNormalizeMountSource_NoOp(t *testing.T) {
	cases := []string{
		"/home/user/project",
		"/mnt/c/Users/ebre/project",
		"C:\\Users\\ebre\\project",
	}
	for _, in := range cases {
		if got := normalizeMountSource(in); got != in {
			t.Errorf("normalizeMountSource(%q) = %q, want unchanged on non-Windows", in, got)
		}
	}
}
