package adminhttp

import (
	"sync"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/tools"
)

func TestToolBuffer_FillsAndPreservesOrder(t *testing.T) {
	b := NewToolBuffer(4)

	for i := 0; i < 3; i++ {
		b.OnToolCall(tools.ToolCallEvent{
			Name:      "memory_write",
			StartedAt: time.Now(),
			ArgsBytes: i,
		})
	}

	got := b.Snapshot()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, ev := range got {
		if ev.ArgsBytes != i {
			t.Fatalf("[%d] ArgsBytes = %d, want %d", i, ev.ArgsBytes, i)
		}
	}
}

func TestToolBuffer_WrapsWhenFull(t *testing.T) {
	b := NewToolBuffer(4)

	// Fill past capacity. The buffer must drop the oldest events
	// in favor of the newest.
	for i := 0; i < 7; i++ {
		b.OnToolCall(tools.ToolCallEvent{
			Name:      "memory_write",
			ArgsBytes: i,
		})
	}

	if b.Len() != 4 {
		t.Fatalf("Len = %d, want 4 (capacity)", b.Len())
	}

	got := b.Snapshot()
	if len(got) != 4 {
		t.Fatalf("Snapshot len = %d, want 4", len(got))
	}
	// Should now hold events 3, 4, 5, 6 in chronological order.
	want := []int{3, 4, 5, 6}
	for i, ev := range got {
		if ev.ArgsBytes != want[i] {
			t.Fatalf("[%d] ArgsBytes = %d, want %d (full snapshot: %+v)",
				i, ev.ArgsBytes, want[i], got)
		}
	}
}

func TestToolBuffer_SnapshotRecent(t *testing.T) {
	b := NewToolBuffer(8)
	for i := 0; i < 5; i++ {
		b.OnToolCall(tools.ToolCallEvent{ArgsBytes: i})
	}

	got := b.SnapshotRecent(3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, ev := range got {
		want := i + 2 // events 2, 3, 4 are the most recent three
		if ev.ArgsBytes != want {
			t.Fatalf("[%d] ArgsBytes = %d, want %d", i, ev.ArgsBytes, want)
		}
	}

	// SnapshotRecent with n >= total returns the full snapshot.
	if got := b.SnapshotRecent(99); len(got) != 5 {
		t.Fatalf("SnapshotRecent(99) len = %d, want 5", len(got))
	}
	if got := b.SnapshotRecent(0); len(got) != 5 {
		t.Fatalf("SnapshotRecent(0) len = %d, want 5", len(got))
	}
}

func TestToolBuffer_DefaultCapacity(t *testing.T) {
	if got := NewToolBuffer(0).Cap(); got != 500 {
		t.Fatalf("default cap = %d, want 500", got)
	}
	if got := NewToolBuffer(-7).Cap(); got != 500 {
		t.Fatalf("negative cap = %d, want fallback to 500", got)
	}
}

func TestToolBuffer_Concurrent(t *testing.T) {
	b := NewToolBuffer(64)
	const writers = 8
	const perWriter = 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				b.OnToolCall(tools.ToolCallEvent{
					Name:      "memory_write",
					ArgsBytes: wid*perWriter + i,
				})
			}
		}(w)
	}

	// A reader pounding Snapshot in parallel must not race.
	reader := make(chan struct{})
	go func() {
		for {
			select {
			case <-reader:
				return
			default:
				_ = b.Snapshot()
			}
		}
	}()

	wg.Wait()
	close(reader)

	if got := b.Len(); got != b.Cap() {
		t.Fatalf("Len = %d, want %d (capacity)", got, b.Cap())
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{-1, "—"},
		{500 * time.Millisecond, "500ms"},
		{1500 * time.Millisecond, "2s"},
		{61 * time.Second, "1m1s"},
		{3*time.Hour + 5*time.Minute + 7*time.Second, "3h5m7s"},
	}
	for _, tc := range cases {
		if got := FormatDuration(tc.in); got != tc.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatRelative_Zero(t *testing.T) {
	if got := FormatRelative(time.Time{}); got != "—" {
		t.Fatalf("FormatRelative(zero) = %q, want —", got)
	}
}
