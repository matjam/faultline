package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestLatest_FiltersDraft(t *testing.T) {
	srv := mockGitHub(t, `{"tag_name":"v1.0.0","name":"v1.0.0","draft":true,"prerelease":false,"assets":[]}`)
	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	rel, err := c.Latest(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("draft release should be filtered out, got %+v", rel)
	}
}

func TestLatest_FiltersPrereleaseByDefault(t *testing.T) {
	srv := mockGitHub(t, `{"tag_name":"v1.0.0-rc.1","name":"v1.0.0-rc.1","prerelease":true,"assets":[]}`)
	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	rel, err := c.Latest(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("prerelease should be filtered, got %+v", rel)
	}
}

func TestLatest_AllowPrerelease(t *testing.T) {
	srv := mockGitHub(t, `{"tag_name":"v1.0.0-rc.1","name":"v1.0.0-rc.1","prerelease":true,"assets":[]}`)
	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	rel, err := c.Latest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil {
		t.Fatal("expected prerelease to be returned when allowed")
	}
	if rel.TagName != "v1.0.0-rc.1" {
		t.Errorf("got tag %q", rel.TagName)
	}
}

func TestLatest_NotFoundIsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	rel, err := c.Latest(context.Background(), false)
	if err != nil {
		t.Errorf("404 should not error, got %v", err)
	}
	if rel != nil {
		t.Errorf("expected nil release on 404, got %+v", rel)
	}
}

func TestRelease_FindAsset(t *testing.T) {
	r := &Release{Assets: []ReleaseAsset{
		{Name: "alpha.tar.gz"},
		{Name: "beta.tar.gz"},
	}}
	if a := r.FindAsset("beta.tar.gz"); a == nil || a.Name != "beta.tar.gz" {
		t.Errorf("FindAsset returned %+v", a)
	}
	if a := r.FindAsset("missing"); a != nil {
		t.Errorf("expected nil for missing asset, got %+v", a)
	}
}

func TestLatest_RateLimited(t *testing.T) {
	resetAt := time.Now().Add(45 * time.Minute).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	_, err := c.Latest(context.Background(), false)
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rle.ResetAt.Unix() != resetAt {
		t.Errorf("ResetAt = %d, want %d", rle.ResetAt.Unix(), resetAt)
	}
}

func TestLatest_403WithoutRateLimitHeadersIsGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 403 with no X-RateLimit-* headers (e.g. permission error,
		// not a rate-limit) must NOT be classified as a rate-limit
		// error -- otherwise we'd back off for hours on a benign
		// permission glitch.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer srv.Close()

	c := newGitHubClient("o/r")
	c.apiBase = srv.URL
	_, err := c.Latest(context.Background(), false)
	if err == nil {
		t.Fatal("expected error")
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		t.Errorf("403 without rate-limit headers should not classify as RateLimitError: %v", err)
	}
}

func TestParseRateLimit_MissingResetHeader(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-RateLimit-Remaining", "0")
	rle := parseRateLimit(resp, "body")
	if rle == nil {
		t.Fatal("expected non-nil RateLimitError when remaining=0")
	}
	if !rle.ResetAt.IsZero() {
		t.Errorf("ResetAt should be zero when header is missing, got %v", rle.ResetAt)
	}
}

func mockGitHub(t *testing.T, releaseJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releaseJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}
