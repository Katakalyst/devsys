// Package registry looks up the newest published tag for an image location
// (registry host + repository path, no tag) directly against the registry
// itself, using the OCI Distribution Spec's tag-listing endpoint. This is
// deliberately registry-agnostic — it works the same way regardless of
// whether the image lives on GHCR, GitLab's container registry, Docker Hub,
// or anywhere else, since GET /v2/<name>/tags/list and the anonymous bearer
// token challenge flow are both standard, not tied to any one host.
//
// devsys CLI Spec, Section 12.3–12.4.
package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// HTTPClient is the client used for all registry requests. A var, not a
// package-level http.DefaultClient use, so tests can point it at a local
// httptest server via a custom Transport if ever needed; in practice tests
// instead override BaseScheme/host through the location argument itself.
var HTTPClient = &http.Client{Timeout: 15 * time.Second}

// LatestReference returns the full, directly-usable reference (location +
// ":" + newest tag) for an image location such as
// "ghcr.io/katakalyst/devsys-base".
func LatestReference(location string) (string, error) {
	tag, err := LatestTag(location)
	if err != nil {
		return "", err
	}
	return location + ":" + tag, nil
}

// LatestTag queries the registry hosting location for every tag under that
// repository and returns the highest semver-formatted one, exactly as
// published (a "v" prefix is tolerated for comparison but not required or
// added).
func LatestTag(location string) (string, error) {
	host, repoPath, err := splitLocation(location)
	if err != nil {
		return "", err
	}
	tags, err := listTags(host, repoPath)
	if err != nil {
		return "", err
	}
	return highestSemver(tags)
}

// splitLocation splits "host/repo/path" into host and repo/path.
func splitLocation(location string) (host, repoPath string, err error) {
	parts := strings.SplitN(location, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid image location %q: expected host/repo", location)
	}
	return parts[0], parts[1], nil
}

// listTags fetches the tag list for a repository, following the standard
// registry token-authentication challenge (RFC-shaped WWW-Authenticate:
// Bearer) if the registry requires it even for anonymous/public read access
// — GHCR and most public registries do.
func listTags(host, repoPath string) ([]string, error) {
	tagsURL := fmt.Sprintf("https://%s/v2/%s/tags/list", host, repoPath)

	resp, err := HTTPClient.Get(tagsURL)
	if err != nil {
		return nil, fmt.Errorf("cannot reach registry %s: %w", host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		token, terr := fetchAnonymousToken(resp.Header.Get("WWW-Authenticate"))
		if terr != nil {
			return nil, fmt.Errorf("registry requires auth and token fetch failed: %w", terr)
		}
		req, err := http.NewRequest(http.MethodGet, tagsURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp2, err := HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cannot reach registry %s: %w", host, err)
		}
		defer resp2.Body.Close()
		resp = resp2
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry %s returned HTTP %d: %s", host, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("cannot parse tags list response from %s: %w", host, err)
	}
	return result.Tags, nil
}

// fetchAnonymousToken implements the registry token-auth challenge: parse
// the WWW-Authenticate header's realm/service/scope, request a token from
// that realm with those params, and return it. This is the standard
// anonymous-pull flow used by GHCR, Docker Hub, and most other registries —
// not specific to any one of them.
func fetchAnonymousToken(wwwAuth string) (string, error) {
	params, err := parseWWWAuthenticate(wwwAuth)
	if err != nil {
		return "", err
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in WWW-Authenticate header %q", wwwAuth)
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("cannot parse token realm %q: %w", realm, err)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()

	resp, err := HTTPClient.Get(u.String())
	if err != nil {
		return "", fmt.Errorf("cannot reach token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cannot parse token response: %w", err)
	}
	if result.Token != "" {
		return result.Token, nil
	}
	if result.AccessToken != "" {
		return result.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint response had no token/access_token field")
}

// parseWWWAuthenticate parses a header of the form:
//
//	Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:x/y:pull"
func parseWWWAuthenticate(header string) (map[string]string, error) {
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, fmt.Errorf("unsupported WWW-Authenticate scheme: %q", header)
	}
	params := map[string]string{}
	rest := strings.TrimPrefix(header, "Bearer ")
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		params[kv[0]] = strings.Trim(kv[1], `"`)
	}
	return params, nil
}

// highestSemver returns the tag, exactly as given in tags, whose value
// compares highest under semver ordering. A missing "v" prefix is tolerated
// for the comparison (most image tags are published as "1.4.0", not
// "v1.4.0") but the returned string is always the original, unmodified tag.
// Non-semver tags (e.g. "latest") are ignored.
func highestSemver(tags []string) (string, error) {
	var best, bestNorm string
	for _, t := range tags {
		norm := t
		if !strings.HasPrefix(norm, "v") {
			norm = "v" + norm
		}
		if !semver.IsValid(norm) {
			continue
		}
		if bestNorm == "" || semver.Compare(norm, bestNorm) > 0 {
			bestNorm = norm
			best = t
		}
	}
	if best == "" {
		return "", fmt.Errorf("no semver-formatted tags found among %v", tags)
	}
	return best, nil
}
