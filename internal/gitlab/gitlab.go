package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is a minimal GitLab REST API client.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	// DryRun, when true, skips all mutating API calls (POST/DELETE) and
	// returns placeholder values. Set automatically in dev builds via
	// NewClientFromEnv or by the dev build tag init in dryrun_dev.go.
	DryRun bool
}

// NewClient creates a GitLab client. baseURL defaults to https://gitlab.com.
// In dev builds (go build -tags dev) DryRun is automatically set to true.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
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

	req, err := http.NewRequest(method, c.BaseURL+"/api/v4"+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
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

// CreateProject creates a new GitLab project and returns its ID and web URL.
func (c *Client) CreateProject(name string) (projectID int, webURL string, err error) {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitLab POST /projects name=%q\n", name)
		return 0, "https://gitlab.com/dry-run/" + name, nil
	}
	payload := map[string]interface{}{
		"name":       name,
		"visibility": "private",
	}
	body, status, err := c.do("POST", "/projects", payload)
	if err != nil {
		return 0, "", err
	}
	if status != http.StatusCreated {
		return 0, "", fmt.Errorf("GitLab create project returned HTTP %d: %s", status, body)
	}

	var result struct {
		ID     int    `json:"id"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, "", fmt.Errorf("cannot parse create project response: %w", err)
	}
	return result.ID, result.WebURL, nil
}

// CreateProjectToken creates an access token for a project.
// accessLevel: 40 = Maintainer.
// expiresAt: ISO-8601 date string (YYYY-MM-DD).
// Returns the token ID and the secret token value.
func (c *Client) CreateProjectToken(projectID int, name string, scopes []string, accessLevel int, expiresAt string) (tokenID int, token string, err error) {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitLab POST /projects/%d/access_tokens name=%q scopes=%v accessLevel=%d expiresAt=%s\n",
			projectID, name, scopes, accessLevel, expiresAt)
		return 0, "dry-run-token-value", nil
	}
	payload := map[string]interface{}{
		"name":         name,
		"scopes":       scopes,
		"access_level": accessLevel,
		"expires_at":   expiresAt,
	}
	path := fmt.Sprintf("/projects/%d/access_tokens", projectID)
	body, status, err := c.do("POST", path, payload)
	if err != nil {
		return 0, "", err
	}
	if status != http.StatusCreated {
		return 0, "", fmt.Errorf("GitLab create token returned HTTP %d: %s", status, body)
	}

	var result struct {
		ID    int    `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, "", fmt.Errorf("cannot parse create token response: %w", err)
	}
	return result.ID, result.Token, nil
}

// RevokeProjectToken deletes an access token from a project.
func (c *Client) RevokeProjectToken(projectID, tokenID int) error {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitLab DELETE /projects/%d/access_tokens/%d\n", projectID, tokenID)
		return nil
	}
	path := fmt.Sprintf("/projects/%d/access_tokens/%d", projectID, tokenID)
	body, status, err := c.do("DELETE", path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("GitLab revoke token returned HTTP %d: %s", status, body)
	}
	return nil
}

// GetProject looks up a project by its web URL or namespace/path and returns its ID.
func (c *Client) GetProject(nameOrURL string) (int, error) {
	// Extract namespace/path from a full URL if given.
	namespacePath := nameOrURL
	if strings.HasPrefix(nameOrURL, "http://") || strings.HasPrefix(nameOrURL, "https://") {
		parsed, err := url.Parse(nameOrURL)
		if err != nil {
			return 0, fmt.Errorf("cannot parse GitLab URL %s: %w", nameOrURL, err)
		}
		namespacePath = strings.TrimPrefix(parsed.Path, "/")
		namespacePath = strings.TrimSuffix(namespacePath, ".git")
	}
	// Also handle SSH remote URLs like git@gitlab.com:namespace/project.git
	if strings.HasPrefix(nameOrURL, "git@") {
		// git@gitlab.com:namespace/project.git → namespace/project
		parts := strings.SplitN(nameOrURL, ":", 2)
		if len(parts) == 2 {
			namespacePath = strings.TrimSuffix(parts[1], ".git")
		}
	}

	encoded := url.PathEscape(namespacePath)
	body, status, err := c.do("GET", "/projects/"+encoded, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GitLab get project returned HTTP %d: %s", status, body)
	}

	var result struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("cannot parse project response: %w", err)
	}
	return result.ID, nil
}

// GetProjectTokens returns all access tokens for a project.
func (c *Client) GetProjectTokens(projectID int) ([]ProjectToken, error) {
	path := fmt.Sprintf("/projects/%d/access_tokens", projectID)
	body, status, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GitLab list tokens returned HTTP %d: %s", status, body)
	}

	var tokens []ProjectToken
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("cannot parse token list: %w", err)
	}
	return tokens, nil
}

// ProjectToken is a partial representation of a GitLab project access token.
type ProjectToken struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
	Revoked   bool   `json:"revoked"`
}
