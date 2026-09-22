package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ---------------------------------------------------------------------------
// CreateDeployKey
// ---------------------------------------------------------------------------

func TestCreateDeployKey_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/octocat/myrepo/keys" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"id": 42})
	})

	keyID, err := c.CreateDeployKey("octocat", "myrepo", "devsys-key", "ssh-ed25519 AAAA...", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if keyID != 42 {
		t.Errorf("keyID: want 42, got %d", keyID)
	}
}

func TestCreateDeployKey_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unprocessable", http.StatusUnprocessableEntity)
	})
	_, err := c.CreateDeployKey("octocat", "myrepo", "devsys-key", "ssh-ed25519 AAAA...", true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCreateDeployKey_DryRun(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	keyID, err := c.CreateDeployKey("octocat", "myrepo", "devsys-key", "ssh-ed25519 AAAA...", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server called %d times; expected 0 in dry-run", calls)
	}
	if keyID != 0 {
		t.Errorf("dry-run keyID: want 0, got %d", keyID)
	}
}

// ---------------------------------------------------------------------------
// ListDeployKeys
// ---------------------------------------------------------------------------

func TestListDeployKeys_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/octocat/myrepo/keys" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, []map[string]interface{}{
			{"id": 1, "title": "key-a", "key": "ssh-ed25519 AAAA...", "read_only": false},
			{"id": 2, "title": "key-b", "key": "ssh-ed25519 BBBB...", "read_only": true},
		})
	})

	keys, err := c.ListDeployKeys("octocat", "myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
	if keys[0].Title != "key-a" || keys[0].ReadOnly {
		t.Errorf("unexpected first key: %+v", keys[0])
	}
	if !keys[1].ReadOnly {
		t.Errorf("second key should be read-only")
	}
}

func TestListDeployKeys_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	_, err := c.ListDeployKeys("octocat", "myrepo")
	if err == nil {
		t.Fatal("expected error on HTTP 404, got nil")
	}
}

// ---------------------------------------------------------------------------
// DeleteDeployKey
// ---------------------------------------------------------------------------

func TestDeleteDeployKey_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/repos/octocat/myrepo/keys/42" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.DeleteDeployKey("octocat", "myrepo", 42); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteDeployKey_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	if err := c.DeleteDeployKey("octocat", "myrepo", 99); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDeleteDeployKey_DryRun(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	if err := c.DeleteDeployKey("octocat", "myrepo", 42); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server called %d times; expected 0 in dry-run", calls)
	}
}

// ---------------------------------------------------------------------------
// GenerateDeployKeyPair
// ---------------------------------------------------------------------------

func TestGenerateDeployKeyPair(t *testing.T) {
	priv, pub, err := github.GenerateDeployKeyPair()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(priv, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("private key does not look like OpenSSH PEM: %q", priv[:min(len(priv), 60)])
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Errorf("public key does not look like Ed25519: %q", pub[:min(len(pub), 60)])
	}
}

// Two calls must produce distinct keypairs.
func TestGenerateDeployKeyPair_Unique(t *testing.T) {
	_, pub1, err := github.GenerateDeployKeyPair()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, pub2, err := github.GenerateDeployKeyPair()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if pub1 == pub2 {
		t.Error("two generated keypairs have the same public key")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
