// Package podman (white-box tests for unexported helpers).
package podman

import "testing"

func TestIsReadOnly(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		// Empty / nil → safe to treat as read-only.
		{nil, true},
		{[]string{}, true},
		// Explicit read-only subcommands.
		{[]string{"--version"}, true},
		{[]string{"version"}, true},
		{[]string{"info"}, true},
		{[]string{"inspect", "some-container"}, true},
		{[]string{"ps"}, true},
		{[]string{"ps", "-a"}, true},
		// Namespaced inspect / ls / list.
		{[]string{"secret", "inspect", "my-secret"}, true},
		{[]string{"secret", "ls"}, true},
		{[]string{"secret", "list"}, true},
		{[]string{"volume", "inspect", "my-vol"}, true},
		{[]string{"volume", "ls"}, true},
		{[]string{"volume", "list"}, true},
		{[]string{"image", "inspect", "my-image"}, true},
		{[]string{"image", "ls"}, true},
		{[]string{"container", "inspect", "c"}, true},
		{[]string{"container", "list"}, true},
		// Mutating — bare namespace keyword without a safe second arg.
		{[]string{"secret"}, false},
		{[]string{"volume"}, false},
		{[]string{"image"}, false},
		{[]string{"container"}, false},
		// Mutating subcommands.
		{[]string{"secret", "create"}, false},
		{[]string{"secret", "rm"}, false},
		{[]string{"volume", "create"}, false},
		{[]string{"volume", "rm"}, false},
		{[]string{"image", "prune"}, false},
		{[]string{"create"}, false},
		{[]string{"build"}, false},
		{[]string{"run"}, false},
		{[]string{"start"}, false},
		{[]string{"stop"}, false},
		{[]string{"rm"}, false},
		{[]string{"pull"}, false},
		{[]string{"push"}, false},
	}

	for _, tc := range tests {
		got := isReadOnly(tc.args)
		if got != tc.want {
			t.Errorf("isReadOnly(%v) = %v; want %v", tc.args, got, tc.want)
		}
	}
}
