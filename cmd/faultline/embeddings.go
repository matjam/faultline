package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	openaiembed "github.com/matjam/faultline/internal/adapters/embeddings/openai"
	"github.com/matjam/faultline/internal/adapters/memory/fs"
	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/search/vector"
	"github.com/matjam/faultline/internal/tools"
)

// vectorPersistInterval is how often the persistence loop checks the
// dirty flag and flushes if set. Bounded data-loss window between
// crashes. Tuned long enough that bulk ingests don't churn the disk
// but short enough to avoid losing significant work.
const vectorPersistInterval = 30 * time.Second

// vectorIndexPath returns the on-disk location of the persisted vector
// index for a given memory root.
func vectorIndexPath(memoryDir string) string {
	return filepath.Join(memoryDir, ".vector", "index.bin")
}

// setupEmbeddings constructs and probes the embeddings client, then
// builds an in-memory vector index, loads any prior on-disk state, and
// runs a startup reconcile pass that embeds any memory file lacking a
// vector. On any failure beyond the probe, returns nil/nil — the
// agent runs with semantic search disabled rather than failing to
// start over an optional feature.
func setupEmbeddings(ctx context.Context, cfg *config.Config, memory *fs.Store, logger *slog.Logger) (tools.Embedder, *vector.Index) {
	logger.Info("embeddings: setup starting",
		slog.String("url", cfg.Embeddings.URL),
		slog.String("model", cfg.Embeddings.Model))

	client := openaiembed.New(
		cfg.Embeddings.URL,
		cfg.Embeddings.APIKey,
		cfg.Embeddings.Model,
		cfg.Embeddings.Timeout.Duration(),
		logger,
	)

	probeCtx, cancel := context.WithTimeout(ctx, cfg.Embeddings.Timeout.Duration()+5*time.Second)
	defer cancel()
	if err := client.Probe(probeCtx); err != nil {
		logger.Warn("embeddings: probe failed; semantic search disabled for this session",
			slog.String("err", err.Error()))
		return nil, nil
	}

	dim := client.Dim()
	idx := vector.New(dim, cfg.Embeddings.Model)

	indexPath := vectorIndexPath(cfg.Agent.MemoryDir)
	if err := idx.Load(indexPath); err != nil {
		switch {
		case errors.Is(err, vector.ErrModelMismatch):
			logger.Info("embeddings: on-disk index has different model/dim; rebuilding from scratch",
				slog.String("path", indexPath))
			idx.Reset(dim, cfg.Embeddings.Model)
		case errors.Is(err, vector.ErrCorrupt):
			// Move aside and continue with a fresh index, mirroring
			// the pattern used by jsonfile state.
			bad := fmt.Sprintf("%s.bad-%d", indexPath, time.Now().Unix())
			logger.Warn("embeddings: on-disk index corrupt; renaming aside and rebuilding",
				slog.String("path", indexPath),
				slog.String("moved_to", bad),
				slog.String("err", err.Error()))
			_ = os.Rename(indexPath, bad)
			idx.Reset(dim, cfg.Embeddings.Model)
		default:
			logger.Warn("embeddings: load failed; rebuilding from scratch",
				slog.String("err", err.Error()))
			idx.Reset(dim, cfg.Embeddings.Model)
		}
	} else {
		logger.Info("embeddings: loaded on-disk index",
			slog.Int("vectors", idx.Len()),
			slog.Int("dim", dim))
	}

	// Reconcile: embed any memory file that's not in the index.
	// Skipped for files in operational paths (prompts/, .trash/).
	if err := reconcileVectorIndex(ctx, client, idx, memory, cfg.Embeddings.BatchSize, logger); err != nil {
		logger.Warn("embeddings: reconcile incomplete; continuing with partial index",
			slog.String("err", err.Error()))
	}

	return client, idx
}

// reconcileVectorIndex finds memory files lacking a vector and embeds
// them in batches. Files larger than the per-input cap are truncated
// (see tools/vector.go for the cap). Empty files are skipped.
//
// Returns the first error encountered; partial progress is preserved
// in the index either way.
func reconcileVectorIndex(ctx context.Context, embedder tools.Embedder, idx *vector.Index, memory *fs.Store, batchSize int, logger *slog.Logger) error {
	all, err := memory.AllFiles()
	if err != nil {
		return fmt.Errorf("list memory files: %w", err)
	}

	type pending struct {
		path string
		text string
	}
	var queue []pending

	for path, content := range all {
		if !shouldReconcile(path) {
			continue
		}
		if idx.Has(path) {
			continue
		}
		text := strings.TrimSpace(content)
		if text == "" {
			continue
		}
		// Mirror the cap applied by tool-side reindex so reconciled
		// vectors are produced from the same prefix of content.
		if len(text) > 30000 {
			text = text[:30000]
		}
		queue = append(queue, pending{path: path, text: text})
	}

	if len(queue) == 0 {
		logger.Info("embeddings: index is up to date, nothing to reconcile")
		return nil
	}

	logger.Info("embeddings: reconcile pass embedding missing files",
		slog.Int("count", len(queue)),
		slog.Int("batch_size", batchSize))

	if batchSize <= 0 {
		batchSize = 100
	}

	embedded := 0
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

		// Each batch gets a fresh context derived from ctx so a
		// hung server can't stall reconcile indefinitely.
		batchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		vecs, err := embedder.Embed(batchCtx, texts)
		cancel()
		if err != nil {
			return fmt.Errorf("embed batch starting at %d: %w", i, err)
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("embed batch starting at %d: got %d vecs for %d inputs", i, len(vecs), len(batch))
		}
		for j, p := range batch {
			if err := idx.Upsert(p.path, vecs[j]); err != nil {
				logger.Warn("embeddings: reconcile upsert failed; continuing",
					slog.String("path", p.path),
					slog.String("err", err.Error()))
				continue
			}
			embedded++
		}

		// Bail out early on cancellation (graceful shutdown) so we
		// don't keep hammering the API for files the operator no
		// longer cares about.
		select {
		case <-ctx.Done():
			logger.Info("embeddings: reconcile canceled mid-pass",
				slog.Int("embedded", embedded),
				slog.Int("total", len(queue)))
			return ctx.Err()
		default:
		}
	}

	logger.Info("embeddings: reconcile complete",
		slog.Int("embedded", embedded),
		slog.Int("total", len(queue)))
	return nil
}

// shouldReconcile mirrors tools.shouldIndexPath. Duplicated here
// because main can't import tools internals; keep the two predicates
// in sync if either is updated.
func shouldReconcile(path string) bool {
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

// runVectorPersistence flushes the index to disk every
// vectorPersistInterval if it has been mutated since the last flush.
// Exits cleanly when ctx is canceled (shutdown).
func runVectorPersistence(ctx context.Context, idx *vector.Index, path string, logger *slog.Logger) {
	t := time.NewTicker(vectorPersistInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !idx.Dirty() {
				continue
			}
			if err := idx.Save(path); err != nil {
				logger.Warn("embeddings: periodic save failed",
					slog.String("err", err.Error()))
				continue
			}
			logger.Debug("embeddings: index flushed",
				slog.Int("vectors", idx.Len()),
				slog.String("path", path))
		}
	}
}

// flushVectorIndex performs a final synchronous flush at shutdown.
// Called from main.go via defer so the very latest mutations survive
// a clean exit.
func flushVectorIndex(idx *vector.Index, path string, logger *slog.Logger) {
	if !idx.Dirty() {
		return
	}
	if err := idx.Save(path); err != nil {
		logger.Warn("embeddings: shutdown save failed",
			slog.String("err", err.Error()))
		return
	}
	logger.Info("embeddings: index flushed at shutdown",
		slog.Int("vectors", idx.Len()),
		slog.String("path", path))
}
