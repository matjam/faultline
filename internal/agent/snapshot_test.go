package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/config"
)

// newTestAgent builds an Agent shell wired only with what Snapshot
// needs: a config and the inspector. The full Run loop's
// dependencies (chat, tools, etc.) are not required because we never
// call Run here.
func newTestAgent() *Agent {
	cfg := &config.Config{}
	cfg.Agent.MaxTokens = 100000
	cfg.Agent.CompactionThreshold = 80000
	a := &Agent{
		cfg:       cfg,
		inspector: newInspectorState(time.Now()),
	}
	return a
}

func TestSnapshot_NilSafe(t *testing.T) {
	var a *Agent
	got := a.Snapshot()
	if got != (AgentSnapshot{}) {
		t.Fatalf("nil receiver should return zero snapshot, got %+v", got)
	}
}

func TestSnapshot_InitialPhaseIsInitializing(t *testing.T) {
	a := newTestAgent()
	got := a.Snapshot()
	if got.Phase != PhaseInitializing {
		t.Fatalf("initial phase = %q, want %q", got.Phase, PhaseInitializing)
	}
	if got.MaxTokens != 100000 {
		t.Fatalf("MaxTokens = %d, want 100000", got.MaxTokens)
	}
	if got.CompactionThreshold != 80000 {
		t.Fatalf("CompactionThreshold = %d, want 80000", got.CompactionThreshold)
	}
}

func TestSnapshot_PhaseTransition(t *testing.T) {
	a := newTestAgent()
	first := a.Snapshot().PhaseSince

	time.Sleep(2 * time.Millisecond)
	a.setPhase(PhaseGenerating)
	got := a.Snapshot()
	if got.Phase != PhaseGenerating {
		t.Fatalf("phase = %q, want generating", got.Phase)
	}
	if !got.PhaseSince.After(first) {
		t.Fatalf("PhaseSince did not advance: first=%v after=%v", first, got.PhaseSince)
	}
}

func TestSnapshot_RecordChatAccumulates(t *testing.T) {
	a := newTestAgent()

	a.recordChat(120*time.Millisecond, 1500, 200, "stop", nil)
	a.recordChat(80*time.Millisecond, 2200, 150, "tool_calls", nil)

	got := a.Snapshot()
	if got.TotalChats != 2 {
		t.Fatalf("TotalChats = %d, want 2", got.TotalChats)
	}
	if got.TotalPromptTokens != 3700 {
		t.Fatalf("TotalPromptTokens = %d, want 3700", got.TotalPromptTokens)
	}
	if got.TotalCompletionTokens != 350 {
		t.Fatalf("TotalCompletionTokens = %d, want 350", got.TotalCompletionTokens)
	}
	if got.LastChatPromptTokens != 2200 {
		t.Fatalf("LastChatPromptTokens = %d, want 2200", got.LastChatPromptTokens)
	}
	if got.LastFinishReason != "tool_calls" {
		t.Fatalf("LastFinishReason = %q", got.LastFinishReason)
	}
	if got.LastChatLatency != 80*time.Millisecond {
		t.Fatalf("LastChatLatency = %v, want 80ms", got.LastChatLatency)
	}
}

func TestSnapshot_RecordChatError(t *testing.T) {
	a := newTestAgent()

	err := errors.New("backend offline")
	a.recordChat(50*time.Millisecond, 0, 0, "", err)

	got := a.Snapshot()
	if got.LastError != "backend offline" {
		t.Fatalf("LastError = %q", got.LastError)
	}
	if got.LastErrorAt.IsZero() {
		t.Fatal("LastErrorAt should be non-zero")
	}
	if got.TotalChats != 1 {
		t.Fatalf("TotalChats = %d, want 1 (errors still count as a chat attempt)", got.TotalChats)
	}
}

func TestSnapshot_RecordIterationTop(t *testing.T) {
	a := newTestAgent()
	a.recordIterationTop(15, 12345, 3, 2, 1)

	got := a.Snapshot()
	if got.MessageCount != 15 {
		t.Fatalf("MessageCount = %d", got.MessageCount)
	}
	if got.TokenEstimate != 12345 {
		t.Fatalf("TokenEstimate = %d", got.TokenEstimate)
	}
	if got.IdleStreak != 3 {
		t.Fatalf("IdleStreak = %d", got.IdleStreak)
	}
	if got.ActiveSubagents != 1 {
		t.Fatalf("ActiveSubagents = %d", got.ActiveSubagents)
	}
}

func TestSnapshot_Concurrent(t *testing.T) {
	a := newTestAgent()

	const writers = 8
	const reads = 200

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				a.recordChat(time.Millisecond, 10, 5, "stop", nil)
				a.recordToolCall()
				a.recordIterationTop(j, j*100, 0, 0, 0)
				a.setPhase(PhaseExecutingTool)
			}
		}()
	}

	// Reader pounds Snapshot in parallel — the race detector
	// catches any unsynchronized field access.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = a.Snapshot()
			}
		}
	}()
	for i := 0; i < reads; i++ {
		_ = a.Snapshot()
	}

	wg.Wait()
	close(done)

	got := a.Snapshot()
	if got.TotalChats != int64(writers*100) {
		t.Fatalf("TotalChats = %d, want %d", got.TotalChats, writers*100)
	}
	if got.TotalToolCalls != int64(writers*100) {
		t.Fatalf("TotalToolCalls = %d, want %d", got.TotalToolCalls, writers*100)
	}
}
