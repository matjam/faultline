package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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

// reindexBatchTimeout caps a single per-mutation reindex (one file,
// possibly several paragraphs). Independent of the per-request HTTP
// timeout in the embedder; this is the wall-clock budget for the
// whole adaptive batching loop on one file.
const reindexBatchTimeout = 60 * time.Second

// vectorBatchTimeout bounds a single /v1/embeddings call inside the
// adaptive batcher. A hung server still can't stall a reconcile or a
// per-mutation reindex forever.
const vectorBatchTimeout = 60 * time.Second

// batchGrowSuccesses is how many consecutive successful batches the
// adaptive batcher requires before it tries doubling the current
// batch size back toward the configured ceiling. Conservative: a
// single failure on the freshly-doubled size demotes us again.
const batchGrowSuccesses = 5

// progressLogInterval is the minimum wall-clock gap between two
// progress log lines emitted from the adaptive batcher. Bounds the
// log rate on small-batch slogs (e.g. after a halve event when each
// batch is just a few seconds).
const progressLogInterval = 30 * time.Second

// progressLogPercentStep is the minimum percent advance that triggers
// an early progress log even if progressLogInterval has not yet
// elapsed. Together they give: at least one line every 30s on a slow
// pass, and at most 10 lines on a fast pass that finishes in under a
// minute. Quiet on small reconciles (a handful of paragraphs won't
// cross either threshold).
const progressLogPercentStep = 10

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

// reindexVector splits the file at path into paragraph-aligned units,
// embeds them via the adaptive batcher, and replaces any prior chunks
// for that file in the vector index. No-op when the feature is
// disabled, the path is operational, or the file body is empty.
//
// Failure mode: any embed error inside the adaptive batcher is logged
// and per-paragraph skips are tolerated (the file ends up partially
// indexed). The vector index is best-effort enrichment; a failed
// embed never blocks the memory write that triggered it.
func (te *Executor) reindexVector(path, content string) {
	if !te.vectorEnabled() || !shouldIndexPath(indexKey(path)) {
		return
	}

	units := splitIntoUnits(content)
	if len(units) == 0 {
		// Empty / whitespace-only file. Drop any prior chunks so
		// the index doesn't carry a stale embedding for content the
		// agent has effectively deleted.
		te.vIndex.RemoveChunks(indexKey(path))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), reindexBatchTimeout)
	defer cancel()

	// Per-mutation: try to embed all units in one shot. The adaptive
	// batcher will fall back if the server can't take them at once.
	vecs, skipped, err := embedWithAdaptiveBatching(ctx, te.embedder, units, len(units), te.logger)
	if err != nil {
		te.logger.Warn("reindex failed",
			slog.String("path", path),
			slog.String("err", err.Error()))
		return
	}

	// Replace any existing chunks for this file with the new ones.
	// Done AFTER successful embed so a failed reindex doesn't leave
	// the index in a worse state than before.
	te.vIndex.RemoveChunks(indexKey(path))

	upserted := 0
	total := len(units)
	for i, v := range vecs {
		if v == nil {
			continue // skipped paragraph (failed even at batch size 1)
		}
		key := unitKey(indexKey(path), i, total)
		if uerr := te.vIndex.Upsert(key, v); uerr != nil {
			te.logger.Warn("vector upsert failed",
				slog.String("key", key),
				slog.String("err", uerr.Error()))
			continue
		}
		upserted++
	}
	if skipped > 0 {
		te.logger.Warn("reindex partial: some paragraphs could not be embedded",
			slog.String("path", path),
			slog.Int("upserted", upserted),
			slog.Int("skipped", skipped),
			slog.Int("total", total))
	}
}

// removeVector clears every chunk belonging to path from the index.
// No-op when disabled.
func (te *Executor) removeVector(path string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.RemoveChunks(indexKey(path))
}

// removeVectorPrefix deletes every vector whose key sits under a
// directory prefix. Chunk keys naturally satisfy this because they
// are formed as `path#N` and `path` already carries the directory
// prefix; the underlying RemovePrefix walks all keys with the
// `<prefix>/` prefix and catches both bare and chunked entries.
func (te *Executor) removeVectorPrefix(path string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.RemovePrefix(indexKey(path))
}

// renameVector moves all chunks of a file from oldPath to newPath
// without re-embedding (file content is unchanged). Used after
// memory_move on a single file.
func (te *Executor) renameVector(oldPath, newPath string) {
	if !te.vectorEnabled() {
		return
	}
	te.vIndex.RenameChunks(indexKey(oldPath), indexKey(newPath))
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

// embedQuery is a thin wrapper around the embedder for use by the
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

// VectorIndex returns the current vector index. May be nil. Provided
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
			"via embedding similarity over paragraph-aligned chunks (the most-similar paragraph of each file " +
			"is shown as the snippet). Each section has up to 5 results with file path, score, and content. " +
			"The two sections often surface different files — pick whichever is more relevant for your query, " +
			"or read both. Use lexical when you remember specific words; use semantic when you remember the " +
			"topic but not the wording. Long results are clipped with a hint pointing back at memory_read so " +
			"you can load the full file. Optionally filter by file modification date (applies to both sections)."
	}
	return "Search across all memory files by keyword relevance (BM25). Returns up to 5 results, each with: " +
		"file path, relevance score, and content. Long results are clipped with a hint pointing back at " +
		"memory_read so you can load the full file. Use this to find memories by topic when you don't know " +
		"the exact file path. Optionally filter by file modification date."
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
			renderBuildResult(&sb, "Vector (semantic) rebuild", res, err)
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

// renderBuildResult formats a VectorBuildResult into the summary
// builder. Shared by rebuild_indexes (and could be reused if we ever
// expose reconcile output to the LLM).
func renderBuildResult(sb *strings.Builder, label string, res VectorBuildResult, err error) {
	prefix := fmt.Sprintf("  - %s:", label)
	if err != nil {
		fmt.Fprintf(sb, "%s PARTIAL — %d files / %d paragraphs embedded in %s before failure (skipped %d): %s\n",
			prefix, res.FilesEmbedded, res.UnitsEmbedded, res.Duration.Round(time.Millisecond), res.UnitsSkipped, err)
		return
	}
	if res.UnitsSkipped > 0 {
		fmt.Fprintf(sb, "%s %d files / %d paragraphs embedded in %s. Skipped %d paragraph(s) that the embedder rejected even at batch size 1 — those sections are not in semantic search but BM25 still finds them.\n",
			prefix, res.FilesEmbedded, res.UnitsEmbedded, res.Duration.Round(time.Millisecond), res.UnitsSkipped)
		return
	}
	fmt.Fprintf(sb, "%s %d files / %d paragraphs embedded in %s.\n",
		prefix, res.FilesEmbedded, res.UnitsEmbedded, res.Duration.Round(time.Millisecond))
}

// ShouldIndexMemoryPath reports whether a memory path is eligible for
// semantic indexing. Exported so main.go's reconcile pass and any
// future caller can apply the same exclusion rule (skip prompts/ and
// .trash/) without duplicating the predicate.
func ShouldIndexMemoryPath(path string) bool {
	return shouldIndexPath(path)
}

// VectorBuildResult summarizes a Reconcile/Rebuild pass.
type VectorBuildResult struct {
	// FilesEmbedded is the number of distinct memory files that have
	// at least one paragraph successfully indexed in this pass.
	FilesEmbedded int

	// UnitsEmbedded is the total number of paragraph-aligned units
	// (chunks) successfully written to the index.
	UnitsEmbedded int

	// UnitsSkipped is the number of units the embedder rejected even
	// at batch size 1 — typically a single paragraph exceeding the
	// model's context. Skipped units are not in the semantic index
	// but are still searchable via BM25.
	UnitsSkipped int

	// Eligible is the number of distinct memory files that qualified
	// for embedding (after exclusion of prompts/, .trash/, empty
	// files, and — for Reconcile only — files already in the index).
	Eligible int

	// Duration is wall-clock time for the pass.
	Duration time.Duration
}

// ReconcileVectorIndex embeds eligible memory files that aren't yet
// represented in the index. Files already present (any chunk for that
// path exists) are left as-is. Used at startup so a restart with an
// existing on-disk index doesn't re-pay the embedding cost for files
// that haven't changed.
func ReconcileVectorIndex(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, logger *slog.Logger) (VectorBuildResult, error) {
	return runVectorBuild(ctx, embedder, idx, memory, batchSize, false, logger)
}

// RebuildVectorIndex clears the in-memory vector index and re-embeds
// every eligible memory file from scratch. Used by the
// rebuild_indexes tool when the LLM observes inconsistency or the
// operator explicitly asks for a rebuild.
func RebuildVectorIndex(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, logger *slog.Logger) (VectorBuildResult, error) {
	idx.Reset(embedder.Dim(), embedder.Model())
	return runVectorBuild(ctx, embedder, idx, memory, batchSize, true, logger)
}

// pendingUnit is one paragraph queued for batch embedding.
type pendingUnit struct {
	path  string
	idx   int // chunk index within the file
	total int // total chunks for that file
	text  string
}

// runVectorBuild is the shared embed-and-upsert loop used by both
// Reconcile (full=false: skip files already in the index) and
// Rebuild (full=true: index has been cleared so every file qualifies).
//
// All paragraphs across all eligible files are fed into one adaptive
// batcher pass, so the batcher can amortize large batches across
// files instead of tearing down between them.
func runVectorBuild(ctx context.Context, embedder Embedder, idx *vector.Index, memory *fs.Store, batchSize int, full bool, logger *slog.Logger) (VectorBuildResult, error) {
	start := time.Now()
	res := VectorBuildResult{}

	all, err := memory.AllFiles()
	if err != nil {
		return res, fmt.Errorf("list memory files: %w", err)
	}

	var queue []pendingUnit
	for path, content := range all {
		if !ShouldIndexMemoryPath(path) {
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		// "Already in the index" for chunked files means any chunk
		// is present; for legacy single-vector files it means the
		// bare path is present.
		if !full && idx.HasChunks(path) {
			res.Eligible++
			continue
		}
		units := splitIntoUnits(content)
		if len(units) == 0 {
			continue
		}
		res.Eligible++
		for i, u := range units {
			queue = append(queue, pendingUnit{
				path:  path,
				idx:   i,
				total: len(units),
				text:  u,
			})
		}
	}

	if len(queue) == 0 {
		res.Duration = time.Since(start)
		mode := "reconcile"
		if full {
			mode = "rebuild"
		}
		logger.Info("vector "+mode+": nothing to do",
			slog.Int("eligible", res.Eligible))
		return res, nil
	}

	mode := "reconcile"
	if full {
		mode = "rebuild"
	}
	logger.Info("vector "+mode+" pass starting",
		slog.Int("paragraphs", len(queue)),
		slog.Int("batch_size", batchSize))

	texts := make([]string, len(queue))
	for i, u := range queue {
		texts[i] = u.text
	}

	vecs, skipped, embErr := embedWithAdaptiveBatching(ctx, embedder, texts, batchSize, logger)
	res.UnitsSkipped = skipped

	// Upsert whatever did embed, even if there was a fatal error
	// (e.g. ctx cancellation) — partial progress is preserved.
	filesSeen := map[string]bool{}
	for i, v := range vecs {
		if v == nil {
			continue
		}
		u := queue[i]
		key := unitKey(u.path, u.idx, u.total)
		if uerr := idx.Upsert(key, v); uerr != nil {
			logger.Warn("vector "+mode+": upsert failed; continuing",
				slog.String("key", key),
				slog.String("err", uerr.Error()))
			continue
		}
		filesSeen[u.path] = true
		res.UnitsEmbedded++
	}
	res.FilesEmbedded = len(filesSeen)
	res.Duration = time.Since(start)

	if embErr != nil {
		logger.Warn("vector "+mode+": ended with error",
			slog.String("err", embErr.Error()),
			slog.Int("files", res.FilesEmbedded),
			slog.Int("paragraphs", res.UnitsEmbedded),
			slog.Int("skipped", res.UnitsSkipped))
		return res, embErr
	}

	logger.Info("vector "+mode+" complete",
		slog.Int("files", res.FilesEmbedded),
		slog.Int("paragraphs", res.UnitsEmbedded),
		slog.Int("skipped", res.UnitsSkipped),
		slog.Duration("duration", res.Duration))
	return res, nil
}

// embedWithAdaptiveBatching embeds a slice of texts via the embedder,
// dynamically halving the batch size on failure and growing it back
// after a streak of successes. Failures at batch size 1 cause that
// individual text to be skipped (corresponding output slot stays nil)
// and the loop continues with the next.
//
// Returns:
//   - vecs: one vector per input text in input order; nil for any
//     text that was skipped (failed even at batch size 1).
//   - skipped: count of nil entries in vecs.
//   - err: non-nil only if ctx is canceled mid-pass; recoverable
//     batch failures are absorbed and never surface as err.
//
// configuredBatchSize is treated as the upper bound; the runtime
// batch size starts at min(configuredBatchSize, len(texts)) and only
// grows back toward configuredBatchSize after enough successes.
func embedWithAdaptiveBatching(ctx context.Context, embedder Embedder, texts []string, configuredBatchSize int, logger *slog.Logger) ([][]float32, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	if configuredBatchSize <= 0 {
		configuredBatchSize = 100
	}

	out := make([][]float32, len(texts))
	batchSize := configuredBatchSize
	if batchSize > len(texts) {
		batchSize = len(texts)
	}

	var (
		skipped   int
		successes int
		i         int
	)

	// Progress reporting state. lastLogTime starts at the entry to the
	// batcher so the first progress line shows up at most
	// progressLogInterval after start (rather than progressLogInterval
	// after the first successful batch). lastLogPercent is the last
	// reported percent rounded down to the nearest progressLogPercentStep.
	startTime := time.Now()
	lastLogTime := startTime
	lastLogPercent := 0

	for i < len(texts) {
		// Honor outer cancellation between batches.
		if err := ctx.Err(); err != nil {
			return out, skipped, err
		}

		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		bctx, cancel := context.WithTimeout(ctx, vectorBatchTimeout)
		result, err := embedder.Embed(bctx, batch)
		cancel()

		// Treat "wrong count" as a soft failure — same recovery path
		// as a real error.
		if err == nil && len(result) != len(batch) {
			err = fmt.Errorf("embedder returned %d vectors for %d inputs", len(result), len(batch))
		}

		if err == nil {
			copy(out[i:end], result)
			i = end
			successes++
			if successes >= batchGrowSuccesses && batchSize < configuredBatchSize {
				newSize := batchSize * 2
				if newSize > configuredBatchSize {
					newSize = configuredBatchSize
				}
				logger.Debug("embed batch growing after success streak",
					slog.Int("from", batchSize),
					slog.Int("to", newSize))
				batchSize = newSize
				successes = 0
			}

			// Periodic progress log. Throttled so a small reconcile
			// stays quiet but a long pass (e.g. model swap rebuilding
			// thousands of paragraphs) gets a heartbeat the operator
			// can see. Triggers when either the percent advance crosses
			// the next progressLogPercentStep bucket OR the wall-clock
			// gap exceeds progressLogInterval. Final summary is logged
			// by the caller (runVectorBuild) so we don't double up at
			// 100%.
			if i < len(texts) {
				percent := i * 100 / len(texts)
				now := time.Now()
				bucket := percent - (percent % progressLogPercentStep)
				if bucket > lastLogPercent || now.Sub(lastLogTime) >= progressLogInterval {
					elapsed := now.Sub(startTime).Round(time.Second)
					var rate float64
					if elapsed > 0 {
						rate = float64(i) / elapsed.Seconds()
					}
					logger.Info("embed: progress",
						slog.Int("done", i),
						slog.Int("total", len(texts)),
						slog.Int("percent", percent),
						slog.Float64("rate_par_per_s", rate),
						slog.Duration("elapsed", elapsed),
						slog.Int("batch_size", batchSize),
						slog.Int("skipped", skipped))
					lastLogTime = now
					lastLogPercent = bucket
				}
			}
			continue
		}

		// If the outer ctx is the cause, surface it; we can't recover.
		if cerr := ctx.Err(); cerr != nil {
			return out, skipped, cerr
		}

		successes = 0

		if batchSize == 1 {
			// Single-input failure: this paragraph is the problem.
			// Skip it and continue — the rest of the pass should
			// still succeed.
			logger.Warn("embed: skipping paragraph after single-input failure",
				slog.Int("input_index", i),
				slog.Int("input_bytes", len(texts[i])),
				slog.String("err", err.Error()))
			out[i] = nil
			skipped++
			i++
			continue
		}

		// Batch failure with size > 1: halve and retry the same
		// range. Don't advance i.
		newSize := batchSize / 2
		if newSize < 1 {
			newSize = 1
		}
		logger.Warn("embed batch failed; halving batch size",
			slog.Int("from", batchSize),
			slog.Int("to", newSize),
			slog.Int("range_start", i),
			slog.String("err", err.Error()))
		batchSize = newSize
	}

	return out, skipped, nil
}

// chunkIdxFromUnitKey returns the numeric chunk index encoded in a
// unit key (e.g. 3 for "notes/foo.md#3"), or -1 if the key has no
// chunk suffix.
func chunkIdxFromUnitKey(key string) int {
	if i := strings.LastIndexByte(key, '#'); i > 0 {
		suffix := key[i+1:]
		if suffix != "" && allDigits(suffix) {
			n, err := strconv.Atoi(suffix)
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// fileLevelHits collapses the raw per-paragraph search results down
// to one entry per file, keeping each file's highest-scoring chunk.
// Used by memory_search to present file-granular results to the LLM.
//
// rawHits should already be sorted by descending score; the iteration
// preserves the first (i.e. best) hit per path. The returned slice is
// truncated to limit and sorted by descending score.
func fileLevelHits(rawHits []vector.Result, limit int) []vector.Result {
	if limit <= 0 || len(rawHits) == 0 {
		return nil
	}
	// Caller may pass in unsorted; sort defensively. Stable so equal
	// scores keep the index-traversal order chosen by Search.
	sort.SliceStable(rawHits, func(i, j int) bool { return rawHits[i].Score > rawHits[j].Score })

	type fileHit struct {
		key   string  // original key (with #N if chunked)
		path  string  // file path (no #N)
		score float32 // best score across this file's chunks
	}
	seen := map[string]int{} // path -> index in out
	out := make([]fileHit, 0, limit)
	for _, h := range rawHits {
		p := pathFromUnitKey(h.Path)
		if _, ok := seen[p]; ok {
			continue
		}
		out = append(out, fileHit{key: h.Path, path: p, score: h.Score})
		seen[p] = len(out) - 1
		if len(out) >= limit {
			break
		}
	}

	// Convert back to []vector.Result so callers don't need to know
	// about fileHit. Path is the bare file path (post-dedup), Score
	// is the best chunk's score. The chunked-key is recoverable via
	// chunkIdxFromUnitKey if we keep it — emit it via Path so callers
	// can re-derive the chunk index. Two-stage: keep both via a side
	// channel.
	results := make([]vector.Result, len(out))
	for i, h := range out {
		results[i] = vector.Result{Path: h.key, Score: h.score}
	}
	return results
}
