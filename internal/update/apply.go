package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// apply is the apply-update pipeline:
//  1. Download the matching tarball asset to a temp file in the binary's
//     directory (so the eventual rename is on the same filesystem and
//     therefore atomic).
//  2. Download SHA256SUMS, verify the tarball's hash matches its line.
//  3. Extract the inner faultline binary to <binaryPath>.new.
//  4. chmod +x the .new file.
//  5. Rename current binary to .previous (one-deep rollback slot).
//  6. Rename .new to current.
//
// On any failure after step 5, attempts to restore from .previous so a
// half-applied update doesn't leave the deploy broken.
func (u *Updater) apply(ctx context.Context, rel *Release) (*Result, error) {
	current := u.cfg.BinaryPath
	if current == "" {
		return nil, errors.New("binary path not configured")
	}

	tagBare := strings.TrimPrefix(rel.TagName, "v")
	assetName := AssetName(tagBare)

	asset := rel.FindAsset(assetName)
	if asset == nil {
		return nil, fmt.Errorf("release %s has no asset matching %q", rel.TagName, assetName)
	}
	checksums := rel.FindAsset(ChecksumsName)
	if checksums == nil {
		return nil, fmt.Errorf("release %s has no %s asset", rel.TagName, ChecksumsName)
	}

	binDir := filepath.Dir(current)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure bin dir: %w", err)
	}

	tarPath := current + ".tar.tmp"
	newPath := current + ".new"
	prevPath := current + ".previous"

	// Always clean up the tarball; .new only on failure.
	defer os.Remove(tarPath)

	// 1. Download tarball.
	if err := downloadTo(ctx, u.gh, asset.BrowserDownloadURL, tarPath); err != nil {
		return nil, err
	}

	// 2. Download checksums and verify.
	wantSum, err := fetchExpectedSum(ctx, u.gh, checksums.BrowserDownloadURL, assetName)
	if err != nil {
		return nil, err
	}
	gotSum, err := fileSha256(tarPath)
	if err != nil {
		return nil, fmt.Errorf("hash tarball: %w", err)
	}
	if gotSum != wantSum {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s",
			assetName, gotSum, wantSum)
	}

	// 3. Extract the faultline binary into <current>.new.
	if err := extractBinary(tarPath, newPath); err != nil {
		_ = os.Remove(newPath)
		return nil, err
	}

	// 4. chmod +x. Fail-safe: clean up .new if we can't make it
	// executable.
	if err := os.Chmod(newPath, 0o755); err != nil {
		_ = os.Remove(newPath)
		return nil, fmt.Errorf("chmod new binary: %w", err)
	}

	// 5. Move current -> .previous (one-deep rollback slot). os.Rename
	// overwrites destination on Linux, so any stale .previous from a
	// prior update is replaced atomically.
	if err := os.Rename(current, prevPath); err != nil {
		_ = os.Remove(newPath)
		return nil, fmt.Errorf("rotate current to previous: %w", err)
	}

	// 6. Move .new -> current. On failure, rollback by restoring
	// .previous to current so the deploy keeps running on the old
	// binary.
	if err := os.Rename(newPath, current); err != nil {
		if rbErr := os.Rename(prevPath, current); rbErr != nil {
			return nil, errors.Join(
				fmt.Errorf("install new binary failed: %w", err),
				fmt.Errorf("rollback failed: %w; deploy is broken", rbErr),
			)
		}
		return nil, fmt.Errorf("install new binary: %w", err)
	}

	return &Result{
		FromVersion: u.currentVersion(),
		ToVersion:   rel.TagName,
		BinaryPath:  current,
	}, nil
}

// downloadTo streams a URL to a file. The file is created with 0644
// permissions; chmod happens later on the extracted binary, not the
// tarball.
func downloadTo(ctx context.Context, gh *githubClient, url, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()

	if err := gh.download(ctx, url, f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	return nil
}

// fileSha256 returns the lowercase hex SHA-256 of the file at path.
func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchExpectedSum downloads SHA256SUMS and finds the entry matching
// assetName. SHA256SUMS lines look like:
//
//	abc123...  faultline_1.0.0_linux_x86_64.tar.gz
//
// (two-space separator). Returns the expected hex digest.
func fetchExpectedSum(ctx context.Context, gh *githubClient, url, assetName string) (string, error) {
	var buf strings.Builder
	if err := gh.download(ctx, url, &buf); err != nil {
		return "", err
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Last field is the filename, first field is the digest.
		// goreleaser writes "<digest>  <filename>" but tolerate
		// any whitespace between.
		if fields[len(fields)-1] == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s not listed in SHA256SUMS", assetName)
}

// extractBinary reads tarPath (a .tar.gz archive) and writes the
// faultline binary inside it to outPath. Other files in the archive
// (LICENSE, README.md, AGENTS.md, config.example.toml) are ignored --
// the deployed binary is the only thing the updater swaps.
func extractBinary(tarPath, outPath string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		// goreleaser tarballs are flat (no nested directories), but be
		// safe and only consider the basename. We accept exactly
		// "faultline" as the binary entry.
		if filepath.Base(hdr.Name) != "faultline" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create new binary: %w", err)
		}
		// Cap copy at the header's size to avoid a malicious archive
		// claiming a tiny file but streaming forever.
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
			out.Close()
			_ = os.Remove(outPath)
			return fmt.Errorf("extract binary: %w", err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close new binary: %w", err)
		}
		return nil
	}
	return errors.New("tarball did not contain a faultline binary")
}
