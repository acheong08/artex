package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo is the release source. It is hard-coded rather than configurable: allowing
// users who can edit config to change the update source would create a remote-code-
// execution path, which is unacceptable for a penetration-testing platform.
const Repo = "Autumn-27/artex"

// latestURL is GitHub's "latest stable release" endpoint, which skips prereleases and drafts.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts limits the domains accessible during updates. Along with checkRedirect,
// any redirect to a host outside the allowlist fails immediately. This is the first
// safeguard against DNS poisoning / MITM binary replacement; SHA256SUMS verification
// is the second.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // Object storage hosting release assets.
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release contains the fields we use from a GitHub Release.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset is a file attached to a Release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient creates an HTTP client that accepts only GitHub domains. An empty proxy
// means a direct connection.
//
// Deliberately do not reuse the default Transport: updates must use TLS and verify
// certificates, unaffected by settings such as InsecureSkipVerify elsewhere.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // Downloads the full package; a per-request timeout would be too restrictive.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL enforces HTTPS and the domain allowlist.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("non-HTTPS URL is not allowed: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("non-GitHub host is not allowed: %s", u.Hostname())
	}
	return nil
}

// FetchLatest retrieves the latest stable release.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach GitHub (configure a global proxy in system settings if needed): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// Unauthenticated GitHub API requests are limited to 60 per IP per hour, easily
		// reached when multiple clients share an outbound IP.
		return nil, fmt.Errorf("GitHub API rate limit reached (60 requests per hour); try again later")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("repository %s has not published a stable release yet", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("release is missing a tag")
	}
	return &rel, nil
}

// AssetName returns the release-package name for the current platform, matching
// package_binary in build.sh: artex-<version>-<os>-<arch>.zip (without the v prefix).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset finds an asset by name in a Release.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
