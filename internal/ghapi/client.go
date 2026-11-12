// Package ghapi is a small GitHub REST client for the Actions endpoints the
// history reports need: workflow runs, their jobs, job logs, and the
// repository cache list and usage. It uses net/http only, follows Link-header
// pagination, and keeps completed-run data in an optional on-disk cache so a
// repeated report does not re-walk the API.
package ghapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// apiVersion pins the REST API version sent with every request.
const apiVersion = "2022-11-28"

// Client talks to one repository's Actions API.
type Client struct {
	BaseURL string // API root, no trailing slash
	Token   string // optional bearer token
	Owner   string
	Repo    string
	HTTP    *http.Client
	// Cache stores completed-run data on disk. Nil disables it.
	Cache *DiskCache
}

// NewClient builds a client for "owner/repo". The API root comes from
// GITHUB_API_URL (set by Actions runners, and on GHES) and falls back to
// api.github.com; the token from GITHUB_TOKEN, then GH_TOKEN.
func NewClient(fullName string) (*Client, error) {
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repo must be owner/name, got %q", fullName)
	}
	base := strings.TrimRight(os.Getenv("GITHUB_API_URL"), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	return &Client{
		BaseURL: base,
		Token:   token,
		Owner:   owner,
		Repo:    repo,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// FullName returns "owner/repo".
func (c *Client) FullName() string { return c.Owner + "/" + c.Repo }

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	URL     string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GET %s: %d %s", e.URL, e.Status, e.Message)
}

// NotFound reports whether err is a 404 or 410 (e.g. logs past retention).
func NotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone)
}

// get performs one GET against an absolute URL or an API path and returns the
// response; the caller closes the body. Non-2xx is turned into *APIError.
func (c *Client) get(rawURL string) (*http.Response, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = c.BaseURL + rawURL
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "gha-cache-optimizer")
	if c.Token != "" {
		// net/http drops Authorization when a redirect leaves the host, so the
		// signed log download URL never sees the token.
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		msg = parsed.Message
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			var sec int64
			if _, err := fmt.Sscan(reset, &sec); err == nil {
				msg += fmt.Sprintf(" (rate limit resets at %s)", time.Unix(sec, 0).UTC().Format(time.RFC3339))
			}
		}
	}
	return nil, &APIError{Status: resp.StatusCode, URL: rawURL, Message: msg}
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getJSON fetches one page into v and returns the rel="next" URL, if any.
func (c *Client) getJSON(rawURL string, v any) (next string, err error) {
	resp, err := c.get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return "", fmt.Errorf("decode %s: %w", rawURL, err)
	}
	if m := nextLinkRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		next = m[1]
	}
	return next, nil
}

// repoPath builds /repos/{owner}/{repo}/<suffix> with escaped segments.
func (c *Client) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + suffix
}
