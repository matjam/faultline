package adminhttp

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
)

// fakeSkillsAdmin lets the fragment + toggle handlers be exercised
// without the real filesystem-backed store.
type fakeSkillsAdmin struct {
	mu     sync.Mutex
	all    []skillsfs.AllSkill
	failOn map[string]error
}

func (f *fakeSkillsAdmin) ListAll() []skillsfs.AllSkill {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]skillsfs.AllSkill, len(f.all))
	copy(out, f.all)
	return out
}

func (f *fakeSkillsAdmin) SetEnabled(name string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[name]; ok {
		return err
	}
	for i := range f.all {
		if f.all[i].Name == name {
			f.all[i].Disabled = !enabled
			return nil
		}
	}
	return errors.New("not found")
}

func makeFakeSkill(name, desc string, disabled bool) skillsfs.AllSkill {
	var s skillsfs.AllSkill
	s.Name = name
	s.Description = desc
	s.Disabled = disabled
	return s
}

func TestFragSkills_RendersList(t *testing.T) {
	sk := &fakeSkillsAdmin{
		all: []skillsfs.AllSkill{
			makeFakeSkill("alpha", "first skill", false),
			makeFakeSkill("beta", "second skill", true),
		},
	}
	ts := newSkillsAdminWiredServer(t, sk)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/skills")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{"alpha", "first skill", "beta", "second skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("skills fragment missing %q\n%s", want, got)
		}
	}
	if !strings.Contains(got, `name="enabled"`) {
		t.Fatalf("missing toggle input:\n%s", got)
	}
}

func TestFragSkills_NoSkillsAdmin(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/skills")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Skills support is not enabled") {
		t.Fatalf("expected disabled placeholder:\n%s", body)
	}
}

func TestSkillsToggle_DisablesSkill(t *testing.T) {
	sk := &fakeSkillsAdmin{
		all: []skillsfs.AllSkill{
			makeFakeSkill("alpha", "first", false),
			makeFakeSkill("beta", "second", false),
		},
	}
	ts := newSkillsAdminWiredServer(t, sk)
	client := loggedInClient(t, ts)

	// Pull dashboard to grab the session CSRF token.
	resp, _ := client.Get(ts.srv.URL + "/admin")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))
	if csrf == "" {
		t.Fatalf("dashboard missing csrf:\n%s", body)
	}

	// POST toggle for alpha with the "enabled" field absent
	// (simulating the unchecked checkbox).
	resp, err := client.PostForm(ts.srv.URL+"/admin/skills/toggle", url.Values{
		"_csrf": {csrf},
		"name":  {"alpha"},
	})
	if err != nil {
		t.Fatalf("POST toggle: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Re-listing should report alpha disabled, beta still enabled.
	all := sk.ListAll()
	gotAlpha, gotBeta := false, false
	for _, s := range all {
		if s.Name == "alpha" && s.Disabled {
			gotAlpha = true
		}
		if s.Name == "beta" && !s.Disabled {
			gotBeta = true
		}
	}
	if !gotAlpha {
		t.Fatal("alpha was not disabled by the toggle POST")
	}
	if !gotBeta {
		t.Fatal("beta should not have been touched")
	}

	// Response body re-renders the fragment with a flash.
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "alpha is now disabled") {
		t.Errorf("expected flash message, got:\n%s", body)
	}
}

func TestSkillsToggle_RequiresCSRF(t *testing.T) {
	sk := &fakeSkillsAdmin{all: []skillsfs.AllSkill{makeFakeSkill("alpha", "x", false)}}
	ts := newSkillsAdminWiredServer(t, sk)
	client := loggedInClient(t, ts)

	resp, err := client.PostForm(ts.srv.URL+"/admin/skills/toggle", url.Values{
		"name":    {"alpha"},
		"enabled": {"on"},
		// no _csrf
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF rejected)", resp.StatusCode)
	}
}
