package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/matjam/faultline/internal/search/vector"
)

// fakeEmbedder is a deterministic Embedder for tests. It records
// every batch it sees and can be configured to fail for batches
// larger than maxBatchSize, mimicking a server with a physical
// batch limit (e.g. llama.cpp's --batch-size).
type fakeEmbedder struct {
	mu         sync.Mutex
	calls      []int // batch sizes seen, in order
	maxBatch   int   // > 0: error when batch size exceeds this
	failSingle bool  // true: also fail batches of size 1 unconditionally
	failOnText string
	dim        int
	model      string
}

func (f *fakeEmbedder) Dim() int      { return f.dim }
func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls = append(f.calls, len(texts))
	f.mu.Unlock()

	if f.maxBatch > 0 && len(texts) > f.maxBatch {
		return nil, errors.New("batch too large")
	}
	if f.failSingle && len(texts) == 1 {
		return nil, errors.New("single failure")
	}
	if f.failOnText != "" {
		for _, t := range texts {
			if t == f.failOnText {
				return nil, errors.New("specific text failure")
			}
		}
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Deterministic non-zero vector: first dim slot is text length.
		v := make([]float32, f.dim)
		v[0] = float32(len(t)) + 1
		_ = i
		_ = t
		out[i] = v
	}
	return out, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEmbedAdaptive_SinglePassNoFailures(t *testing.T) {
	f := &fakeEmbedder{dim: 4, model: "m"}
	texts := []string{"a", "b", "c", "d"}

	vecs, skipped, err := embedWithAdaptiveBatching(context.Background(), f, texts, 100, discardLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped: want 0 got %d", skipped)
	}
	if len(vecs) != 4 {
		t.Fatalf("len: %d", len(vecs))
	}
	if len(f.calls) != 1 || f.calls[0] != 4 {
		t.Errorf("expected one call of 4, got %v", f.calls)
	}
}

func TestEmbedAdaptive_HalvesOnFailure(t *testing.T) {
	// Server accepts max 2 per batch. Configured at 8.
	f := &fakeEmbedder{dim: 4, model: "m", maxBatch: 2}
	texts := make([]string, 8)
	for i := range texts {
		texts[i] = "t"
	}

	vecs, skipped, err := embedWithAdaptiveBatching(context.Background(), f, texts, 8, discardLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if skipped != 0 {
		t.Errorf("nothing should be skipped; got %d", skipped)
	}
	for i, v := range vecs {
		if v == nil {
			t.Errorf("vec[%d] is nil", i)
		}
	}
	// First call: 8 (fails). Halve: 4 (fails). Halve: 2 (succeeds, stays).
	// Then 2,2,2 = 4 successes total. Wait — we have 8 inputs.
	// Sequence: 8 (fail), 4 (fail), 2 (succ, i=0..2), 2 (succ, i=2..4),
	//          2 (succ, i=4..6), 2 (succ, i=6..8). After 5 successes
	//          we'd try doubling but we already hit the end.
	// So calls = [8, 4, 2, 2, 2, 2].
	if len(f.calls) < 4 {
		t.Errorf("expected at least 4 calls (failures plus successes), got %v", f.calls)
	}
	if f.calls[0] != 8 || f.calls[1] != 4 {
		t.Errorf("first two calls should be original size then halved: %v", f.calls)
	}
}

func TestEmbedAdaptive_GrowsBackAfterSuccesses(t *testing.T) {
	// maxBatch=4, configured=8: first call fails (8), halves to 4 (success).
	// Five more 4-batches succeed -> grow back toward 8.
	f := &fakeEmbedder{dim: 4, model: "m", maxBatch: 4}
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = "t"
	}

	_, skipped, err := embedWithAdaptiveBatching(context.Background(), f, texts, 8, discardLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if skipped != 0 {
		t.Errorf("nothing should skip; got %d", skipped)
	}
	// Look for a call of 8 (or attempt thereof) somewhere after the
	// initial 8 -> 4 demotion. With maxBatch=4 every grow-back attempt
	// of 8 will also fail — verify we see *attempts* at growing.
	saw8After := 0
	for i := 1; i < len(f.calls); i++ {
		if f.calls[i] == 8 {
			saw8After++
		}
	}
	if saw8After == 0 {
		t.Errorf("expected at least one grow-back attempt at size 8; calls=%v", f.calls)
	}
}

func TestEmbedAdaptive_SkipsSingleInputFailure(t *testing.T) {
	// "bad" always fails; everything else succeeds.
	f := &fakeEmbedder{dim: 4, model: "m", failOnText: "bad", maxBatch: 0}
	// Configure maxBatch = 1 so the batcher reaches single-input mode.
	f.maxBatch = 1
	texts := []string{"good1", "bad", "good2"}

	vecs, skipped, err := embedWithAdaptiveBatching(context.Background(), f, texts, 4, discardLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skip, got %d", skipped)
	}
	if vecs[0] == nil || vecs[2] == nil {
		t.Errorf("good inputs should be embedded: vec[0]=%v vec[2]=%v", vecs[0], vecs[2])
	}
	if vecs[1] != nil {
		t.Errorf("bad input should be skipped (vec[1] nil), got %v", vecs[1])
	}
}

func TestEmbedAdaptive_RespectsContextCancellation(t *testing.T) {
	f := &fakeEmbedder{dim: 4, model: "m"}
	texts := make([]string, 10)
	for i := range texts {
		texts[i] = "t"
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	_, _, err := embedWithAdaptiveBatching(ctx, f, texts, 5, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestEmbedAdaptive_EmptyInput(t *testing.T) {
	f := &fakeEmbedder{dim: 4, model: "m"}
	vecs, skipped, err := embedWithAdaptiveBatching(context.Background(), f, nil, 100, discardLogger())
	if err != nil || skipped != 0 || vecs != nil {
		t.Errorf("empty input: vecs=%v skipped=%d err=%v", vecs, skipped, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("empty input should not call embedder; got %v", f.calls)
	}
}

func TestFileLevelHits_DedupesByPath(t *testing.T) {
	raw := []vector.Result{
		{Path: "a.md#0", Score: 0.9},
		{Path: "a.md#3", Score: 0.85}, // same file, lower score — drop
		{Path: "b.md", Score: 0.8},    // different file
		{Path: "c.md#1", Score: 0.7},
		{Path: "b.md#5", Score: 0.6}, // duplicate of b.md
	}
	got := fileLevelHits(raw, 5)
	if len(got) != 3 {
		t.Fatalf("want 3 distinct files, got %d: %+v", len(got), got)
	}
	// Order should be by best score: a.md (0.9), b.md (0.8), c.md (0.7)
	want := []string{"a.md#0", "b.md", "c.md#1"}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("got[%d].Path = %q, want %q", i, got[i].Path, w)
		}
	}
}

func TestFileLevelHits_RespectsLimit(t *testing.T) {
	raw := []vector.Result{
		{Path: "a", Score: 0.9},
		{Path: "b", Score: 0.8},
		{Path: "c", Score: 0.7},
		{Path: "d", Score: 0.6},
	}
	got := fileLevelHits(raw, 2)
	if len(got) != 2 {
		t.Errorf("want 2 (limit), got %d", len(got))
	}
}

func TestFileLevelHits_Empty(t *testing.T) {
	if got := fileLevelHits(nil, 5); got != nil {
		t.Errorf("nil input: got %v", got)
	}
	if got := fileLevelHits([]vector.Result{{Path: "x"}}, 0); got != nil {
		t.Errorf("limit 0: got %v", got)
	}
}

func TestChunkIdxFromUnitKey(t *testing.T) {
	cases := []struct {
		key  string
		want int
	}{
		{"foo.md", -1},
		{"foo.md#0", 0},
		{"foo.md#42", 42},
		{"a/b/c.md#7", 7},
		{"foo.md#abc", -1},
		{"foo.md#", -1},
	}
	for _, c := range cases {
		if got := chunkIdxFromUnitKey(c.key); got != c.want {
			t.Errorf("chunkIdxFromUnitKey(%q) = %d, want %d", c.key, got, c.want)
		}
	}
}
