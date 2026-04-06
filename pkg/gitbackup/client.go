package gitbackup

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// rust4gitClient pushes files to a rust4git instance via the State API.
// Uses Bearer token authentication with automatic renewal.
type rust4gitClient struct {
	baseURL  string // e.g. "http://git.gt.lo"
	repoName string // e.g. "mkube/configstate"
	branch   string
	author   string
	email    string

	tokenMu      sync.RWMutex
	token        string
	tokenFile    string // path to persist renewed tokens
	client       *http.Client
}

func newClient(baseURL, repoName, branch, author, email, username, password string, passwordFile string, insecureTLS bool) *rust4gitClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
	}
	return &rust4gitClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		repoName: repoName,
		branch:   branch,
		author:   author,
		email:    email,
		token:    password,
		tokenFile: passwordFile,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			// Don't follow redirects — 303 means auth failure
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// setAuth adds Bearer token auth to a request.
func (c *rust4gitClient) setAuth(req *http.Request) {
	c.tokenMu.RLock()
	tok := c.token
	c.tokenMu.RUnlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// RenewToken calls POST /api/auth/renew to get a fresh token.
// On success, updates the in-memory token and persists to tokenFile.
func (c *rust4gitClient) RenewToken() error {
	u := c.baseURL + "/api/auth/renew"

	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return fmt.Errorf("creating renew request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("renewing token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("token renewal rejected (status %d) — token may be expired", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token renewal: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parsing renewal response: %w", err)
	}
	if result.Token == "" {
		return fmt.Errorf("renewal returned empty token")
	}

	c.tokenMu.Lock()
	c.token = result.Token
	c.tokenMu.Unlock()

	// Persist to file so it survives restarts
	if c.tokenFile != "" {
		if err := os.WriteFile(c.tokenFile, []byte(result.Token+"\n"), 0600); err != nil {
			return fmt.Errorf("persisting renewed token: %w", err)
		}
	}

	return nil
}

// pushFile pushes a single file to the repo via the State API.
// Returns the commit short ID on success.
func (c *rust4gitClient) pushFile(path, message string, content []byte) (string, error) {
	u := fmt.Sprintf("%s/api/repos/%s/state/%s", c.baseURL, c.repoName, path)

	params := url.Values{}
	params.Set("branch", c.branch)
	params.Set("message", message)
	params.Set("author", c.author)
	params.Set("email", c.email)

	req, err := http.NewRequest("POST", u+"?"+params.Encode(), strings.NewReader(string(content)))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pushing %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther {
		return "", fmt.Errorf("push %s: auth failed (303 redirect to login)", path)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("push %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	return "", nil
}

// ensureRepo creates the repo if it doesn't exist. Ignores conflict errors.
func (c *rust4gitClient) ensureRepo() error {
	u := fmt.Sprintf("%s/api/repos", c.baseURL)
	body := fmt.Sprintf(`{"name":%q,"visibility":"private"}`, c.repoName)

	req, err := http.NewRequest("POST", u, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating repo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther {
		return fmt.Errorf("auth failed (303 redirect to login) — check token")
	}
	// 201 = created, 409/400/200 = already exists
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create repo: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// getFile retrieves a file from the repo. Returns content and nil error, or nil and error.
func (c *rust4gitClient) getFile(path string) ([]byte, error) {
	u := fmt.Sprintf("%s/api/repos/%s/state/%s?branch=%s", c.baseURL, c.repoName, path, url.QueryEscape(c.branch))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode == http.StatusSeeOther {
		return nil, fmt.Errorf("auth failed (303 redirect to login)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: status %d", path, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
