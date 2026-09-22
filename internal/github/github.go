package github

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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

// CreateDeployKey registers a public key as a deploy key on a GitHub
// repository. readWrite true grants push access; false is read-only.
// Returns the key ID assigned by GitHub.
//
// The public key must be in OpenSSH authorized_keys format (the string
// returned by GenerateDeployKeyPair). The matching private key is what gets
// stored as the Podman secret — GitHub never sees it.
func (c *Client) CreateDeployKey(owner, repo, title, publicKey string, readWrite bool) (keyID int, err error) {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitHub POST /repos/%s/%s/keys title=%q readWrite=%v\n",
			owner, repo, title, readWrite)
		return 0, nil
	}
	payload := map[string]interface{}{
		"title":     title,
		"key":       publicKey,
		"read_only": !readWrite,
	}
	path := fmt.Sprintf("/repos/%s/%s/keys", owner, repo)
	body, status, err := c.do("POST", path, payload)
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("GitHub create deploy key returned HTTP %d: %s", status, body)
	}
	var result struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("cannot parse create deploy key response: %w", err)
	}
	return result.ID, nil
}

// ListDeployKeys returns all deploy keys registered on a GitHub repository.
func (c *Client) ListDeployKeys(owner, repo string) ([]DeployKey, error) {
	path := fmt.Sprintf("/repos/%s/%s/keys", owner, repo)
	body, status, err := c.do("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GitHub list deploy keys returned HTTP %d: %s", status, body)
	}
	var keys []DeployKey
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, fmt.Errorf("cannot parse deploy key list: %w", err)
	}
	return keys, nil
}

// DeleteDeployKey removes a deploy key from a GitHub repository.
func (c *Client) DeleteDeployKey(owner, repo string, keyID int) error {
	if c.DryRun {
		fmt.Fprintf(os.Stdout, "[dry-run] GitHub DELETE /repos/%s/%s/keys/%d\n", owner, repo, keyID)
		return nil
	}
	path := fmt.Sprintf("/repos/%s/%s/keys/%d", owner, repo, keyID)
	body, status, err := c.do("DELETE", path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("GitHub delete deploy key returned HTTP %d: %s", status, body)
	}
	return nil
}

// DeployKey is a partial representation of a GitHub deploy key.
type DeployKey struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Key      string `json:"key"`
	ReadOnly bool   `json:"read_only"`
}

// GenerateDeployKeyPair generates an Ed25519 SSH keypair.
// Returns the OpenSSH PEM-encoded private key and the authorized_keys-format
// public key string. The public key is registered with GitHub; the private key
// is stored as the Podman secret and mounted into the container.
func GenerateDeployKeyPair() (privateKeyPEM, publicKey string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("cannot generate Ed25519 key: %w", err)
	}

	privBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", fmt.Errorf("cannot marshal private key: %w", err)
	}
	privateKeyPEM = string(pem.EncodeToMemory(privBlock))

	pubSSH, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("cannot create SSH public key: %w", err)
	}
	publicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubSSH)))

	return privateKeyPEM, publicKey, nil
}
