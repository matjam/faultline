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
	"github.com/matjam/faultline/internal/agent"
	"github.com/matjam/faultline/internal/subagent"
	"github.com/matjam/faultline/internal/tools"
)

// fakeAgentInspector is a deterministic inspector for fragment tests.
type fakeAgentInspector struct{ snap agent.AgentSnapshot }

func (f *fakeAgentInspector) Snapshot() agent.AgentSnapshot { return f.snap }

// fakeSubagentInspector is a deterministic inspector for fragment tests.
type fakeSubagentInspector struct {
	active   []subagent.ActiveStatus
	profiles []subagent.Profile
}

func (f *fakeSubagentInspector) Status() []subagent.ActiveStatus { return f.active }
func (f *fakeSubagentInspector) Profiles() []subagent.Profile    { return f.profiles }

// newFragmentsTestServer mirrors newTestServer in server_test.go but
// also wires inspectors and a tool buffer.
func newFragmentsTestServer(t *testing.T,
	ag AgentInspector,
	sub SubagentInspector,
	buf *ToolBuffer,
) *testServer {
	return newAdminTestServer(t, ag, sub, buf, nil, nil)
}

// newSkillsAdminWiredServer is the skills-test variant: nil agent &
// subagent inspectors, real tool buffer, real SkillsAdmin.
func newSkillsAdminWiredServer(t *testing.T, sk SkillsAdmin) *testServer {
	return newAdminTestServer(t, nil, nil, NewToolBuffer(8), sk, nil)
}

// newAdminTestServer is the most general builder; the helpers above
// fill in nils for the subset they don't need.
func newAdminTestServer(t *testing.T,
	ag AgentInspector,
	sub SubagentInspector,
	buf *ToolBuffer,
	sk SkillsAdmin,
	upd UpdateInspector,
) *testServer {
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
		Bind:      "127.0.0.1:0",
		Users:     store,
		Sessions:  sessions,
		StartedAt: time.Now(),
		Logger:    logger,
		Agent:     ag,
		Subagents: sub,
		Tools:     buf,
		Skills:    sk,
		Update:    upd,
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

// loggedInClient returns a cookie-jar client that has already
// completed a login round-trip against ts.
func loggedInClient(t *testing.T, ts *testServer) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.srv.URL + "/admin/login")
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf := extractFormCSRF(string(body))
	if csrf == "" {
		t.Fatalf("no CSRF in login form")
	}
	resp, err = client.PostForm(ts.srv.URL+"/admin/login", url.Values{
		"username": {"admin"},
		"password": {ts.password},
		"_csrf":    {csrf},
	})
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	return client
}

func TestFragStatus_RendersAgentData(t *testing.T) {
	now := time.Now()
	ai := &fakeAgentInspector{snap: agent.AgentSnapshot{
		StartedAt:                now.Add(-30 * time.Minute),
		Phase:                    agent.PhaseGenerating,
		PhaseSince:               now.Add(-12 * time.Second),
		MessageCount:             47,
		TokenEstimate:            12345,
		MaxTokens:                100000,
		CompactionThreshold:      80000,
		IdleStreak:               2,
		TotalChats:               19,
		TotalPromptTokens:        300000,
		TotalCompletionTokens:    7500,
		TotalToolCalls:           33,
		LastChatAt:               now.Add(-3 * time.Second),
		LastChatLatency:          850 * time.Millisecond,
		LastChatPromptTokens:     5400,
		LastChatCompletionTokens: 220,
		LastFinishReason:         "tool_calls",
	}}
	ts := newFragmentsTestServer(t, ai, nil, NewToolBuffer(64))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"Primary agent",
		"generating", // phase string
		"47",         // message count
		"tool_calls", // finish reason
		"12345",      // token estimate
		"100000",     // max tokens
		"80000",      // compaction threshold
		"19",         // total chats
		"300000",     // total prompt tokens
		"850ms",      // last chat latency formatted
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status fragment missing %q\n--- body ---\n%s", want, got)
		}
	}
}

func TestFragStatus_NoAgentRendersPlaceholder(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(64))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no agent inspector wired") {
		t.Fatalf("expected placeholder, got: %s", body)
	}
}

func TestFragTools_RendersBufferedEvents(t *testing.T) {
	buf := NewToolBuffer(16)
	for _, name := range []string{"memory_read", "memory_write", "sandbox_execute"} {
		buf.OnToolCall(tools.ToolCallEvent{
			Mode:          "primary",
			Name:          name,
			StartedAt:     time.Now().Add(-2 * time.Second),
			Duration:      40 * time.Millisecond,
			ArgsSummary:   "{path: " + name + ".md}",
			ResultSummary: "ok",
		})
	}
	ts := newFragmentsTestServer(t, nil, nil, buf)
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/tools")
	if err != nil {
		t.Fatalf("GET tools: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, name := range []string{"memory_read", "memory_write", "sandbox_execute"} {
		if !strings.Contains(got, name) {
			t.Errorf("tools fragment missing %q\n%s", name, got)
		}
	}
	if !strings.Contains(got, "3 / 16 buffered") {
		t.Errorf("expected '3 / 16 buffered' counter, got: %s", got)
	}
}

func TestFragTools_EmptyBufferShowsHint(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(16))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/tools")
	if err != nil {
		t.Fatalf("GET tools: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "No tool calls observed yet") {
		t.Fatalf("empty-buffer hint missing: %s", body)
	}
}

func TestFragSubagents_RendersActive(t *testing.T) {
	si := &fakeSubagentInspector{
		active: []subagent.ActiveStatus{
			{
				WorkID:  "wk_001",
				Profile: "default",
				Prompt:  "summarize the latest sessions",
				Started: time.Now().Add(-5 * time.Second),
				Async:   true,
			},
		},
		profiles: []subagent.Profile{
			{Name: "fast", Model: "qwen-7b", Purpose: "quick lookups"},
			{Name: "deep", Model: "gpt-4o", Purpose: "deep research"},
		},
	}
	ts := newFragmentsTestServer(t, nil, si, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/subagents")
	if err != nil {
		t.Fatalf("GET subagents: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"Subagents",
		"wk_001",
		"summarize the latest sessions",
		"fast", "deep",
		"qwen-7b", "gpt-4o",
		"async",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("subagents fragment missing %q\n%s", want, got)
		}
	}
}

func TestFragSubagents_DisabledRendersPlaceholder(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	client := loggedInClient(t, ts)

	resp, err := client.Get(ts.srv.URL + "/admin/fragments/subagents")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Subagent delegation is not enabled") {
		t.Fatalf("expected disabled placeholder, got: %s", body)
	}
}

func TestFragments_RequireAuth(t *testing.T) {
	ts := newFragmentsTestServer(t, nil, nil, NewToolBuffer(8))
	for _, path := range []string{
		"/admin/fragments/status",
		"/admin/fragments/tools",
		"/admin/fragments/subagents",
	} {
		t.Run(path, func(t *testing.T) {
			client := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			resp, err := client.Get(ts.srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303 (redirect to login)", resp.StatusCode)
			}
		})
	}
}
