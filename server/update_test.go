package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache protects the GitHub API quota: unauthenticated requests are
// limited to 60 per IP per hour, while the "new version available" banner checks
// on every full-page load. Without caching, several tabs could exhaust the quota.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("five lookups should make one remote request, got %d", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// An explicit update check must return fresh data so newly published versions
	// are visible without waiting for the cache to expire.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass the cache and make a second remote request, got %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Move the cached timestamp back to simulate an expired TTL.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expired TTL should trigger a second remote request, got %d", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("GitHub unavailable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error")
	}
	// Cache failures briefly so an unreachable GitHub does not cause every page
	// load to wait through another timeout.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("error should be cached briefly; expected one remote request, got %d", calls)
	}

	// Error TTL must be much shorter than the success TTL so recovery is detected quickly.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL (%v) must be shorter than success TTL (%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 2 {
		t.Errorf("expired error TTL should trigger a retry; expected two remote requests, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// Closing a tab cancels its request, but does not indicate a GitHub failure.
	// Never cache cancellation, or every visitor would see a misleading error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // Expire the cache to force a remote lookup.

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("a canceled caller should receive the cancellation error")
	}

	// Key invariant: cancellation leaves no trace—the error is not cached, and
	// the previous successful result is retained.
	if c.err != nil {
		t.Fatalf("cancellation error should not be cached, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("cache should retain the previous successful result, got %+v", c.rel)
	}

	// Since cancellation fetched no new data, the next caller should make a new
	// remote request and receive a normal result.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("normal request after cancellation should not fail: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("expected a normal result, got %+v", rel)
	}
}
