package adminhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/update"
)

// fakeUpdater is a deterministic stand-in for *update.Updater that
// satisfies the UpdateInspector port.
type fakeUpdater struct {
	enabled    bool
	current    string
	state      update.State
	applyErr   error
	applyRes   *update.Result
	appliedTo  string
	applyCalls int
}

func (f *fakeUpdater) Enabled() bool          { return f.enabled }
func (f *fakeUpdater) CurrentVersion() string { return f.current }
func (f *fakeUpdater) State() update.State    { return f.state }
func (f *fakeUpdater) Apply(ctx context.Context) (*update.Result, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	f.appliedTo = f.applyRes.ToVersion
	return f.applyRes, nil
}

func newUpdateAdminServer(t *testing.T, u UpdateInspector) *testServer {
	return newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, u)
}

// newAdminTestServer-with-update needs a version that takes the
// UpdateInspector. Add it as an overload of the existing helper.
func init() {
	// Plug into the existing helper at compile time via a small
	// indirection isn't possible because the helper is a package-
	// scoped function with a fixed signature. Tests below use a
	// dedicated builder defined in this file.
	_ = newAdminTestServer
}

// newAdminTestServerWithUpdate is the same as newAdminTestServer in
// fragments_test.go but extended with an UpdateInspector. Defined
// here rather than in the shared test helper to avoid editing every
// call site.
func newAdminTestServerWithUpdate(t *testing.T, u UpdateInspector) *testServer {
	return newAdminTestServer(t, nil, nil, NewToolBuffer(8), nil, u)
}

func TestFragUpdate_NoUpdater(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/update")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "updater is not wired") {
		t.Fatalf("expected not-wired placeholder:\n%s", body)
	}
}

func TestFragUpdate_DisabledRendersNotice(t *testing.T) {
	u := &fakeUpdater{
		enabled: false,
		current: "1.5.0",
		state:   update.State{CurrentVersion: "1.5.0"},
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/update")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "auto-update off") {
		t.Errorf("expected auto-update-off badge, got:\n%s", got)
	}
	if !strings.Contains(got, "1.5.0") {
		t.Errorf("expected current version 1.5.0, got:\n%s", got)
	}
	if strings.Contains(got, "Apply update") {
		t.Errorf("apply button should not render when disabled:\n%s", got)
	}
}

func TestFragUpdate_AvailableShowsApplyButton(t *testing.T) {
	u := &fakeUpdater{
		enabled: true,
		current: "1.5.0",
		state: update.State{
			LastChecked:     time.Now().Add(-2 * time.Minute),
			CurrentVersion:  "1.5.0",
			LatestVersion:   "1.6.0",
			UpdateAvailable: true,
		},
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/update")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"auto-update on",
		"1.5.0",
		"1.6.0",
		"new!",
		"Apply update 1.5.0 → 1.6.0",
		`hx-post="/admin/update/apply"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
}

func TestFragUpdate_NoUpdateAvailable(t *testing.T) {
	u := &fakeUpdater{
		enabled: true,
		current: "1.5.0",
		state: update.State{
			LastChecked:     time.Now().Add(-30 * time.Second),
			CurrentVersion:  "1.5.0",
			LatestVersion:   "1.5.0",
			UpdateAvailable: false,
		},
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/update")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "current") {
		t.Errorf("expected 'current' badge:\n%s", got)
	}
	if strings.Contains(got, "Apply update") {
		t.Errorf("apply button should not render when up to date:\n%s", got)
	}
}

func TestFragUpdate_LastErrorRenders(t *testing.T) {
	u := &fakeUpdater{
		enabled: true,
		current: "1.5.0",
		state: update.State{
			LastChecked: time.Now().Add(-10 * time.Second),
			Err:         errors.New("github says rate limited"),
		},
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/update")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "github says rate limited") {
		t.Fatalf("error text not surfaced:\n%s", body)
	}
}

func TestUpdateApply_DisabledIsRefused(t *testing.T) {
	u := &fakeUpdater{enabled: false, current: "1.5.0"}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/update/apply", url.Values{
		"_csrf": {csrf},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Self-update is disabled") {
		t.Fatalf("expected disabled flash:\n%s", body)
	}
	if u.applyCalls != 0 {
		t.Fatalf("Apply was called %d times despite being disabled", u.applyCalls)
	}
}

func TestUpdateApply_Success(t *testing.T) {
	u := &fakeUpdater{
		enabled: true,
		current: "1.5.0",
		state: update.State{
			LatestVersion:   "1.6.0",
			UpdateAvailable: true,
		},
		applyRes: &update.Result{
			FromVersion: "1.5.0",
			ToVersion:   "1.6.0",
		},
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/update/apply", url.Values{
		"_csrf": {csrf},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if u.applyCalls != 1 {
		t.Fatalf("Apply calls = %d, want 1", u.applyCalls)
	}
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Updated 1.5.0 → 1.6.0") {
		t.Errorf("expected success flash:\n%s", body)
	}
}

func TestUpdateApply_Failure(t *testing.T) {
	u := &fakeUpdater{
		enabled:  true,
		current:  "1.5.0",
		applyErr: errors.New("sha256 mismatch"),
	}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))

	resp, err := client.PostForm(ts.srv.URL+"/admin/update/apply", url.Values{
		"_csrf": {csrf},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Update failed: sha256 mismatch") {
		t.Errorf("expected failure flash:\n%s", body)
	}
}

func TestUpdateApply_RequiresCSRF(t *testing.T) {
	u := &fakeUpdater{enabled: true, current: "1.5.0"}
	ts := newAdminTestServerWithUpdate(t, u)
	client := loggedInClient(t, ts)

	resp, err := client.PostForm(ts.srv.URL+"/admin/update/apply", url.Values{
		// no _csrf
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if u.applyCalls != 0 {
		t.Fatalf("Apply was called %d times despite missing CSRF", u.applyCalls)
	}
}

// silence unused warning on newUpdateAdminServer when tests use the
// alias instead.
var _ = newUpdateAdminServer
