package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// githubClient is a tiny client for the subset of GitHub releases API
// the updater needs. No external SDK because we only consume two
// endpoints and want to control timeouts + auth knobs.
type githubClient struct {
	apiBase string // default https://api.github.com; overridden in tests
	repo    string // "owner/name"
	http    *http.Client
}

func newGitHubClient(repo string) *githubClient {
	return &githubClient{
		apiBase: "https://api.github.com",
		repo:    repo,
		http: &http.Client{
			// Long enough for slow networks; short enough to fail fast on
			// dead hosts.
			Timeout: 30 * time.Second,
		},
	}
}

// Release is the trimmed-down GitHub release shape we consume. Fields
// not used here (body, author, etc.) are ignored on decode.
type Release struct {
	TagName    string         `json:"tag_name"`
	Name       string         `json:"name"`
	Prerelease bool           `json:"prerelease"`
	Draft      bool           `json:"draft"`
	HTMLURL    string         `json:"html_url"`
	Assets     []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is one downloadable artifact attached to a release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// FindAsset returns the asset matching name, or nil.
func (r *Release) FindAsset(name string) *ReleaseAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// Latest fetches the latest non-draft release. When allowPrerelease is
// false and the latest is a prerelease, returns nil with no error
// (the caller treats this as "no eligible release available").
func (c *githubClient) Latest(ctx context.Context, allowPrerelease bool) (*Release, error) {
	rel, err := c.getRelease(ctx, "/releases/latest")
	if err != nil {
		return nil, err
	}
	if rel == nil {
		return nil, nil // 404, no release published yet
	}
	if rel.Draft {
		return nil, nil
	}
	if rel.Prerelease && !allowPrerelease {
		return nil, nil
	}
	return rel, nil
}

func (c *githubClient) getRelease(ctx context.Context, path string) (*Release, error) {
	url := c.apiBase + "/repos/" + c.repo + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Pin to v3 (current stable) explicitly; future-proofing.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "faultline-updater")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Repo or release missing. Treat as "no release available"
		// rather than an error so a misconfigured repo doesn't crash
		// the updater on every poll.
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github releases: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// download fetches a URL into out, with progress and a context-bound
// HTTP request. Caller is responsible for closing out.
func (c *githubClient) download(ctx context.Context, url string, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "faultline-updater")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download %s: HTTP %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return nil
}
