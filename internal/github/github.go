package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultAPIBase = "https://api.github.com"

// Client is a minimal GitHub REST API client.
type Client struct {
	apiBase    string
	Token      string
	HTTPClient *http.Client
	// DryRun, when true, skips all mutating API calls (POST/DELETE) and
	// returns placeholder values. Set automatically in dev builds via the
	// dev build tag init in dryrun_dev.go.
	DryRun bool
}

// NewClient creates a GitHub client using the given PAT.
// GitHub's API base URL is fixed — self-hosted GitHub Enterprise is not
// supported here (unlike GitLab, where the base URL is configurable).
func NewClient(token string) *Client {
	return NewClientWithBase(defaultAPIBase, token)
}

// NewClientWithBase creates a GitHub client with a custom base URL.
// Intended for unit tests that point the client at a local httptest server.
func NewClientWithBase(baseURL, token string) *Client {
	return &Client{
		apiBase:    baseURL,
		Token:      token,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		DryRun:     defaultDryRun,
	}
}

func (c *Client) do(method, path string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("cannot marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.apiBase+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read response body: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// CreateRepo creates a new private repository for the authenticated user.
// Returns the repository's numeric ID and its full_name ("owner/repo").
func (c *Client) CreateRepo(name string) (repoID int, fullName string, err error) {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitHub POST /user/repos name=%q\n", name)
		return 0, "dry-run/" + name, nil
	}
	payload := map[string]interface{}{
		"name":    name,
		"private": true,
	}
	body, status, err := c.do("POST", "/user/repos", payload)
	if err != nil {
		return 0, "", err
	}
	if status != http.StatusCreated {
		return 0, "", fmt.Errorf("GitHub create repo returned HTTP %d: %s", status, body)
	}
	var result struct {
		ID       int    `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, "", fmt.Errorf("cannot parse create repo response: %w", err)
	}
	return result.ID, result.FullName, nil
}

// GetRepo verifies that owner/repo exists and is reachable with this
// client's token. Used to validate an --attach target and the just-entered
// fine-grained PAT together in one call — devsys never creates a GitHub
// repo implicitly, but it must still surface a REST error directly rather
// than silently accepting a typo'd owner/repo (Git Remote & Credential Spec
// §9's error cases).
func (c *Client) GetRepo(owner, repo string) error {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitHub GET /repos/%s/%s\n", owner, repo)
		return nil
	}
	path := fmt.Sprintf("/repos/%s/%s", owner, repo)
	body, status, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GitHub get repo returned HTTP %d: %s", status, body)
	}
	return nil
}
