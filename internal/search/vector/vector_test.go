package vector

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// approxEqual reports whether a and b are within tol.
func approxEqual(a, b, tol float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func TestUpsertAndSearch(t *testing.T) {
	idx := New(3, "test-model")

	// Three orthogonal vectors. After normalisation they remain
	// orthogonal; cosine similarity is the dot product.
	if err := idx.Upsert("x", []float32{1, 0, 0}); err != nil {
		t.Fatalf("upsert x: %v", err)
	}
	if err := idx.Upsert("y", []float32{0, 1, 0}); err != nil {
		t.Fatalf("upsert y: %v", err)
	}
	if err := idx.Upsert("z", []float32{0, 0, 1}); err != nil {
		t.Fatalf("upsert z: %v", err)
	}

	results, err := idx.Search([]float32{1, 0, 0}, 3, 0, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	if results[0].Path != "x" {
		t.Errorf("top result should be x, got %q", results[0].Path)
	}
	if !approxEqual(results[0].Score, 1.0, 1e-6) {
		t.Errorf("top score should be ~1.0, got %f", results[0].Score)
	}
}

func TestSearchMinScore(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("near", []float32{1, 0.1})
	_ = idx.Upsert("far", []float32{0, 1})

	// Query along x-axis. "near" has high cosine; "far" has near-zero.
	results, err := idx.Search([]float32{1, 0}, 5, 0.5, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Path != "near" {
		t.Errorf("expected only 'near' above min_score 0.5, got %+v", results)
	}
}

func TestSearchFilter(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("a/foo", []float32{1, 0})
	_ = idx.Upsert("b/bar", []float32{1, 0})

	results, _ := idx.Search([]float32{1, 0}, 5, 0, func(p string) bool {
		return p == "a/foo"
	})
	if len(results) != 1 || results[0].Path != "a/foo" {
		t.Errorf("filter should restrict to a/foo, got %+v", results)
	}
}

func TestUpsertReplacesExisting(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("p", []float32{1, 0})
	_ = idx.Upsert("p", []float32{0, 1})

	if got := idx.Len(); got != 1 {
		t.Errorf("len after replace: want 1 got %d", got)
	}

	// Query along x-axis should now miss (we wrote a y-axis vector).
	results, _ := idx.Search([]float32{1, 0}, 5, 0.5, nil)
	if len(results) != 0 {
		t.Errorf("after replace, query along old direction should miss, got %+v", results)
	}
}

func TestUpsertDimMismatch(t *testing.T) {
	idx := New(3, "m")
	if err := idx.Upsert("x", []float32{1, 0}); err == nil {
		t.Errorf("expected dim mismatch error")
	}
}

func TestUpsertZeroVector(t *testing.T) {
	idx := New(3, "m")
	if err := idx.Upsert("x", []float32{0, 0, 0}); err == nil {
		t.Errorf("expected zero-vector error")
	}
}

func TestRemove(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("a", []float32{1, 0})
	if !idx.Remove("a") {
		t.Errorf("remove should return true for existing key")
	}
	if idx.Has("a") {
		t.Errorf("a should be gone after remove")
	}
	if idx.Remove("a") {
		t.Errorf("second remove should return false")
	}
}

func TestRemovePrefix(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("a/x", []float32{1, 0})
	_ = idx.Upsert("a/y", []float32{0, 1})
	_ = idx.Upsert("a", []float32{1, 1})  // exact-prefix match
	_ = idx.Upsert("ab", []float32{1, 1}) // sibling, must NOT be removed
	_ = idx.Upsert("b/x", []float32{1, 0})

	n := idx.RemovePrefix("a")
	if n != 3 {
		t.Errorf("RemovePrefix count: want 3 got %d", n)
	}
	if !idx.Has("ab") {
		t.Errorf("'ab' should not be removed by prefix 'a'")
	}
	if !idx.Has("b/x") {
		t.Errorf("'b/x' should not be removed by prefix 'a'")
	}
}

func TestRename(t *testing.T) {
	idx := New(2, "m")
	_ = idx.Upsert("old", []float32{1, 0})
	if !idx.Rename("old", "new") {
		t.Errorf("rename should succeed")
	}
	if idx.Has("old") {
		t.Errorf("old key should be gone")
	}
	if !idx.Has("new") {
		t.Errorf("new key should exist")
	}
	// Search should still find via the new key.
	results, _ := idx.Search([]float32{1, 0}, 1, 0.5, nil)
	if len(results) != 1 || results[0].Path != "new" {
		t.Errorf("search after rename: %+v", results)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")

	src := New(4, "test-model-v1")
	_ = src.Upsert("alpha.md", []float32{1, 0, 0, 0})
	_ = src.Upsert("beta/gamma.md", []float32{0, 1, 0, 0})
	_ = src.Upsert("d", []float32{0.5, 0.5, 0.5, 0.5})

	if err := src.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if src.Dirty() {
		t.Errorf("dirty flag should be cleared after save")
	}

	dst := New(4, "test-model-v1")
	if err := dst.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	if dst.Len() != 3 {
		t.Errorf("len after load: want 3 got %d", dst.Len())
	}

	srcPaths := src.Paths()
	dstPaths := dst.Paths()
	sort.Strings(srcPaths)
	sort.Strings(dstPaths)
	for i, p := range srcPaths {
		if dstPaths[i] != p {
			t.Errorf("paths[%d] differ: %q vs %q", i, p, dstPaths[i])
		}
	}

	// Spot-check one vector roundtripped exactly.
	results, _ := dst.Search([]float32{1, 0, 0, 0}, 1, 0.5, nil)
	if len(results) != 1 || results[0].Path != "alpha.md" {
		t.Errorf("search after load: %+v", results)
	}
	if !approxEqual(results[0].Score, 1.0, 1e-6) {
		t.Errorf("score after load: want ~1.0 got %f", results[0].Score)
	}
}

func TestLoadNonexistentIsNoop(t *testing.T) {
	dir := t.TempDir()
	idx := New(2, "m")
	_ = idx.Upsert("a", []float32{1, 0})

	err := idx.Load(filepath.Join(dir, "does-not-exist.bin"))
	if err != nil {
		t.Errorf("loading nonexistent file should be a no-op, got %v", err)
	}
	if idx.Len() != 1 {
		t.Errorf("index should be untouched, len=%d", idx.Len())
	}
}

func TestLoadModelMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")

	src := New(4, "model-A")
	_ = src.Upsert("x", []float32{1, 0, 0, 0})
	if err := src.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	dst := New(4, "model-B")
	err := dst.Load(path)
	if !errors.Is(err, ErrModelMismatch) {
		t.Errorf("expected ErrModelMismatch, got %v", err)
	}
}

func TestLoadDimMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")

	src := New(4, "m")
	_ = src.Upsert("x", []float32{1, 0, 0, 0})
	if err := src.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	dst := New(8, "m")
	err := dst.Load(path)
	if !errors.Is(err, ErrModelMismatch) {
		t.Errorf("expected ErrModelMismatch on dim change, got %v", err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")
	if err := os.WriteFile(path, []byte("not a real index"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	idx := New(4, "m")
	err := idx.Load(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt, got %v", err)
	}
}

func TestNormalizationIsApplied(t *testing.T) {
	idx := New(3, "m")
	// Length-2 vector along x. After normalisation, dot with (1,0,0)
	// should be exactly 1.0 (within float tol).
	_ = idx.Upsert("p", []float32{2, 0, 0})

	results, _ := idx.Search([]float32{1, 0, 0}, 1, 0, nil)
	if len(results) != 1 {
		t.Fatalf("expected one result")
	}
	if !approxEqual(results[0].Score, 1.0, 1e-6) {
		t.Errorf("normalised dot should be ~1.0, got %f", results[0].Score)
	}

	// Sanity check on raw normalize helper.
	v := []float32{3, 4}
	if err := normalize(v); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !approxEqual(float32(math.Sqrt(float64(v[0]*v[0]+v[1]*v[1]))), 1.0, 1e-6) {
		t.Errorf("normalised vector length not ~1.0: %v", v)
	}
}

func TestDirtyFlag(t *testing.T) {
	idx := New(2, "m")
	if idx.Dirty() {
		t.Errorf("fresh index should not be dirty")
	}
	_ = idx.Upsert("a", []float32{1, 0})
	if !idx.Dirty() {
		t.Errorf("dirty should be set after Upsert")
	}

	dir := t.TempDir()
	if err := idx.Save(filepath.Join(dir, "i.bin")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if idx.Dirty() {
		t.Errorf("dirty should be cleared after Save")
	}

	idx.Remove("a")
	if !idx.Dirty() {
		t.Errorf("dirty should be set after Remove")
	}
}

func TestSearchDimMismatch(t *testing.T) {
	idx := New(3, "m")
	_ = idx.Upsert("x", []float32{1, 0, 0})

	if _, err := idx.Search([]float32{1, 0}, 1, 0, nil); err == nil {
		t.Errorf("expected dim mismatch error")
	}
}
