package adminhttp

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeConfigStore lets us drive the config handlers without a real
// TOML file or shutdown plumbing.
type fakeConfigStore struct {
	mu sync.Mutex

	path    string
	content []byte
	readErr error

	validateErr   error
	writeErr      error
	restartCalls  int
	lastWritten   []byte
	lastValidated []byte
}

func (f *fakeConfigStore) Path() string { return f.path }

func (f *fakeConfigStore) Read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]byte, len(f.content))
	copy(out, f.content)
	return out, nil
}

func (f *fakeConfigStore) Validate(raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastValidated = append([]byte(nil), raw...)
	return f.validateErr
}

func (f *fakeConfigStore) Write(raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeConfigStore) Restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartCalls++
}

func newConfigAdminServer(t *testing.T, c ConfigStore) *testServer {
	return newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, nil, c)
}

func TestFragConfig_NoStore(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/config")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "configuration store is not wired") {
		t.Fatalf("expected not-wired placeholder:\n%s", body)
	}
}

func TestFragConfig_RendersExisting(t *testing.T) {
	c := &fakeConfigStore{
		path:    "/etc/faultline/config.toml",
		content: []byte("[api]\nurl = \"http://example/v1\"\n"),
	}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/config")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"/etc/faultline/config.toml",
		"http://example/v1",
		`name="content"`,
		"Validate", "Save",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
}

func TestFragConfig_SurfacesReadErr(t *testing.T) {
	c := &fakeConfigStore{
		path:    "/missing/config.toml",
		readErr: errors.New("permission denied"),
	}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/config")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "permission denied") {
		t.Fatalf("expected read error in body:\n%s", body)
	}
	// Save / Validate buttons are disabled when the read failed.
	if !strings.Contains(string(body), "disabled") {
		t.Fatalf("expected disabled buttons after read error:\n%s", body)
	}
}

func TestConfigValidate_OK(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml", content: []byte("hello = 1\n")}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/validate", url.Values{
		"_csrf":   {csrf},
		"content": {"new = true\n"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Looks good") {
		t.Fatalf("expected validate-OK flash:\n%s", body)
	}
	// The textarea should now show the posted content, not the
	// on-disk content (we don't want to lose the operator's edits).
	if !strings.Contains(string(body), "new = true") {
		t.Fatalf("textarea did not retain posted content:\n%s", body)
	}
}

func TestConfigValidate_Error(t *testing.T) {
	c := &fakeConfigStore{
		path:        "/x/config.toml",
		content:     []byte("hello = 1\n"),
		validateErr: errors.New("toml: invalid syntax at line 1"),
	}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/validate", url.Values{
		"_csrf":   {csrf},
		"content": {"this is = nonsense ===\n"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Validation failed") {
		t.Fatalf("expected validation-failed flash:\n%s", body)
	}
	if !strings.Contains(string(body), "invalid syntax at line 1") {
		t.Fatalf("expected error text in flash:\n%s", body)
	}
}

func TestConfigSave_PersistsAndPromptsRestart(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml", content: []byte("old\n")}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/save", url.Values{
		"_csrf":   {csrf},
		"content": {"new\n"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)

	if string(c.lastWritten) != "new\n" {
		t.Fatalf("Write was called with %q, want %q", c.lastWritten, "new\n")
	}
	if !strings.Contains(string(body), "Saved") {
		t.Fatalf("expected saved flash:\n%s", body)
	}
	if !strings.Contains(string(body), "Restart agent now") {
		t.Fatalf("expected post-save restart button:\n%s", body)
	}
}

func TestConfigSave_RejectsBadConfig(t *testing.T) {
	c := &fakeConfigStore{
		path:        "/x/config.toml",
		content:     []byte("old\n"),
		validateErr: errors.New("syntax error at line 3"),
	}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/save", url.Values{
		"_csrf":   {csrf},
		"content": {"broken\n"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Save failed: syntax error") {
		t.Fatalf("expected save-failed flash:\n%s", body)
	}
	if c.lastWritten != nil {
		t.Fatalf("Write should not have been called on bad content; got %q", c.lastWritten)
	}
}

func TestConfigRestart_TriggersShutdown(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml", content: []byte("ok\n")}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/config/restart", url.Values{
		"_csrf": {csrf},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if c.restartCalls != 1 {
		t.Fatalf("Restart calls = %d, want 1", c.restartCalls)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Restart signal sent") {
		t.Fatalf("expected restart flash:\n%s", body)
	}
}

func TestConfig_RequireCSRF(t *testing.T) {
	c := &fakeConfigStore{path: "/x/config.toml", content: []byte("ok\n")}
	ts := newConfigAdminServer(t, c)
	client := loggedInClient(t, ts)

	for _, path := range []string{
		"/admin/config/validate",
		"/admin/config/save",
		"/admin/config/restart",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := client.PostForm(ts.srv.URL+path, url.Values{
				"content": {"hello\n"},
			})
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
	if c.restartCalls != 0 {
		t.Fatalf("Restart was called despite missing CSRF: %d", c.restartCalls)
	}
}
