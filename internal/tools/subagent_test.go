package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/subagent"
)

func newPrimaryExecutorWithMgr(t *testing.T, mgr *subagent.Manager) *Executor {
	t.Helper()
	return New(Deps{
		Mode:            ModePrimary,
		Logger:          silentTestLogger(),
		SubagentManager: mgr,
	})
}

func newSubagentExecutor(t *testing.T, sink func(string)) *Executor {
	t.Helper()
	return New(Deps{
		Mode:             ModeSubagent,
		Logger:           silentTestLogger(),
		SubagentReportFn: sink,
	})
}

func newTestManager(t *testing.T, spawn subagent.SpawnFunc) *subagent.Manager {
	t.Helper()
	profiles := []subagent.Profile{
		{Name: "default", APIURL: "x", Model: "m", Purpose: "fallback"},
		{Name: "fast", APIURL: "x", Model: "m", Purpose: "quick lookups"},
	}
	if spawn == nil {
		spawn = func(context.Context, string, subagent.Profile, string, int) subagent.Report { return subagent.Report{} }
	}
	return subagent.New(subagent.Config{}, profiles, spawn, silentTestLogger())
}

func TestSubagentToolDefsPrimary(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	defs := te.subagentToolDefs()
	wantNames := map[string]bool{
		"subagent_run":    false,
		"subagent_spawn":  false,
		"subagent_wait":   false,
		"subagent_status": false,
		"subagent_cancel": false,
	}
	for _, d := range defs {
		if d.Function == nil {
			continue
		}
		if _, ok := wantNames[d.Function.Name]; ok {
			wantNames[d.Function.Name] = true
		} else {
			t.Errorf("unexpected tool advertised: %s", d.Function.Name)
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("primary missing tool def: %s", name)
		}
	}
}

func TestSubagentToolDefsPrimaryEmptyWhenNoManager(t *testing.T) {
	te := New(Deps{Mode: ModePrimary, Logger: silentTestLogger()})
	if defs := te.subagentToolDefs(); len(defs) != 0 {
		t.Errorf("expected no subagent tool defs without manager, got %d", len(defs))
	}
}

func TestSubagentToolDefsChild(t *testing.T) {
	te := newSubagentExecutor(t, func(string) {})
	defs := te.subagentToolDefs()
	if len(defs) != 1 {
		t.Fatalf("subagent should advertise 1 tool, got %d", len(defs))
	}
	if defs[0].Function == nil || defs[0].Function.Name != "subagent_report" {
		t.Errorf("subagent should advertise subagent_report, got %+v", defs[0])
	}
}

func TestSubagentToolDefsChildEmptyWhenNoSink(t *testing.T) {
	te := New(Deps{Mode: ModeSubagent, Logger: silentTestLogger()})
	if defs := te.subagentToolDefs(); len(defs) != 0 {
		t.Errorf("subagent without sink should advertise nothing; got %d defs", len(defs))
	}
}

func TestSubagentRunDispatch(t *testing.T) {
	wantText := "the answer"
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		return subagent.Report{Text: wantText}
	}
	mgr := newTestManager(t, spawn)
	te := newPrimaryExecutorWithMgr(t, mgr)

	got := te.subagentRun(context.Background(), `{"profile":"fast","prompt":"do it"}`)
	if !strings.Contains(got, wantText) {
		t.Errorf("subagentRun output missing text: got %q", got)
	}
}

func TestSubagentRunRejectsEmptyArgs(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	if got := te.subagentRun(context.Background(), `{"prompt":"x"}`); !strings.Contains(got, "profile") {
		t.Errorf("missing profile not rejected: %s", got)
	}
	if got := te.subagentRun(context.Background(), `{"profile":"fast"}`); !strings.Contains(got, "prompt") {
		t.Errorf("missing prompt not rejected: %s", got)
	}
}

func TestSubagentRunUnknownProfile(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	got := te.subagentRun(context.Background(), `{"profile":"no-such","prompt":"x"}`)
	if !strings.Contains(strings.ToLower(got), "unknown") {
		t.Errorf("expected unknown-profile error, got: %s", got)
	}
}

func TestSubagentSpawnReturnsWorkID(t *testing.T) {
	hold := make(chan struct{})
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		<-hold
		return subagent.Report{Text: "ok"}
	}
	mgr := newTestManager(t, spawn)
	te := newPrimaryExecutorWithMgr(t, mgr)

	got := te.subagentSpawn(context.Background(), `{"profile":"fast","prompt":"go"}`)
	if !strings.Contains(got, "sub-") {
		t.Errorf("spawn output missing work_id: %s", got)
	}
	close(hold)
	// Drain to keep tests clean.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.HasPending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = mgr.Pending()
}

func TestSubagentWaitReturnsReport(t *testing.T) {
	release := make(chan struct{})
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		<-release
		return subagent.Report{Text: "wait-result"}
	}
	mgr := newTestManager(t, spawn)
	te := newPrimaryExecutorWithMgr(t, mgr)

	workID, err := mgr.Spawn(context.Background(), "fast", "task")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Kick off the wait in a goroutine; release the spawn shortly after.
	resultCh := make(chan string, 1)
	go func() {
		resultCh <- te.subagentWait(context.Background(),
			`{"work_id":"`+workID+`"}`)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case got := <-resultCh:
		if !strings.Contains(got, "wait-result") {
			t.Errorf("subagent_wait output missing report text: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subagent_wait did not return")
	}

	// Inbox must be empty: the wait drained the report.
	if mgr.HasPending() {
		t.Error("expected inbox to be empty after subagent_wait drained the report")
	}
}

func TestSubagentWaitUnknownWorkID(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	got := te.subagentWait(context.Background(), `{"work_id":"sub-deadbeef"}`)
	if !strings.Contains(strings.ToLower(got), "no subagent") &&
		!strings.Contains(strings.ToLower(got), "no report") {
		t.Errorf("expected unknown-work_id error, got: %s", got)
	}
}

func TestSubagentWaitCancelOnCtx(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return subagent.Report{}
	}
	mgr := newTestManager(t, spawn)
	te := newPrimaryExecutorWithMgr(t, mgr)

	workID, _ := mgr.Spawn(context.Background(), "fast", "task")

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan string, 1)
	go func() {
		resultCh <- te.subagentWait(ctx, `{"work_id":"`+workID+`"}`)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case got := <-resultCh:
		if !strings.Contains(strings.ToLower(got), "canceled") {
			t.Errorf("expected cancellation message, got: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subagent_wait did not return after ctx cancel")
	}
}

func TestSubagentStatusEmpty(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	got := te.subagentStatus()
	if !strings.Contains(strings.ToLower(got), "no active") {
		t.Errorf("expected 'no active' message, got: %s", got)
	}
}

func TestSubagentStatusListsActive(t *testing.T) {
	hold := make(chan struct{})
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		<-hold
		return subagent.Report{}
	}
	mgr := newTestManager(t, spawn)
	te := newPrimaryExecutorWithMgr(t, mgr)

	if _, err := mgr.Spawn(context.Background(), "fast", "first task"); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	got := te.subagentStatus()
	if !strings.Contains(got, "first task") {
		t.Errorf("status missing prompt preview: %s", got)
	}
	if !strings.Contains(got, "fast") {
		t.Errorf("status missing profile name: %s", got)
	}
	close(hold)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.ActiveCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = mgr.Pending()
}

func TestSubagentCancelUnknownWorkID(t *testing.T) {
	mgr := newTestManager(t, nil)
	te := newPrimaryExecutorWithMgr(t, mgr)
	got := te.subagentCancel(`{"work_id":"sub-deadbeef"}`)
	if !strings.Contains(strings.ToLower(got), "no active") &&
		!strings.Contains(strings.ToLower(got), "fail") {
		t.Errorf("expected cancel failure, got: %s", got)
	}
}

func TestSubagentReportInvokesSink(t *testing.T) {
	var got string
	te := newSubagentExecutor(t, func(text string) { got = text })
	out := te.subagentReport(`{"summary":"the result"}`)
	if got != "the result" {
		t.Errorf("sink got %q, want 'the result'", got)
	}
	if !strings.Contains(strings.ToLower(out), "delivered") {
		t.Errorf("report tool output missing confirmation: %s", out)
	}
}

func TestSubagentReportNotAvailableWithoutSink(t *testing.T) {
	te := New(Deps{Mode: ModeSubagent, Logger: silentTestLogger()})
	got := te.subagentReport(`{"summary":"x"}`)
	if !strings.Contains(strings.ToLower(got), "only available") {
		t.Errorf("expected 'only available' error, got: %s", got)
	}
}

func TestSubagentForbiddenStrippedFromSubagentTools(t *testing.T) {
	te := New(Deps{
		Mode:             ModeSubagent,
		Logger:           silentTestLogger(),
		SubagentReportFn: func(string) {},
	})
	defs := te.ToolDefs()
	for _, d := range defs {
		if d.Function == nil {
			continue
		}
		if _, banned := subagentForbidden[d.Function.Name]; banned {
			t.Errorf("subagent advertised forbidden tool %q", d.Function.Name)
		}
	}
}

func TestSubagentExecuteRejectsForbiddenDefensively(t *testing.T) {
	te := New(Deps{Mode: ModeSubagent, Logger: silentTestLogger()})
	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "sleep", Arguments: `{"seconds":1}`},
	})
	if !strings.Contains(strings.ToLower(got), "not available") {
		t.Errorf("expected 'not available', got: %s", got)
	}
}

func TestFormatReportFlags(t *testing.T) {
	r := subagent.Report{Truncated: true, Text: "partial"}
	got := formatReport(r)
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncated flag, got: %s", got)
	}
	if !strings.Contains(got, "partial") {
		t.Errorf("expected text body, got: %s", got)
	}

	r = subagent.Report{Canceled: true}
	if !strings.Contains(formatReport(r), "canceled") {
		t.Error("expected canceled flag")
	}

	r = subagent.Report{Err: errors.New("boom"), Text: "x"}
	if !strings.Contains(formatReport(r), "boom") {
		t.Error("expected error message")
	}
}
