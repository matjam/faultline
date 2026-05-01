package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/adapters/memory/fs"
	"github.com/matjam/faultline/internal/search/vector"
)

// Embedder is the consumer-side contract for an embeddings backend.
// Implemented by *internal/adapters/embeddings/openai.Client. Defined
// here (where it's consumed) rather than in the agent's ports.go
// because the agent loop never embeds — only the tool dispatcher does.
type Embedder interface {
	// Embed returns one vector per input string, in input order. An
	// implementation MUST return either len(texts) vectors or a
	// non-nil error.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dim returns the vector dimensionality. Must be stable for the
	// lifetime of the embedder; called by the dispatcher to allocate
	// query buffers.
	Dim() int

	// Model is a free-form identifier persisted alongside the index
	// so a model change triggers a rebuild on next startup.
	Model() string
}

// maxEmbedInputBytes is the byte-length cap applied to file content
// before it's sent to the embeddings API. text-embedding-3-small
// accepts up to 8192 tokens (~30 KB of typical English). We truncate
// rather than chunk for v1 because:
//   - typical memory files are well under this limit;
//   - chunking adds complexity (multi-vector files, score aggregation,
//     dedup on return) that doesn't pay off until we see real
//     long-document use cases.
//
// When this limit fires, the embedding represents only the prefix.
// Truncation is logged at debug level.
const maxEmbedInputBytes = 30000

// vectorEmbedTimeout is the per-mutation embed call timeout.
// Independent of the per-request timeout in the embedder itself
// (which applies to bulk reconciles too); this exists so that
// embed-on-mutation never blocks a memory-write tool for too long
// even if the configured embeddings.timeout is generous.
const vectorEmbedTimeout = 30 * time.Second

// vectorEnabled reports whether semantic indexing is wired up. Both
// the embedder and the index must be non-nil; either being nil is
// the "feature disabled" path and all helper methods become no-ops.
func (te *Executor) vectorEnabled() bool {
	return te.embedder != nil && te.vIndex != nil
}

// shouldIndexPath reports whether a path is eligible for semantic
// indexing. Operational paths — the prompts/ tree (self-modifying
// agent prompts) and the trash — are excluded so semantic search
// never surfaces them as memory hits.
func shouldIndexPath(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "prompts/") || path == "prompts" {
		return false
	}
	if fs.IsTrashPath(path) {
		return false
	}
	return true
}

// truncateForEmbedding returns text trimmed to maxEmbedInputBytes if
// it would otherwise exceed the embedding model's input limit. Returns
// the (possibly trimmed) text and a bool indicating whether truncation
// occurred.
func truncateForEmbedding(text string) (string, bool) {
	if len(text) <= maxEmbedInputBytes {
		return text, false
	}
	return text[:maxEmbedInputBytes], true
}

// reindexVector embeds the current content of the file at path and
// upserts the result into the vector index. No-op when the feature is
// disabled, when the path is operational, or when the file content is
// empty (vector indexing of empty documents would produce a zero
// vector that can't be normalised).
//
// Failure mode: any error is logged at warn and swallowed. The vector
// index is best-effort enrichment; a failed embed never blocks the
// memory write that triggered it. The unindexed file is reachable on
// the next mutation or via the startup reconcile pass.
func (te *Executor) reindexVector(path, content string) {
	if !te.vectorEnabled() || !shouldIndexPath(indexKey(path)) {
		return
	}
	if strings.TrimSpace(content) == "" {
		// Don't index empty files — the embedding API typically
		// rejects empty input and even when it doesn't, the resulting
		// vector is meaningless. Remove any stale entry.
		te.vIndex.Remove(indexKey(path))
		return
	}

	text, truncated := truncateForEmbedding(content)
	if truncated {
		te.logger.Debug("embedding truncated for indexing",
			slog.String("path", path),
			slog.Int("orig_bytes", len(content)),
			slog.Int("sent_bytes", len(text)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), vectorEmbedTimeout)
	defer cancel()

	vecs, err := te.embedder.Embed(ctx, []string{text})
	if err != nil {
		te.logger.Warn("embed-on-mutation failed; file will remain unindexed",
			slog.String("path", path),
			slog.String("err", err.Error()))
		return
	}
	if len(vecs) != 1 {
		te.logger.Warn("embed-on-mutation returned wrong count",
			slog.String("path", path),
			slog.Int("got", len(vecs)))
		return
	}

	if err := te.vIndex.Upsert(indexKey(path), vecs[0]); err != nil {
		te.logger.Warn("vector upsert failed",
			slog.String("path", path),
			slog.String("err", err.Error()))
	}
}

// removeVector removes the vector for path from the index. No-op when
// disabled.
func (te *Executor) removeVector(path string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.Remove(indexKey(path))
}

// removeVectorPrefix removes all vectors under a directory prefix.
// path should be the directory key without a trailing slash; this
// helper appends one before delegating, matching the BM25 index's
// RemovePrefix semantics.
func (te *Executor) removeVectorPrefix(path string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.RemovePrefix(indexKey(path))
}

// renameVector moves a vector from oldPath to newPath without re-
// embedding (the file content hasn't changed). Used after memory_move
// on individual files.
func (te *Executor) renameVector(oldPath, newPath string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.Rename(indexKey(oldPath), indexKey(newPath))
}

// reindexVectorDir walks all files under the given memory directory
// and re-embeds each one. Used after directory-level operations
// (memory_move on a directory, memory_restore of a trashed directory)
// where individual file paths changed.
//
// Errors during the walk or per-file embeds are logged but do not
// abort the walk; one bad file shouldn't leave the rest unindexed.
func (te *Executor) reindexVectorDir(dirPath string) {
	if !te.vectorEnabled() {
		return
	}
	entries, err := te.memory.List(dirPath)
	if err != nil {
		return
	}
	for _, e := range entries {
		subPath := dirPath + "/" + e.Name
		if e.IsDir {
			te.reindexVectorDir(subPath)
			continue
		}
		content, readErr := te.memory.Read(subPath)
		if readErr != nil {
			continue
		}
		te.reindexVector(subPath, content)
	}
}

// EmbedQuery is a thin wrapper around the embedder for use by the
// memory_search handler. Returns an error if the embedder is not
// configured (caller should fall back to lexical-only output).
func (te *Executor) embedQuery(ctx context.Context, query string) ([]float32, error) {
	if te.embedder == nil {
		return nil, errors.New("embeddings disabled")
	}
	vecs, err := te.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, errors.New("embed query: empty result")
	}
	return vecs[0], nil
}

// vectorIndex returns the current vector index. May be nil. Provided
// so wiring code (main.go) can call Save() at shutdown without poking
// at unexported fields.
func (te *Executor) VectorIndex() *vector.Index {
	return te.vIndex
}

// memorySearchDescription returns the LLM-facing description for the
// memory_search tool. The description varies based on whether
// semantic search is wired up so the LLM's mental model matches the
// tool's actual output shape.
func (te *Executor) memorySearchDescription() string {
	if te.vectorEnabled() {
		return "Search across all memory files. Returns TWO labeled result sections per query: " +
			"'Lexical results (BM25)' for keyword matches, and 'Semantic results' for meaning-based matches " +
			"via embedding similarity. Each section has up to 5 results with file path, score, and content. " +
			"The two sections often surface different files — pick whichever is more relevant for your query, " +
			"or read both. Use lexical when you remember specific words; use semantic when you remember " +
			"the topic but not the wording. Long results are clipped with a hint pointing back at memory_read " +
			"so you can load the full file. Optionally filter by file modification date (applies to both sections)."
	}
	return "Search across all memory files by keyword relevance (BM25). Returns up to 5 results, each with: " +
		"file path, relevance score, and content. Long results are clipped with a hint pointing back at " +
		"memory_read so you can load the full file. Use this to find memories by topic when you don't know " +
		"the exact file path. Optionally filter by file modification date."
}
