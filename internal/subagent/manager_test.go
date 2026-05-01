package subagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestProfiles() []Profile {
	return []Profile{
		{
			Name:    DefaultProfileName,
			APIURL:  "http://localhost:5001/v1",
			Model:   "qwen",
			Purpose: "default backend",
		},
		{
			Name:    "fast",
			APIURL:  "http://localhost:5001/v1",
			Model:   "qwen-7b",
			Purpose: "quick lookups",
		},
	}
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"fast", true},
		{"deep-research", true},
		{"a", true},
		{"a-1", true},

		{"", false},
		{"default", false}, // reserved
		{"FAST", false},    // uppercase
		{"-fast", false},
		{"fast-", false},
		{"fa--st", false},
		{"with space", false},
		{"under_score", false},
		// 33 chars
		{"abcdefghij-abcdefghij-abcdefghij-x", false},
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		if c.ok && err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", c.name)
		}
	}
}

func TestValidateProfile(t *testing.T) {
	good := Profile{Name: "fast", APIURL: "http://x/v1", Model: "m"}
	if err := ValidateProfile(good); err != nil {
		t.Errorf("ValidateProfile(good) = %v, want nil", err)
	}

	if err := ValidateProfile(Profile{Name: "fast"}); err == nil {
		t.Error("missing api_url: want error")
	}
	if err := ValidateProfile(Profile{Name: "fast", APIURL: "x"}); err == nil {
		t.Error("missing model: want error")
	}

	long := make([]byte, MaxPurposeLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateProfile(Profile{Name: "fast", APIURL: "x", Model: "m", Purpose: string(long)}); err == nil {
		t.Error("over-long purpose: want error")
	}
}

func TestRunSyncReturnsReport(t *testing.T) {
	want := "all done"
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		if profile.Name != "fast" {
			t.Errorf("got profile %q, want fast", profile.Name)
		}
		if prompt != "do the thing" {
			t.Errorf("got prompt %q, want 'do the thing'", prompt)
		}
		if maxTurns <= 0 {
			t.Errorf("maxTurns = %d, want positive", maxTurns)
		}
		return Report{Text: want}
	}

	m := New(Config{}, newTestProfiles(), spawn, newTestLogger())
	rep, err := m.Run(context.Background(), "fast", "do the thing")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rep.Text != want {
		t.Errorf("rep.Text = %q, want %q", rep.Text, want)
	}
	if rep.WorkID == "" {
		t.Error("WorkID empty")
	}
	if rep.Profile != "fast" {
		t.Errorf("rep.Profile = %q, want fast", rep.Profile)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after Run, want 0", m.ActiveCount())
	}
}

func TestSpawnAsyncReportLandsInInbox(t *testing.T) {
	released := make(chan struct{})
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		<-released
		return Report{Text: "async-done"}
	}

	m := New(Config{}, newTestProfiles(), spawn, newTestLogger())
	workID, err := m.Spawn(context.Background(), "fast", "off you go")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if workID == "" {
		t.Fatal("empty workID")
	}
	if !m.HasPending() && m.ActiveCount() != 1 {
		t.Error("expected one active subagent and no pending yet")
	}

	close(released)

	// Wait for goroutine to deliver.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.HasPending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	pending := m.Pending()
	if len(pending) != 1 {
		t.Fatalf("Pending() returned %d reports, want 1", len(pending))
	}
	if pending[0].Text != "async-done" {
		t.Errorf("text = %q, want async-done", pending[0].Text)
	}
	if pending[0].WorkID != workID {
		t.Errorf("workID mismatch: got %q want %q", pending[0].WorkID, workID)
	}
	if m.HasPending() {
		t.Error("Pending() did not drain")
	}
}

func TestSpawnConcurrentCap(t *testing.T) {
	hold := make(chan struct{})
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		<-hold
		return Report{Text: "ok"}
	}

	m := New(Config{MaxConcurrent: 2}, newTestProfiles(), spawn, newTestLogger())

	if _, err := m.Spawn(context.Background(), "fast", "a"); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if _, err := m.Spawn(context.Background(), "fast", "b"); err != nil {
		t.Fatalf("second spawn: %v", err)
	}
	if _, err := m.Spawn(context.Background(), "fast", "c"); err == nil {
		t.Fatal("third spawn: expected cap error, got nil")
	}

	// Sync run should still be allowed even at the async cap, because
	// it doesn't count toward MaxConcurrent.
	syncDone := make(chan struct{})
	go func() {
		_, _ = m.Run(context.Background(), "fast", "sync-while-capped")
		close(syncDone)
	}()
	// Give it a moment to register.
	time.Sleep(20 * time.Millisecond)

	close(hold)
	<-syncDone
	// drain
	_ = m.Pending()
}

func TestCancelStopsSubagent(t *testing.T) {
	var observed atomic.Bool
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		<-ctx.Done()
		observed.Store(true)
		return Report{Canceled: true, Err: ctx.Err()}
	}

	m := New(Config{}, newTestProfiles(), spawn, newTestLogger())
	workID, err := m.Spawn(context.Background(), "fast", "long task")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Wait for active registration.
	for i := 0; i < 100; i++ {
		if m.ActiveCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := m.Cancel(workID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Wait for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.HasPending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pending := m.Pending()
	if len(pending) != 1 {
		t.Fatalf("len(pending) = %d, want 1", len(pending))
	}
	if !pending[0].Canceled {
		t.Error("Report.Canceled = false, want true")
	}
	if !observed.Load() {
		t.Error("spawnFn did not observe ctx.Done")
	}
	if !errors.Is(pending[0].Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", pending[0].Err)
	}
}

func TestCancelUnknownWorkID(t *testing.T) {
	m := New(Config{}, newTestProfiles(), func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	if err := m.Cancel("sub-deadbeef"); err == nil {
		t.Error("Cancel(unknown) returned nil; want error")
	}
}

func TestCancelAllStopsEveryone(t *testing.T) {
	var wg sync.WaitGroup
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		wg.Add(1)
		defer wg.Done()
		<-ctx.Done()
		return Report{Canceled: true}
	}

	m := New(Config{MaxConcurrent: 4}, newTestProfiles(), spawn, newTestLogger())
	for i := 0; i < 3; i++ {
		if _, err := m.Spawn(context.Background(), "fast", "x"); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}

	// Wait until all are registered.
	for i := 0; i < 100; i++ {
		if m.ActiveCount() == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	m.CancelAll()
	wg.Wait()
}

func TestUnknownProfileRejected(t *testing.T) {
	m := New(Config{}, newTestProfiles(), func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	if _, err := m.Run(context.Background(), "no-such", "x"); err == nil {
		t.Error("Run(unknown profile) returned nil error")
	}
	if _, err := m.Spawn(context.Background(), "no-such", "x"); err == nil {
		t.Error("Spawn(unknown profile) returned nil error")
	}
}

func TestEmptyPromptRejected(t *testing.T) {
	m := New(Config{}, newTestProfiles(), func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	if _, err := m.Run(context.Background(), "fast", ""); err == nil {
		t.Error("Run(empty prompt) returned nil error")
	}
	if _, err := m.Spawn(context.Background(), "fast", ""); err == nil {
		t.Error("Spawn(empty prompt) returned nil error")
	}
}

func TestInboxDropsOldestWhenFull(t *testing.T) {
	release := make(chan int, 10)
	spawn := func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report {
		<-release
		return Report{Text: prompt}
	}

	m := New(Config{MaxConcurrent: 10, MaxInbox: 2}, newTestProfiles(), spawn, newTestLogger())

	for i := 0; i < 3; i++ {
		if _, err := m.Spawn(context.Background(), "fast", string(rune('a'+i))); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}
	// Release them in order; final inbox should hold the last 2.
	for i := 0; i < 3; i++ {
		release <- i
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.ActiveCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	pending := m.Pending()
	if len(pending) != 2 {
		t.Fatalf("len(pending) = %d, want 2 (inbox cap)", len(pending))
	}
	// First report (text "a") should be the dropped one; "b" and "c" survive.
	for _, r := range pending {
		if r.Text == "a" {
			t.Errorf("oldest report 'a' was not dropped; got %+v", r)
		}
	}
}

func TestProfilesReturnsCopy(t *testing.T) {
	in := newTestProfiles()
	m := New(Config{}, in, func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	out := m.Profiles()
	if len(out) != len(in) {
		t.Fatalf("Profiles len = %d, want %d", len(out), len(in))
	}
	out[0].Name = "tampered"
	if m.profiles[0].Name == "tampered" {
		t.Error("Profiles() returned underlying slice; mutation leaked back")
	}
}

func TestFindProfile(t *testing.T) {
	m := New(Config{}, newTestProfiles(), func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	if _, ok := m.FindProfile("fast"); !ok {
		t.Error("FindProfile(fast) ok=false; want true")
	}
	if _, ok := m.FindProfile("no-such"); ok {
		t.Error("FindProfile(no-such) ok=true; want false")
	}
}

func TestMaxTurnsPerRunDefault(t *testing.T) {
	m := New(Config{}, newTestProfiles(), func(context.Context, string, Profile, string, int) Report { return Report{} }, newTestLogger())
	if m.MaxTurnsPerRun() != 50 {
		t.Errorf("default MaxTurnsPerRun = %d, want 50", m.MaxTurnsPerRun())
	}
}
