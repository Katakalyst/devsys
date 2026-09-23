package cmd

import (
	"testing"
	"time"
)

func futureDate(days int) string {
	return time.Now().AddDate(0, 0, days).Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// parseSelection — Git Remote & Credential Spec §9: "a list of indices,
// separated by comma or space, always — never inferred from concatenated
// digits."
// ---------------------------------------------------------------------------

func TestParseSelection(t *testing.T) {
	cases := []struct {
		input   string
		max     int
		want    []int
		wantErr bool
	}{
		{"1,5", 5, []int{0, 4}, false},
		{"1 5", 5, []int{0, 4}, false},
		{"1, 5", 5, []int{0, 4}, false},
		{"15", 20, []int{14}, false}, // "15" is index fifteen, never "1" and "5"
		{"3", 5, []int{2}, false},
		{"1,1,2", 5, []int{0, 1}, false}, // duplicates collapse, order preserved
		{"", 5, nil, true},
		{"0", 5, nil, true},
		{"6", 5, nil, true},
		{"x", 5, nil, true},
	}
	for _, tc := range cases {
		got, err := parseSelection(tc.input, tc.max)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSelection(%q, %d): expected error, got %v", tc.input, tc.max, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSelection(%q, %d): unexpected error: %v", tc.input, tc.max, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseSelection(%q, %d) = %v, want %v", tc.input, tc.max, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseSelection(%q, %d) = %v, want %v", tc.input, tc.max, got, tc.want)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// tokenExpiryText — the ⚠ warning must trigger at exactly the same 30-day
// threshold devsys enter's checkTokenExpiry already uses (Spec §9: "one
// shared threshold, not a second number to keep in sync").
// ---------------------------------------------------------------------------

func TestTokenExpiryText(t *testing.T) {
	if got := tokenExpiryText(""); got != "token ok" {
		t.Errorf("empty expiry: want %q, got %q", "token ok", got)
	}
	if got := tokenExpiryText("not-a-date"); got != "token ok" {
		t.Errorf("malformed expiry: want %q, got %q", "token ok", got)
	}

	future := futureDate(60)
	if got := tokenExpiryText(future); containsWarning(got) {
		t.Errorf("expiry 60 days out should not warn: got %q", got)
	}

	soon := futureDate(10)
	if got := tokenExpiryText(soon); !containsWarning(got) {
		t.Errorf("expiry 10 days out should warn: got %q", got)
	}
}

func containsWarning(s string) bool {
	for _, r := range s {
		if r == '⚠' {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// repoDefaultName
// ---------------------------------------------------------------------------

func TestRepoDefaultName(t *testing.T) {
	cases := []struct {
		project, relPath, want string
	}{
		{"myproject", ".", "myproject"},
		{"myproject", "frontend", "myproject-frontend"},
		{"myproject", "libs/shared", "myproject-libs-shared"},
	}
	for _, tc := range cases {
		got := repoDefaultName(tc.project, tc.relPath)
		if got != tc.want {
			t.Errorf("repoDefaultName(%q, %q) = %q, want %q", tc.project, tc.relPath, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// embedTokenInHTTPSURL — both platforms now use this identically (Git
// Remote & Credential Spec §7's corrected GitHub decision).
// ---------------------------------------------------------------------------

// remoteHasEmbeddedCredentials must distinguish a URL auth itself wrote
// (embedded userinfo) from a bare URL the agent set via plain `git remote
// add` — this is what tells rotate whether it's safe to rewrite the remote
// (Git Remote & Credential Spec §9's rotate mechanical detail).
func TestRemoteHasEmbeddedCredentials(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://oauth2:glpat-xxx@gitlab.com/owner/project.git", true},
		{"https://x-access-token:ghp-yyy@github.com/owner/repo.git", true},
		{"https://gitlab.com/owner/project.git", false},
		{"https://github.com/owner/repo.git", false},
		{"git@github.com:owner/repo.git", false}, // SCP-style has no URL userinfo at all
	}
	for _, tc := range cases {
		if got := remoteHasEmbeddedCredentials(tc.url); got != tc.want {
			t.Errorf("remoteHasEmbeddedCredentials(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestEmbedTokenInHTTPSURL(t *testing.T) {
	cases := []struct {
		rawURL, user, token, want string
	}{
		{"https://gitlab.com/owner/project", "oauth2", "glpat-xxx", "https://oauth2:glpat-xxx@gitlab.com/owner/project.git"},
		{"https://gitlab.com/owner/project.git", "oauth2", "glpat-xxx", "https://oauth2:glpat-xxx@gitlab.com/owner/project.git"},
		{"https://github.com/owner/repo", "x-access-token", "ghp-yyy", "https://x-access-token:ghp-yyy@github.com/owner/repo.git"},
	}
	for _, tc := range cases {
		got, err := embedTokenInHTTPSURL(tc.rawURL, tc.user, tc.token)
		if err != nil {
			t.Fatalf("embedTokenInHTTPSURL(%q): unexpected error: %v", tc.rawURL, err)
		}
		if got != tc.want {
			t.Errorf("embedTokenInHTTPSURL(%q) = %q, want %q", tc.rawURL, got, tc.want)
		}
	}
}
