package gitlab_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/katakalyst/devsys/internal/gitlab"
)

// newTestServer creates a test HTTP server and a client pointed at it.
// The handler func receives the request; the server is closed via t.Cleanup.
func newTestServer(t *testing.T, h http.HandlerFunc) *gitlab.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := gitlab.NewClient(ts.URL, "test-token")
	c.DryRun = false // always off in unit tests so we can test real paths
	return c
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// CreateProject
// ---------------------------------------------------------------------------

func TestCreateProject_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":      42,
			"web_url": "https://gitlab.example.com/ns/myproject",
		})
	})

	id, url, err := c.CreateProject("myproject")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 42 {
		t.Errorf("id: want 42, got %d", id)
	}
	if url != "https://gitlab.example.com/ns/myproject" {
		t.Errorf("url: unexpected %q", url)
	}
}

func TestCreateProject_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	_, _, err := c.CreateProject("myproject")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCreateProject_DryRun(t *testing.T) {
	// DryRun should return a placeholder without hitting the server.
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	id, url, err := c.CreateProject("myproject")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server was called %d times; expected 0 in dry-run", calls)
	}
	if id != 0 {
		t.Errorf("dry-run id: want 0, got %d", id)
	}
	if url == "" {
		t.Error("dry-run url: expected non-empty placeholder")
	}
}

// ---------------------------------------------------------------------------
// CreateProjectToken
// ---------------------------------------------------------------------------

func TestCreateProjectToken_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/7/access_tokens" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":    99,
			"token": "glpat-abc123",
		})
	})

	tokenID, token, err := c.CreateProjectToken(7, "mytoken", []string{"api"}, 40, "2026-12-31")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokenID != 99 {
		t.Errorf("tokenID: want 99, got %d", tokenID)
	}
	if token != "glpat-abc123" {
		t.Errorf("token: want %q, got %q", "glpat-abc123", token)
	}
}

func TestCreateProjectToken_DryRun(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	_, tok, err := c.CreateProjectToken(7, "mytoken", []string{"api"}, 40, "2026-12-31")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server was called %d times; expected 0 in dry-run", calls)
	}
	if tok == "" {
		t.Error("dry-run token: expected non-empty placeholder")
	}
}

// ---------------------------------------------------------------------------
// RevokeProjectToken
// ---------------------------------------------------------------------------

func TestRevokeProjectToken_Success(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v4/projects/7/access_tokens/99" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.RevokeProjectToken(7, 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRevokeProjectToken_DryRun(t *testing.T) {
	calls := 0
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	c.DryRun = true

	if err := c.RevokeProjectToken(7, 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("server was called %d times; expected 0 in dry-run", calls)
	}
}

// ---------------------------------------------------------------------------
// GetProject
// ---------------------------------------------------------------------------

func TestGetProject_ByURL(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": 55})
	})

	id, err := c.GetProject("https://gitlab.example.com/ns/myproject")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 55 {
		t.Errorf("id: want 55, got %d", id)
	}
}

func TestGetProject_BySSHURL(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": 77})
	})

	id, err := c.GetProject("git@gitlab.example.com:ns/myproject.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 77 {
		t.Errorf("id: want 77, got %d", id)
	}
}

// ---------------------------------------------------------------------------
// GetProjectTokens
// ---------------------------------------------------------------------------

func TestGetProjectTokens(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]interface{}{
			{"id": 1, "name": "tok-a", "expires_at": "2026-12-31", "revoked": false},
			{"id": 2, "name": "tok-b", "expires_at": "2026-06-01", "revoked": true},
		})
	})

	tokens, err := c.GetProjectTokens(7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("want 2 tokens, got %d", len(tokens))
	}
	if tokens[0].Name != "tok-a" || tokens[0].Revoked {
		t.Errorf("unexpected first token: %+v", tokens[0])
	}
	if !tokens[1].Revoked {
		t.Errorf("second token should be revoked")
	}
}

func TestGetProjectTokens_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	_, err := c.GetProjectTokens(7)
	if err == nil {
		t.Fatal("expected error on HTTP 401, got nil")
	}
}

// ---------------------------------------------------------------------------
// NewClient — default baseURL
// ---------------------------------------------------------------------------

func TestNewClient_DefaultBaseURL(t *testing.T) {
	c := gitlab.NewClient("", "token")
	if c.BaseURL != "https://gitlab.com" {
		t.Errorf("expected default baseURL https://gitlab.com, got %q", c.BaseURL)
	}
}

func TestNewClient_TrailingSlashStripped(t *testing.T) {
	c := gitlab.NewClient("https://mygit.example.com/", "token")
	if c.BaseURL != "https://mygit.example.com" {
		t.Errorf("expected trailing slash stripped, got %q", c.BaseURL)
	}
}

// ---------------------------------------------------------------------------
// GetProject — error paths
// ---------------------------------------------------------------------------

func TestGetProject_HTTPError(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	_, err := c.GetProject("ns/myproject")
	if err == nil {
		t.Fatal("expected error on HTTP 404, got nil")
	}
}

// ---------------------------------------------------------------------------
// RevokeProjectToken — HTTP 200 accepted as success alongside 204
// ---------------------------------------------------------------------------

func TestRevokeProjectToken_HTTP200(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Some GitLab versions return 200 instead of 204.
		w.WriteHeader(http.StatusOK)
	})
	if err := c.RevokeProjectToken(7, 99); err != nil {
		t.Fatalf("unexpected error on HTTP 200: %v", err)
	}
}
