package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// The shared helper in internal/tools is also used by the
	// rebuild_indexes tool with full-rebuild semantics.
	if _, err := tools.ReconcileVectorIndex(ctx, client, idx, memory, cfg.Embeddings.BatchSize, logger); err != nil {
		logger.Warn("embeddings: reconcile incomplete; continuing with partial index",
			slog.String("err", err.Error()))
	}

	return client, idx
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
