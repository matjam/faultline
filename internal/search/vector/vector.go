// Package vector is a pure-Go in-memory vector index over text
// documents keyed by string paths. It supports brute-force cosine
// similarity search and persists to disk in a custom binary format
// (see serialize.go) so we don't recompute embeddings on every
// restart.
//
// At Faultline's scale (thousands of files, ~1536-dim vectors) flat
// scan is sub-millisecond per query and avoids the operational
// complexity of HNSW/IVF approximate-nearest-neighbor structures.
//
// Used as the semantic-search backend for the memory adapter; the
// lexical (BM25) and semantic results are returned separately by
// memory_search so the LLM can pick the more relevant set.
package vector

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Result is a single semantic search hit. Score is the cosine
// similarity in [-1, 1]; values closer to 1 are more similar. For
// L2-normalised vectors (which the index always produces) cosine
// similarity equals the dot product.
type Result struct {
	Path  string  `json:"path"`
	Score float32 `json:"score"`
}

// Index is an in-memory vector index. Vectors are L2-normalised on
// Upsert so Search can use plain dot products.
//
// Concurrency: read/write operations are guarded by an RWMutex.
// Search takes the read lock; Upsert/Remove/RemovePrefix/Build take
// the write lock. The dirty flag is an atomic so callers (e.g. a
// persistence goroutine) can poll it without contending the main
// lock.
type Index struct {
	mu      sync.RWMutex
	vectors map[string][]float32

	// dim is the vector dimensionality. All vectors in the index must
	// have this length. Set on first Upsert (or via Reset/Load); a
	// later Upsert with a mismatched dim is a programming error and
	// returns an error rather than corrupting the index.
	dim int

	// model is a free-form identifier for the embedding model that
	// produced the vectors. Persisted so Load can detect a
	// model/dim mismatch and signal the caller to re-embed.
	model string

	// dirty is set on every mutation and cleared on Save. A
	// background persistence loop polls it to decide whether to
	// flush.
	dirty atomic.Bool
}

// New returns a new empty Index for vectors of the given dimensionality
// produced by the named model. Both fields are persisted in the binary
// format and used to detect mismatches at Load time.
func New(dim int, model string) *Index {
	return &Index{
		vectors: make(map[string][]float32),
		dim:     dim,
		model:   model,
	}
}

// Dim returns the vector dimensionality the index was constructed for.
func (idx *Index) Dim() int { return idx.dim }

// Model returns the embedding model identifier the index was
// constructed for.
func (idx *Index) Model() string { return idx.model }

// Len returns the number of vectors currently in the index.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.vectors)
}

// Has reports whether the given path is currently indexed.
func (idx *Index) Has(path string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.vectors[path]
	return ok
}

// HasChunks reports whether path is represented in the index by
// either a bare-path entry or any "path#N" chunk entry. Used by the
// startup reconcile pass to decide whether a file already has at
// least one embedding (in which case it's left alone) versus needing
// a fresh paragraph-aware indexing pass.
//
// O(n) in the index size; fine for the few-times-a-startup cadence
// the reconcile pass runs at.
func (idx *Index) HasChunks(path string) bool {
	prefix := path + "#"

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if _, ok := idx.vectors[path]; ok {
		return true
	}
	for k := range idx.vectors {
		if hasPathChunkPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// Paths returns a snapshot of all indexed paths in unspecified order.
// Useful for the startup reconcile pass that detects which memory
// files lack a vector.
func (idx *Index) Paths() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]string, 0, len(idx.vectors))
	for p := range idx.vectors {
		out = append(out, p)
	}
	return out
}

// Dirty reports whether the index has been mutated since the last
// successful Save.
func (idx *Index) Dirty() bool { return idx.dirty.Load() }

// Upsert adds or replaces the vector for the given path. The vector is
// L2-normalised in place before storage so Search can use plain dot
// products.
//
// Returns an error if the vector's length does not match the index's
// configured dim, or if the vector is all-zero (cannot be normalised).
func (idx *Index) Upsert(path string, vec []float32) error {
	if path == "" {
		return errors.New("vector: path is empty")
	}
	if len(vec) != idx.dim {
		return fmt.Errorf("vector: dim mismatch for %q: got %d, want %d", path, len(vec), idx.dim)
	}

	// Make a defensive copy so callers can reuse their slice.
	cp := make([]float32, len(vec))
	copy(cp, vec)
	if err := normalize(cp); err != nil {
		return fmt.Errorf("vector: cannot normalise vector for %q: %w", path, err)
	}

	idx.mu.Lock()
	idx.vectors[path] = cp
	idx.mu.Unlock()
	idx.dirty.Store(true)
	return nil
}

// Remove deletes the entry for the given path if present. Returns true
// if a vector was actually removed.
func (idx *Index) Remove(path string) bool {
	idx.mu.Lock()
	_, ok := idx.vectors[path]
	if ok {
		delete(idx.vectors, path)
	}
	idx.mu.Unlock()
	if ok {
		idx.dirty.Store(true)
	}
	return ok
}

// RemovePrefix deletes all entries whose path is exactly equal to or
// starts with prefix + "/". A trailing slash on prefix is normalised
// away. Returns the number of entries removed.
func (idx *Index) RemovePrefix(prefix string) int {
	prefix = strings.TrimSuffix(prefix, "/")
	pfx := prefix + "/"

	idx.mu.Lock()
	n := 0
	for p := range idx.vectors {
		if p == prefix || strings.HasPrefix(p, pfx) {
			delete(idx.vectors, p)
			n++
		}
	}
	idx.mu.Unlock()
	if n > 0 {
		idx.dirty.Store(true)
	}
	return n
}

// Rename moves the vector currently keyed at oldPath to newPath. If
// oldPath is not indexed this is a no-op. If newPath already exists it
// is overwritten. Returns true if a vector was actually renamed.
//
// Useful for memory_move on a file with one embedding unit: the file
// content is unchanged so the embedding is unchanged; we just swap
// the key. For files with multiple chunked units, use RenameChunks.
func (idx *Index) Rename(oldPath, newPath string) bool {
	if oldPath == newPath {
		return false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	v, ok := idx.vectors[oldPath]
	if !ok {
		return false
	}
	delete(idx.vectors, oldPath)
	idx.vectors[newPath] = v
	idx.dirty.Store(true)
	return true
}

// RemoveChunks removes the bare path key plus every "path#N" key
// belonging to the same memory file. Returns the number of vectors
// removed. Used as the cleanup step before re-upserting a file's
// units (so a file that shrank from 5 chunks to 2 doesn't leave 3
// stale entries) and on memory_delete.
//
// Matching rule: a key K belongs to path P iff K == P or K starts
// with P + "#" followed by an all-digit suffix. The all-digit check
// avoids accidentally matching unrelated keys that happen to start
// with the same prefix (e.g. "foo" vs "foobar").
func (idx *Index) RemoveChunks(path string) int {
	prefix := path + "#"

	idx.mu.Lock()
	n := 0
	for k := range idx.vectors {
		if k == path {
			delete(idx.vectors, k)
			n++
			continue
		}
		if !hasPathChunkPrefix(k, prefix) {
			continue
		}
		delete(idx.vectors, k)
		n++
	}
	idx.mu.Unlock()
	if n > 0 {
		idx.dirty.Store(true)
	}
	return n
}

// RenameChunks moves every vector keyed at oldPath or oldPath#N over
// to newPath or newPath#N respectively. Returns the count of vectors
// moved.
//
// Used by memory_move on a multi-chunk file: file content is unchanged
// so embeddings are unchanged; we just swap the key prefix. For a
// single-chunk file this is equivalent to Rename.
func (idx *Index) RenameChunks(oldPath, newPath string) int {
	if oldPath == newPath {
		return 0
	}
	oldPrefix := oldPath + "#"

	idx.mu.Lock()
	defer idx.mu.Unlock()

	type pair struct {
		oldKey, newKey string
		vec            []float32
	}
	var moves []pair

	for k, v := range idx.vectors {
		switch {
		case k == oldPath:
			moves = append(moves, pair{oldKey: k, newKey: newPath, vec: v})
		case hasPathChunkPrefix(k, oldPrefix):
			suffix := k[len(oldPath):] // includes the leading '#'
			moves = append(moves, pair{oldKey: k, newKey: newPath + suffix, vec: v})
		}
	}

	for _, m := range moves {
		delete(idx.vectors, m.oldKey)
		idx.vectors[m.newKey] = m.vec
	}
	if len(moves) > 0 {
		idx.dirty.Store(true)
	}
	return len(moves)
}

// hasPathChunkPrefix reports whether k begins with prefix (which must
// end in "#") and the remainder is one or more digits with nothing
// after them. This is the sole criterion used to identify a chunk key
// as belonging to a particular memory path.
func hasPathChunkPrefix(k, prefix string) bool {
	if len(k) <= len(prefix) {
		return false
	}
	if k[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	return true
}

// Reset replaces the index contents with a fresh, empty map and sets
// the dim and model. Used when Load detects a model/dim mismatch and
// the caller needs to rebuild from scratch.
func (idx *Index) Reset(dim int, model string) {
	idx.mu.Lock()
	idx.vectors = make(map[string][]float32)
	idx.dim = dim
	idx.model = model
	idx.mu.Unlock()
	idx.dirty.Store(true)
}

// Search returns up to k results ranked by descending cosine
// similarity. Results with score below minScore are dropped. If
// filter is non-nil, only paths for which filter(path) returns true
// are considered.
//
// The query vector is normalised in place by Search; callers do not
// need to normalise it themselves but should not assume the slice is
// preserved unchanged.
func (idx *Index) Search(query []float32, k int, minScore float32, filter func(string) bool) ([]Result, error) {
	if len(query) != idx.dim {
		return nil, fmt.Errorf("vector: query dim mismatch: got %d, want %d", len(query), idx.dim)
	}
	if k <= 0 {
		return nil, nil
	}
	if err := normalize(query); err != nil {
		return nil, fmt.Errorf("vector: cannot normalise query: %w", err)
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.vectors) == 0 {
		return nil, nil
	}

	// Single pass: compute scores, keep top-k via partial sort.
	out := make([]Result, 0, len(idx.vectors))
	for path, v := range idx.vectors {
		if filter != nil && !filter(path) {
			continue
		}
		score := dot(query, v)
		if score < minScore {
			continue
		}
		out = append(out, Result{Path: path, Score: score})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})

	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// dot computes the dot product of two equal-length vectors. Caller
// guarantees lengths match.
func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// normalize divides v in place by its L2 norm. Returns an error if v
// is the zero vector (norm is exactly 0).
func normalize(v []float32) error {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return errors.New("zero vector")
	}
	inv := float32(1.0 / math.Sqrt(sumSq))
	for i := range v {
		v[i] *= inv
	}
	return nil
}
