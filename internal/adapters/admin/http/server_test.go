package adminhttp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/adapters/auth/users"
)

// testServer wires a real *Server backed by real users + sessions
// stores against an in-process httptest.Server. Returned bootstrap
// password is the operator-equivalent secret.
type testServer struct {
	srv      *httptest.Server
	users    *users.Store
	sessions *users.SessionStore
	password string // bootstrap admin password
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.toml")
	store, boot, err := users.New(usersPath)
	if err != nil {
		t.Fatalf("users.New: %v", err)
	}
	if boot == nil {
		t.Fatalf("expected bootstrap on fresh dir")
	}

	ctx := context.Background()
	sessions := users.NewSessionStore(ctx, time.Hour)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := New(Deps{
		Bind:      "127.0.0.1:0", // not used by ServeMux below
		Users:     store,
		Sessions:  sessions,
		StartedAt: time.Now(),
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("adminhttp.New: %v", err)
	}

	mux := http.NewServeMux()
	srv.routes(mux)
	hs := httptest.NewServer(srv.requestLogger(mux))
	t.Cleanup(func() {
		hs.Close()
		sessions.Close()
	})

	return &testServer{
		srv:      hs,
		users:    store,
		sessions: sessions,
		password: boot.Password,
	}
}

// noFollowClient returns an HTTP client that does NOT follow
// redirects, so tests can assert on the redirect itself.
func noFollowClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Jar: jar,
	}
}

func newJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return jar
}

func TestServer_Healthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.srv.URL + "/admin/healthz")
	if err != nil {
		t.Fatalf("GET /admin/healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestServer_DashboardRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))
	resp, err := client.Get(ts.srv.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/login") {
		t.Fatalf("Location = %q, want /admin/login...", loc)
	}
}

func TestServer_LoginGet_RendersForm(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))
	resp, err := client.Get(ts.srv.URL + "/admin/login")
	if err != nil {
		t.Fatalf("GET /admin/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Login form should set the login-CSRF cookie.
	var sawCSRF bool
	for _, c := range resp.Cookies() {
		if c.Name == "faultline_login_csrf" && c.Value != "" {
			sawCSRF = true
		}
	}
	if !sawCSRF {
		t.Fatal("login response did not set faultline_login_csrf cookie")
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `name="username"`) {
		t.Fatalf("login HTML missing username field: %s", body)
	}
	if !strings.Contains(string(body), `name="_csrf"`) {
		t.Fatalf("login HTML missing _csrf field: %s", body)
	}
}

func TestServer_LoginPost_NoCSRF_Rejected(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.PostForm(ts.srv.URL+"/admin/login", url.Values{
		"username": {"admin"},
		"password": {ts.password},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestServer_FullLoginAndDashboard(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))

	// 1) GET the form so we collect the login-CSRF cookie + token.
	resp, err := client.Get(ts.srv.URL + "/admin/login")
	if err != nil {
		t.Fatalf("GET form: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))
	if csrf == "" {
		t.Fatalf("could not extract _csrf from form: %s", body)
	}

	// 2) POST credentials with that CSRF token.
	form := url.Values{
		"username": {"admin"},
		"password": {ts.password},
		"_csrf":    {csrf},
	}
	resp, err = client.PostForm(ts.srv.URL+"/admin/login", form)
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("Location = %q, want /admin", loc)
	}

	// 3) GET /admin should now return 200 + dashboard content.
	resp, err = client.Get(ts.srv.URL + "/admin")
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Welcome, admin") {
		t.Fatalf("dashboard body missing welcome text: %s", body)
	}
}

func TestServer_LoginPost_WrongPassword(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))

	resp, _ := client.Get(ts.srv.URL + "/admin/login")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	form := url.Values{
		"username": {"admin"},
		"password": {"definitely-wrong"},
		"_csrf":    {csrf},
	}
	resp, err := client.PostForm(ts.srv.URL+"/admin/login", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Invalid username or password") {
		t.Fatalf("body missing error message: %s", body)
	}
}

func TestServer_Logout(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))

	// Log in first.
	resp, _ := client.Get(ts.srv.URL + "/admin/login")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	loginCSRF := extractFormCSRF(string(body))

	form := url.Values{
		"username": {"admin"},
		"password": {ts.password},
		"_csrf":    {loginCSRF},
	}
	resp, _ = client.PostForm(ts.srv.URL+"/admin/login", form)
	resp.Body.Close()

	// Pull the dashboard so we can scrape the session-CSRF token
	// from the navbar logout form.
	resp, _ = client.Get(ts.srv.URL + "/admin")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionCSRF := extractFormCSRF(string(body))
	if sessionCSRF == "" {
		t.Fatalf("dashboard missing logout CSRF: %s", body)
	}

	// POST /admin/logout with the session-CSRF token.
	resp, err := client.PostForm(ts.srv.URL+"/admin/logout", url.Values{
		"_csrf": {sessionCSRF},
	})
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", resp.StatusCode)
	}

	// Dashboard should now redirect us to login again.
	resp, err = client.Get(ts.srv.URL + "/admin")
	if err != nil {
		t.Fatalf("GET after logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post-logout status = %d, want 303", resp.StatusCode)
	}
}

func TestServer_Logout_RejectsBadCSRF(t *testing.T) {
	ts := newTestServer(t)
	client := noFollowClient(newJar(t))

	// Log in.
	resp, _ := client.Get(ts.srv.URL + "/admin/login")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	loginCSRF := extractFormCSRF(string(body))
	resp, _ = client.PostForm(ts.srv.URL+"/admin/login", url.Values{
		"username": {"admin"},
		"password": {ts.password},
		"_csrf":    {loginCSRF},
	})
	resp.Body.Close()

	// POST /admin/logout with the WRONG csrf.
	resp, err := client.PostForm(ts.srv.URL+"/admin/logout", url.Values{
		"_csrf": {"definitely-not-the-right-token"},
	})
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestServer_StaticAssetsServe(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{
		"/admin/static/htmx.min.js",
		"/admin/static/daisyui.css",
		"/admin/static/tailwind.js",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			if n < 1024 {
				t.Fatalf("asset suspiciously small (%d bytes)", n)
			}
		})
	}
}

// extractFormCSRF pulls the value out of <input ... name="_csrf"
// value="...">. Lightweight regex-equivalent scanning so we don't
// pull in an HTML parser for two test cases.
func extractFormCSRF(html string) string {
	const marker = `name="_csrf" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
