package vector

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// File format ("FVEC v1"):
//
//   magic[4]      = "FVEC"
//   version[4]    = uint32 LE                  (= fileVersion)
//   dim[4]        = uint32 LE
//   count[4]      = uint32 LE
//   model_len[2]  = uint16 LE
//   model[N]      = UTF-8 bytes (no NUL terminator)
//   records[count]:
//     path_len[2] = uint16 LE
//     path[N]     = UTF-8 bytes
//     vector      = dim × float32 LE (raw IEEE-754 bits)
//
// Total per record: 2 + len(path) + dim*4 bytes. For a 1536-dim
// vector with a 60-char path that's ~6.2 KB; 10k vectors ≈ 62 MB on
// disk which is fine.
//
// Atomic write: we write to "<path>.tmp" and rename over <path>.
// Corrupt files are not auto-deleted by the package itself; callers
// (e.g. main.go) should rename them aside with a ".bad-<unix>" suffix
// and continue with a fresh index, mirroring the pattern in
// internal/adapters/state/jsonfile.

const (
	fileMagic   = "FVEC"
	fileVersion = uint32(1)

	// maxModelLen is the largest model identifier we'll accept on
	// load. Anything larger almost certainly indicates a corrupt or
	// foreign file.
	maxModelLen = 1024

	// maxPathLen is the largest path we'll accept per record.
	maxPathLen = 4096

	// maxDim is a sanity bound on the per-vector dimensionality. The
	// largest practical OpenAI embedding today is 3072 dim
	// (text-embedding-3-large); 8192 leaves headroom for future
	// models without permitting absurd values.
	maxDim = 8192
)

// ErrModelMismatch is returned by Load when the on-disk index was
// produced by a different embedding model or dimensionality than the
// in-memory Index expects. The caller should Reset the index and
// re-embed all sources.
var ErrModelMismatch = errors.New("vector: on-disk index has a different model or dim")

// ErrCorrupt is returned by Load when the file does not start with the
// FVEC magic, has a future version, or has internal inconsistencies.
// The caller should rename the file aside and continue with an empty
// index.
var ErrCorrupt = errors.New("vector: index file is corrupt or unrecognized")

// Save writes the index to path. The write is atomic: data goes to
// "<path>.tmp" and is renamed over <path> on success.
//
// On success the dirty flag is cleared.
func (idx *Index) Save(path string) error {
	if path == "" {
		return errors.New("vector: save path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vector: mkdir parent: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("vector: create temp: %w", err)
	}
	// On any error after this point, do best-effort cleanup of the
	// temp file.
	cleanup := func() { _ = os.Remove(tmp) }

	bw := bufio.NewWriter(f)

	idx.mu.RLock()
	dim := idx.dim
	model := idx.model
	// Snapshot keys so we can write in deterministic order. Sorted
	// output makes byte-level comparison meaningful in tests and
	// keeps diffs reviewable if anyone ever inspects the file.
	paths := make([]string, 0, len(idx.vectors))
	for p := range idx.vectors {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	count := uint32(len(paths))
	idx.mu.RUnlock()

	if dim < 0 || dim > maxDim {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: refusing to save with dim=%d", dim)
	}
	if len(model) > maxModelLen {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: model identifier too long (%d bytes)", len(model))
	}

	// Header.
	if _, err := bw.WriteString(fileMagic); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: write magic: %w", err)
	}
	if err := writeU32(bw, fileVersion); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := writeU32(bw, uint32(dim)); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := writeU32(bw, count); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := writeU16(bw, uint16(len(model))); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if _, err := bw.WriteString(model); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: write model: %w", err)
	}

	// Records. Re-take the read lock for the duration of the dump.
	// This blocks Upsert/Remove for the duration of a Save, which is
	// acceptable: typical Save is well under 100ms even for tens of
	// thousands of vectors, and Save is only called from the
	// persistence loop or shutdown, not the hot path.
	idx.mu.RLock()
	for _, p := range paths {
		v, ok := idx.vectors[p]
		if !ok {
			// Concurrent Remove between snapshot and now. Skip it
			// and decrement the count we wrote to the header.
			continue
		}
		if len(v) != dim {
			idx.mu.RUnlock()
			_ = f.Close()
			cleanup()
			return fmt.Errorf("vector: internal error: vector for %q has dim %d, want %d", p, len(v), dim)
		}
		if len(p) > maxPathLen {
			idx.mu.RUnlock()
			_ = f.Close()
			cleanup()
			return fmt.Errorf("vector: path too long: %d bytes", len(p))
		}
		if err := writeU16(bw, uint16(len(p))); err != nil {
			idx.mu.RUnlock()
			_ = f.Close()
			cleanup()
			return err
		}
		if _, err := bw.WriteString(p); err != nil {
			idx.mu.RUnlock()
			_ = f.Close()
			cleanup()
			return fmt.Errorf("vector: write path: %w", err)
		}
		if err := writeFloat32s(bw, v); err != nil {
			idx.mu.RUnlock()
			_ = f.Close()
			cleanup()
			return err
		}
	}
	idx.mu.RUnlock()

	if err := bw.Flush(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: flush: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("vector: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("vector: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("vector: rename into place: %w", err)
	}

	idx.dirty.Store(false)
	return nil
}

// Load replaces the index contents with the file at path. If the file
// does not exist, Load returns nil and leaves the index untouched
// (treat as "no prior state").
//
// If the on-disk model/dim does not match the index's configured
// model/dim, Load returns ErrModelMismatch; the caller should call
// Reset and re-embed.
//
// If the file is unreadable or fails the format checks, Load returns
// an error wrapping ErrCorrupt. The caller is responsible for renaming
// the bad file aside before continuing.
func (idx *Index) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("vector: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	br := bufio.NewReader(f)

	// Header. corruptf wraps ErrCorrupt together with any underlying
	// I/O error so callers can errors.Is(err, ErrCorrupt) regardless of
	// where the failure happened. errors.Join keeps both in the chain
	// without smuggling errors through a non-%w verb.
	corruptf := func(format string, args ...any) error {
		return errors.Join(ErrCorrupt, fmt.Errorf(format, args...))
	}

	magic := make([]byte, 4)
	if _, err := io.ReadFull(br, magic); err != nil {
		return corruptf("read magic: %w", err)
	}
	if string(magic) != fileMagic {
		return corruptf("magic %q != %q", string(magic), fileMagic)
	}
	version, err := readU32(br)
	if err != nil {
		return corruptf("read version: %w", err)
	}
	if version != fileVersion {
		return corruptf("version %d unsupported (want %d)", version, fileVersion)
	}
	dim32, err := readU32(br)
	if err != nil {
		return corruptf("read dim: %w", err)
	}
	if dim32 == 0 || dim32 > maxDim {
		return corruptf("implausible dim %d", dim32)
	}
	dim := int(dim32)

	count, err := readU32(br)
	if err != nil {
		return corruptf("read count: %w", err)
	}

	modelLen, err := readU16(br)
	if err != nil {
		return corruptf("read model_len: %w", err)
	}
	if int(modelLen) > maxModelLen {
		return corruptf("model_len %d exceeds limit", modelLen)
	}
	modelBytes := make([]byte, modelLen)
	if _, err := io.ReadFull(br, modelBytes); err != nil {
		return corruptf("read model: %w", err)
	}
	model := string(modelBytes)

	// Mismatch check before allocating any record memory.
	if dim != idx.dim || model != idx.model {
		return fmt.Errorf("%w: file has dim=%d model=%q, want dim=%d model=%q",
			ErrModelMismatch, dim, model, idx.dim, idx.model)
	}

	// Records.
	loaded := make(map[string][]float32, count)
	for i := uint32(0); i < count; i++ {
		pathLen, err := readU16(br)
		if err != nil {
			return corruptf("record %d read path_len: %w", i, err)
		}
		if int(pathLen) > maxPathLen || pathLen == 0 {
			return corruptf("record %d implausible path_len %d", i, pathLen)
		}
		pathBytes := make([]byte, pathLen)
		if _, err := io.ReadFull(br, pathBytes); err != nil {
			return corruptf("record %d read path: %w", i, err)
		}
		vec := make([]float32, dim)
		if err := readFloat32s(br, vec); err != nil {
			return corruptf("record %d read vector: %w", i, err)
		}
		loaded[string(pathBytes)] = vec
	}

	// Trailing data is allowed (forward compatibility), but we
	// don't try to interpret it.
	idx.mu.Lock()
	idx.vectors = loaded
	idx.mu.Unlock()
	idx.dirty.Store(false)
	return nil
}

// --- low-level helpers ----------------------------------------------------

func writeU16(w io.Writer, v uint16) error {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	_, err := w.Write(buf[:])
	if err != nil {
		return fmt.Errorf("vector: write u16: %w", err)
	}
	return nil
}

func writeU32(w io.Writer, v uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	_, err := w.Write(buf[:])
	if err != nil {
		return fmt.Errorf("vector: write u32: %w", err)
	}
	return nil
}

func writeFloat32s(w io.Writer, vs []float32) error {
	buf := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	_, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("vector: write float32s: %w", err)
	}
	return nil
}

func readU16(r io.Reader) (uint16, error) {
	var buf [2]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[:]), nil
}

func readU32(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func readFloat32s(r io.Reader, dst []float32) error {
	buf := make([]byte, 4*len(dst))
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return nil
}
