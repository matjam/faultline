package adminhttp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/adapters/auth/users"
)

// TestPages_RenderForEverySection asserts that every authenticated
// section route returns 200 with the section title in the body. This
// catches template parse / data binding regressions across the
// dashboard/configuration/subagents/skills/version/logs pages with a
// single sweep.
func TestPages_RenderForEverySection(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	cases := []struct {
		path     string
		contains string
	}{
		{"/admin", "status overview"},
		{"/admin/configuration", "configuration"},
		{"/admin/subagents", "subagent stream"},
		{"/admin/skills", "skills catalog"},
		{"/admin/version", "version &amp; updates"},
		{"/admin/logs", "log stream"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := client.Get(ts.srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), c.contains) {
				t.Errorf("body missing %q\n--- body ---\n%s",
					c.contains, body)
			}
		})
	}
}

// TestPages_SidebarHighlightsActive checks that each page surfaces an
// active CSS class on its corresponding sidebar entry. Regression
// guard against a Section field that doesn't get plumbed through the
// pageData struct.
func TestPages_SidebarHighlightsActive(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	cases := []struct {
		path  string
		label string
	}{
		{"/admin", "dashboard"},
		{"/admin/configuration", "configuration"},
		{"/admin/subagents", "subagent"},
		{"/admin/skills", "skills"},
		{"/admin/version", "version"},
		{"/admin/logs", "logs"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := client.Get(ts.srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			// Look for a sidebar anchor with class "active"
			// containing the section label. The exact
			// markup is governed by layout.html; we just
			// search for the combination.
			marker := `class="active"`
			if !strings.Contains(string(body), marker) {
				t.Fatalf("no active sidebar entry on %s: %s",
					c.path, body)
			}
			// Active row should contain the right label
			// somewhere downstream of the active class.
			start := strings.Index(string(body), marker)
			tail := string(body)[start:]
			if !strings.Contains(tail, c.label) {
				t.Errorf("active sidebar row near %s does not mention %q",
					c.path, c.label)
			}
		})
	}
}

func TestPages_LogsFragmentReadsTodayFile(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed today's log file with a few lines.
	today := todayFilename(t)
	logPath := filepath.Join(dir, today)
	if err := os.WriteFile(logPath, []byte(
		"line=1 level=INFO msg=hello\n"+
			"line=2 level=WARN msg=careful\n"+
			"line=3 level=ERROR msg=oops\n"), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	ts := newAdminTestServerWithLogDir(t, dir)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/logs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{"hello", "careful", "oops"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
	if !strings.Contains(got, `lvl-error`) {
		t.Errorf("missing error severity class:\n%s", got)
	}
	if !strings.Contains(got, `lvl-warn`) {
		t.Errorf("missing warn severity class:\n%s", got)
	}
}

func TestPages_LogsFragmentMissingFile(t *testing.T) {
	dir := t.TempDir() // no log file
	ts := newAdminTestServerWithLogDir(t, dir)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/logs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no log entries yet") {
		t.Errorf("expected empty placeholder, got: %s", body)
	}
}

func TestConfigurationSave_PersistsAndPromptsRestart(t *testing.T) {
	c := &fakeConfigStore{
		path:    "/x/config.toml",
		content: []byte(""),
	}
	ts := newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, nil, c)
	client := loggedInClient(t, ts)

	// Pull the configuration page so we can extract a CSRF token.
	resp, _ := client.Get(ts.srv.URL + "/admin/configuration")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))
	if csrf == "" {
		t.Fatalf("no CSRF in configuration page: %s", body)
	}

	// Submit a small change.
	resp, err := client.PostForm(ts.srv.URL+"/admin/configuration/save",
		url.Values{
			"_csrf":            {csrf},
			"api.url":          {"http://new.example/v1"},
			"agent.max_tokens": {"123456"},
		})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)

	if c.lastWritten == nil {
		t.Fatalf("Write was not called")
	}
	written := string(c.lastWritten)
	if !strings.Contains(written, "http://new.example/v1") {
		t.Errorf("written TOML missing api.url override:\n%s", written)
	}
	if !strings.Contains(written, "123456") {
		t.Errorf("written TOML missing max_tokens override:\n%s", written)
	}
	if !strings.Contains(string(body), "Saved") {
		t.Errorf("expected saved flash:\n%s", body)
	}
	if !strings.Contains(string(body), "restart agent now") {
		t.Errorf("expected restart button after save:\n%s", body)
	}
}

func TestConfigurationSave_RequiresCSRF(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml"}
	ts := newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, nil, c)
	client := loggedInClient(t, ts)

	resp, err := client.PostForm(ts.srv.URL+"/admin/configuration/save",
		url.Values{"api.url": {"x"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestConfigRestart_TriggersShutdown(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml"}
	ts := newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, nil, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin/configuration")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/restart",
		url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if c.restartCalls != 1 {
		t.Fatalf("Restart calls = %d, want 1", c.restartCalls)
	}
}

// --- helpers --------------------------------------------------------

// fakeConfigStore is the test double for ConfigStore. Restored from
// the deleted handlers_config_test.go since the new tests still use
// it.
type fakeConfigStore struct {
	path    string
	content []byte
	readErr error

	validateErr  error
	writeErr     error
	restartCalls int
	lastWritten  []byte
}

func (f *fakeConfigStore) Path() string { return f.path }
func (f *fakeConfigStore) Read() ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]byte, len(f.content))
	copy(out, f.content)
	return out, nil
}
func (f *fakeConfigStore) Validate(raw []byte) error {
	return f.validateErr
}
func (f *fakeConfigStore) Write(raw []byte) error {
	if f.validateErr != nil {
		return f.validateErr
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	f.content = append([]byte(nil), raw...)
	f.lastWritten = f.content
	return nil
}
func (f *fakeConfigStore) Restart() { f.restartCalls++ }

// todayFilename returns the basename of today's expected log file
// (matching internal/log.Daily's naming pattern). Test-only.
func todayFilename(t *testing.T) string {
	t.Helper()
	return time.Now().Format("2006-01-02") + ".log"
}

// newAdminTestServerWithLogDir is a parallel to newAdminTestServer
// that additionally sets Deps.LogDir so the logs page has a directory
// to read from. Refactoring the common helper to take a Deps overrides
// struct would be cleaner; doing the literal duplication here keeps
// the diff for this PR contained.
func newAdminTestServerWithLogDir(t *testing.T, logDir string) *testServer {
	t.Helper()
	dir := t.TempDir()
	store, boot, err := users.New(filepath.Join(dir, "users.toml"))
	if err != nil {
		t.Fatalf("users.New: %v", err)
	}
	if boot == nil {
		t.Fatalf("expected bootstrap on fresh dir")
	}
	sessions := users.NewSessionStore(context.Background(), time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := New(Deps{
		Bind:      "127.0.0.1:0",
		Users:     store,
		Sessions:  sessions,
		StartedAt: time.Now(),
		Logger:    logger,
		LogDir:    logDir,
		Tools:     NewToolBuffer(8),
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
	return &testServer{srv: hs, users: store, sessions: sessions, password: boot.Password}
}
