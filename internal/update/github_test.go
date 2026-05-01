package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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

func mockGitHub(t *testing.T, releaseJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releaseJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}
