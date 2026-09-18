package registry_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/katakalyst/devsys/internal/registry"
)

// withTestServer points registry.HTTPClient at ts for the duration of t,
// restoring the original client on cleanup. Uses an httptest.NewTLSServer
// since registry.go always requests https://.
func withTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)

	orig := registry.HTTPClient
	registry.HTTPClient = ts.Client()
	t.Cleanup(func() { registry.HTTPClient = orig })

	// ts.URL is "https://127.0.0.1:PORT" — strip the scheme to get a bare
	// host:port usable as an image location's host segment.
	host := ts.URL[len("https://"):]
	return ts, host
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestLatestTag_NoAuthRequired(t *testing.T) {
	_, host := withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/katakalyst/devsys-base/tags/list" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]interface{}{
			"name": "katakalyst/devsys-base",
			"tags": []string{"1.0.0", "1.4.0", "1.2.0", "latest"},
		})
	})

	tag, err := registry.LatestTag(host + "/katakalyst/devsys-base")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "1.4.0" {
		t.Errorf("LatestTag() = %q; want %q", tag, "1.4.0")
	}
}

func TestLatestTag_VPrefixedTags(t *testing.T) {
	_, host := withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"tags": []string{"v1.0.0", "v2.3.1", "v2.1.0"},
		})
	})

	tag, err := registry.LatestTag(host + "/ns/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v2.3.1" {
		t.Errorf("LatestTag() = %q; want %q", tag, "v2.3.1")
	}
}

func TestLatestTag_AuthChallengeFlow(t *testing.T) {
	var tokenRequested bool
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequested = true
		if r.URL.Query().Get("scope") != "repository:ns/repo:pull" {
			t.Errorf("unexpected scope in token request: %v", r.URL.Query())
		}
		writeJSON(w, map[string]interface{}{"token": "fake-token-123"})
	})
	var host string
	mux.HandleFunc("/v2/ns/repo/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token-123" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://`+host+`/token",service="test-registry",scope="repository:ns/repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]interface{}{"tags": []string{"0.9.0", "1.0.0"}})
	})

	_, h := withTestServer(t, mux.ServeHTTP)
	host = h

	tag, err := registry.LatestTag(host + "/ns/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "1.0.0" {
		t.Errorf("LatestTag() = %q; want %q", tag, "1.0.0")
	}
	if !tokenRequested {
		t.Error("expected the token endpoint to be hit for the auth challenge")
	}
}

func TestLatestTag_NoSemverTags_ReturnsError(t *testing.T) {
	_, host := withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"tags": []string{"latest", "dev", "main"}})
	})

	_, err := registry.LatestTag(host + "/ns/repo")
	if err == nil {
		t.Fatal("expected error when no tags are semver-formatted")
	}
}

func TestLatestTag_RegistryError(t *testing.T) {
	_, host := withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	_, err := registry.LatestTag(host + "/ns/repo")
	if err == nil {
		t.Fatal("expected error on HTTP 404")
	}
}

func TestLatestTag_InvalidLocation(t *testing.T) {
	_, err := registry.LatestTag("no-slash-at-all")
	if err == nil {
		t.Fatal("expected error for a location with no repo path")
	}
}

func TestLatestReference(t *testing.T) {
	_, host := withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"tags": []string{"1.0.0", "1.1.0"}})
	})

	ref, err := registry.LatestReference(host + "/ns/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := host + "/ns/repo:1.1.0"
	if ref != want {
		t.Errorf("LatestReference() = %q; want %q", ref, want)
	}
}
