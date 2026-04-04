package gitbackup

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// rust4gitClient pushes files to a rust4git instance via the State API.
type rust4gitClient struct {
	baseURL  string // e.g. "http://git.gt.lo:3000"
	repoName string // e.g. "mkube/configstate"
	branch   string
	author   string
	email    string
	username string
	password string
	client   *http.Client
}

func newClient(baseURL, repoName, branch, author, email, username, password string, insecureTLS bool) *rust4gitClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
	}
	return &rust4gitClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		repoName: repoName,
		branch:   branch,
		author:   author,
		email:    email,
		username: username,
		password: password,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
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
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pushing %s: %w", path, err)
	}
	defer resp.Body.Close()

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
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating repo: %w", err)
	}
	defer resp.Body.Close()

	// 201 = created, 409 or 200 = already exists
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
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
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: status %d", path, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
