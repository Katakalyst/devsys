package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/katakalyst/devsys/internal/github"
)

// newTestServer creates a test HTTP server and a client pointed at it.
// The handler receives the request; the server is closed via t.Cleanup.
// Because the client's apiBase constant points at api.github.com, we swap the
// base URL by reaching into the struct — DryRun is always disabled so tests
// exercise the real code paths.
func newTestServer(t *testing.T, h http.HandlerFunc) *github.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := github.NewClientWithBase(ts.URL, "test-token")
	c.DryRun = false
	return c
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// CreateRepo
// ---------------------------------------------------------------------------

func TestCreateRepo_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/user/repos" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":        123,
			"full_name": "octocat/myrepo",
		})
	})

	id, fullName, err := c.CreateRepo("myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 123 {
		t.Errorf("id: want 123, got %d", id)
	}
	if fullName != "octocat/myrepo" {
		t.Errorf("fullName: want %q, got %q", "octocat/myrepo", fullName)
	}
}

func TestCreateRepo_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	_, _, err := c.CreateRepo("myrepo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// APIBaseForHost
// ---------------------------------------------------------------------------

func TestAPIBaseForHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"github.com", "https://api.github.com"},
		{"", "https://api.github.com"},
		{"ghe.mycompany.com", "https://ghe.mycompany.com/api/v3"},
		{"github.example.org", "https://github.example.org/api/v3"},
	}
	for _, tc := range cases {
		got := github.APIBaseForHost(tc.host)
		if got != tc.want {
			t.Errorf("APIBaseForHost(%q): want %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestCreateRepo_DryRun(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	id, fullName, err := c.CreateRepo("myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server called %d times; expected 0 in dry-run", calls)
	}
	if id != 0 {
		t.Errorf("dry-run id: want 0, got %d", id)
	}
	if fullName == "" {
		t.Error("dry-run fullName: expected non-empty placeholder")
	}
}
