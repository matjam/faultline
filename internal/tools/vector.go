package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// rebuildIndexes is the handler for the rebuild_indexes tool. It does
// a full rebuild of one or both search indexes from current disk
// state and returns a human-readable summary the LLM can pass through
// to the operator.
func (te *Executor) rebuildIndexes(ctx context.Context, argsJSON string) string {
	var args struct {
		Scope string `json:"scope"`
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %s", err)
		}
	}

	scope := strings.ToLower(strings.TrimSpace(args.Scope))
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "lexical", "semantic":
	default:
		return fmt.Sprintf("Error: invalid scope %q (expected 'all', 'lexical', or 'semantic')", args.Scope)
	}

	rebuildLexical := scope == "all" || scope == "lexical"
	rebuildSemantic := scope == "all" || scope == "semantic"

	var sb strings.Builder
	sb.WriteString("Index rebuild summary:\n")

	// Lexical (BM25) — always available, always cheap.
	if rebuildLexical {
		start := time.Now()
		docs, err := te.memory.AllFiles()
		if err != nil {
			fmt.Fprintf(&sb, "  - BM25 (lexical): FAILED to list memory: %s\n", err)
		} else {
			te.index.Build(docs)
			fmt.Fprintf(&sb, "  - BM25 (lexical): %d documents indexed in %s.\n",
				len(docs), time.Since(start).Round(time.Millisecond))
			te.logger.Info("rebuild_indexes: BM25 rebuilt",
				slog.Int("documents", len(docs)),
				slog.Duration("duration", time.Since(start)))
		}
	}

	// Semantic (vector) — only when configured.
	if rebuildSemantic {
		switch {
		case !te.vectorEnabled():
			sb.WriteString("  - Vector (semantic): SKIPPED — semantic search is not configured ([embeddings].enabled=false or probe failed at startup).\n")
		default:
			res, err := RebuildVectorIndex(ctx, te.embedder, te.vIndex, te.memory, te.embedBatchSize, te.logger)
			if err != nil {
				fmt.Fprintf(&sb, "  - Vector (semantic): PARTIAL — %d/%d embedded in %s before failure: %s\n",
					res.Embedded, res.Eligible, res.Duration.Round(time.Millisecond), err)
			} else {
				fmt.Fprintf(&sb, "  - Vector (semantic): %d documents embedded in %s (%d eligible files; ineligible=prompts/.trash/empty).\n",
					res.Embedded, res.Duration.Round(time.Millisecond), res.Eligible)
			}
		}
	}

	if !rebuildLexical && !rebuildSemantic {
		// Defensive; the validation above prevents this.
		return "Error: nothing to rebuild for the given scope."
	}

	sb.WriteString("\nNote: incremental updates from memory_write/edit/append/insert/delete/move/restore keep both indexes in sync between rebuilds. ")
	sb.WriteString("Avoid calling rebuild_indexes routinely.")
	return sb.String()
}

// ShouldIndexMemoryPath reports whether a memory path is eligible for
// semantic indexing. Exported so main.go's reconcile pass and any
// future caller can apply the same exclusion rule (skip prompts/ and
// .trash/) without duplicating the predicate.
func ShouldIndexMemoryPath(path string) bool {
	return shouldIndexPath(path)
}

// vectorBatchTimeout bounds a single /v1/embeddings batch call during
// bulk reconcile/rebuild. Independent of the per-mutation timeout
// (vectorEmbedTimeout) because batches can legitimately take longer
// than a single-file embed; a hung server still can't stall the
// whole pass forever.
const vectorBatchTimeout = 60 * time.Second

// VectorBuildResult summarizes a Reconcile/Rebuild pass.
type VectorBuildResult struct {
	// Embedded is the number of vectors successfully written this pass.
	Embedded int
	// Eligible is the number of memory files that qualified for
	// embedding (after exclusion of prompts/, .trash/, and empty files).
	Eligible int
	// Skipped is the number of eligible files that were already in
	// the index and not re-embedded. Always 0 for a full Rebuild;
	// for Reconcile, this is Eligible - Embedded when no errors hit.
	Skipped int
	// Duration is wall-clock time for the pass.
	Duration time.Duration
}

// ReconcileVectorIndex embeds memory files that are eligible but
// missing from the index. Files already present are left as-is. Used
// at startup so a restart with an existing on-disk index doesn't
// re-pay the embedding cost for files that haven't changed.
//
// Empty files are skipped. Files larger than maxEmbedInputBytes are
// truncated to that prefix (matching the per-mutation reindex
// behavior). Errors abort the pass; vectors written before the
// error are preserved in the index.
func ReconcileVectorIndex(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, logger *slog.Logger) (VectorBuildResult, error) {
	return runVectorBuild(ctx, embedder, idx, memory, batchSize, false, logger)
}

// RebuildVectorIndex clears the in-memory vector index and re-embeds
// every eligible memory file from scratch. Used by the
// rebuild_indexes tool when the LLM observes inconsistency or the
// operator explicitly asks for a rebuild.
//
// The reset uses the embedder's current dim and model identifier, so
// switching embedders is not the intended use of this function (that
// happens automatically at startup via ErrModelMismatch).
func RebuildVectorIndex(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, logger *slog.Logger) (VectorBuildResult, error) {
	idx.Reset(embedder.Dim(), embedder.Model())
	return runVectorBuild(ctx, embedder, idx, memory, batchSize, true, logger)
}

// runVectorBuild is the shared embed-and-upsert loop used by both
// Reconcile (full=false: skip files already in the index) and
// Rebuild (full=true: index has been cleared so every file qualifies).
func runVectorBuild(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, full bool, logger *slog.Logger) (VectorBuildResult, error) {
	start := time.Now()
	res := VectorBuildResult{}

	all, err := memory.AllFiles()
	if err != nil {
		return res, fmt.Errorf("list memory files: %w", err)
	}

	type pending struct {
		path string
		text string
	}
	var queue []pending

	for path, content := range all {
		if !ShouldIndexMemoryPath(path) {
			continue
		}
		text := strings.TrimSpace(content)
		if text == "" {
			continue
		}
		res.Eligible++
		if !full && idx.Has(path) {
			res.Skipped++
			continue
		}
		if len(text) > maxEmbedInputBytes {
			text = text[:maxEmbedInputBytes]
		}
		queue = append(queue, pending{path: path, text: text})
	}

	if len(queue) == 0 {
		res.Duration = time.Since(start)
		mode := "reconcile"
		if full {
			mode = "rebuild"
		}
		logger.Info("vector "+mode+": nothing to do",
			slog.Int("eligible", res.Eligible),
			slog.Int("skipped", res.Skipped))
		return res, nil
	}

	if batchSize <= 0 {
		batchSize = 100
	}

	mode := "reconcile"
	if full {
		mode = "rebuild"
	}
	logger.Info("vector "+mode+" pass starting",
		slog.Int("count", len(queue)),
		slog.Int("batch_size", batchSize))

	for i := 0; i < len(queue); i += batchSize {
		end := i + batchSize
		if end > len(queue) {
			end = len(queue)
		}
		batch := queue[i:end]

		texts := make([]string, len(batch))
		for j, p := range batch {
			texts[j] = p.text
		}

		batchCtx, cancel := context.WithTimeout(ctx, vectorBatchTimeout)
		vecs, err := embedder.Embed(batchCtx, texts)
		cancel()
		if err != nil {
			res.Duration = time.Since(start)
			return res, fmt.Errorf("embed batch starting at %d: %w", i, err)
		}
		if len(vecs) != len(batch) {
			res.Duration = time.Since(start)
			return res, fmt.Errorf("embed batch starting at %d: got %d vecs for %d inputs", i, len(vecs), len(batch))
		}
		for j, p := range batch {
			if err := idx.Upsert(p.path, vecs[j]); err != nil {
				logger.Warn("vector "+mode+": upsert failed; continuing",
					slog.String("path", p.path),
					slog.String("err", err.Error()))
				continue
			}
			res.Embedded++
		}

		select {
		case <-ctx.Done():
			res.Duration = time.Since(start)
			logger.Info("vector "+mode+": canceled mid-pass",
				slog.Int("embedded", res.Embedded),
				slog.Int("total", len(queue)))
			return res, ctx.Err()
		default:
		}
	}

	res.Duration = time.Since(start)
	logger.Info("vector "+mode+" complete",
		slog.Int("embedded", res.Embedded),
		slog.Int("eligible", res.Eligible),
		slog.Int("skipped", res.Skipped),
		slog.Duration("duration", res.Duration))
	return res, nil
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
