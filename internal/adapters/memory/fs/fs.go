// Package fs is the filesystem-backed memory adapter. Memory files are
// plain Markdown stored under a base directory, with a soft-delete trash
// subdirectory for restoration.
package fs

import (
	"bufio"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/search/bm25"
)

// trashDir is the name of the trash subdirectory inside the memory store.
const trashDir = ".trash"

// Store manages the file-based memory system.
type Store struct {
	baseDir  string
	trashDir string // absolute path to the trash directory
}

// New creates a new memory store, ensuring the base directory exists.
func New(baseDir string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("create memory directory: %w", err)
	}
	trash := filepath.Join(baseDir, trashDir)
	if err := os.MkdirAll(trash, 0755); err != nil {
		return nil, fmt.Errorf("create trash directory: %w", err)
	}
	return &Store{baseDir: baseDir, trashDir: trash}, nil
}

// IsTrashPath returns true if the given relative path refers to the trash directory
// or anything inside it.
func IsTrashPath(path string) bool {
	clean := strings.ToLower(filepath.Clean(path))
	return clean == trashDir || strings.HasPrefix(clean, trashDir+string(filepath.Separator))
}

// cleanPath normalizes a user-supplied path: cleans it, strips leading
// slashes, and lowercases it so all memory filenames are case-insensitive.
func cleanPath(path string) string {
	path = filepath.Clean(path)
	path = strings.TrimPrefix(path, "/")
	path = strings.ToLower(path)
	return path
}

// resolvePath safely resolves a memory path, preventing directory traversal.
// Ensures .md extension is present on file paths.
func (m *Store) resolvePath(path string) (string, error) {
	path = cleanPath(path)

	if path == "" {
		return m.baseDir, nil
	}

	// Ensure .md extension for files (not directories)
	if !strings.HasSuffix(path, "/") && !strings.HasSuffix(path, ".md") {
		path = path + ".md"
	}

	full := filepath.Join(m.baseDir, path)

	// Verify it's still within the base directory
	rel, err := filepath.Rel(m.baseDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes memory directory", path)
	}

	return full, nil
}

// resolveAnyPath safely resolves a path without forcing .md extension.
// Used for operations that work on both files and directories.
func (m *Store) resolveAnyPath(path string) (string, error) {
	path = cleanPath(path)

	if path == "" || path == "." {
		return m.baseDir, nil
	}

	full := filepath.Join(m.baseDir, path)

	// Verify it's still within the base directory
	rel, err := filepath.Rel(m.baseDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes memory directory", path)
	}

	return full, nil
}

// resolveExisting resolves a path to an existing file or directory.
// Tries the exact path first, then with .md extension for files.
func (m *Store) resolveExisting(path string) (string, error) {
	path = cleanPath(path)

	if path == "" || path == "." {
		return m.baseDir, nil
	}

	// Try without .md first (handles directories and exact file paths)
	full, err := m.resolveAnyPath(path)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(full); statErr == nil {
		return full, nil
	}

	// Try with .md extension
	if !strings.HasSuffix(path, ".md") {
		fullMD, err := m.resolveAnyPath(path + ".md")
		if err != nil {
			return "", err
		}
		if _, statErr := os.Stat(fullMD); statErr == nil {
			return fullMD, nil
		}
	}

	return "", fmt.Errorf("path %q does not exist", path)
}

// Read reads a memory file and returns its content.
func (m *Store) Read(path string) (string, error) {
	full, err := m.resolvePath(path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("memory file %q does not exist", path)
		}
		return "", fmt.Errorf("read memory: %w", err)
	}

	return string(data), nil
}

// ReadLines reads a memory file with optional offset and line limit.
// offset is 1-indexed (0 or 1 both start from the beginning).
// lines <= 0 means read all lines. Returns content with line numbers.
func (m *Store) ReadLines(path string, offset, lines int) (string, int, error) {
	full, err := m.resolvePath(path)
	if err != nil {
		return "", 0, err
	}

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("memory file %q does not exist", path)
		}
		return "", 0, fmt.Errorf("read memory: %w", err)
	}
	defer f.Close()

	if offset < 1 {
		offset = 1
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	collected := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if lines > 0 && collected >= lines {
			break
		}
		fmt.Fprintf(&sb, "%d: %s\n", lineNum, scanner.Text())
		collected++
	}

	if err := scanner.Err(); err != nil {
		return "", lineNum, fmt.Errorf("read memory: %w", err)
	}

	if collected == 0 {
		if lineNum == 0 {
			return "(empty file)", 0, nil
		}
		return fmt.Sprintf("(no lines in range: file has %d lines, requested offset %d)", lineNum, offset), lineNum, nil
	}

	return sb.String(), lineNum, nil
}

// Edit performs an exact string find-and-replace within a memory file.
// If replaceAll is false, the oldString must appear exactly once in the file
// or the operation fails. Returns the number of replacements made.
func (m *Store) Edit(path string, oldString, newString string, replaceAll bool) (int, error) {
	full, err := m.resolvePath(path)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("memory file %q does not exist", path)
		}
		return 0, fmt.Errorf("read memory: %w", err)
	}

	content := string(data)
	count := strings.Count(content, oldString)

	if count == 0 {
		return 0, fmt.Errorf("oldString not found in %q", path)
	}
	if count > 1 && !replaceAll {
		return 0, fmt.Errorf("found %d matches for oldString in %q; use replace_all to replace all occurrences, or provide more surrounding context to make the match unique", count, path)
	}

	var result string
	if replaceAll {
		result = strings.ReplaceAll(content, oldString, newString)
	} else {
		result = strings.Replace(content, oldString, newString, 1)
	}

	if err := os.WriteFile(full, []byte(result), 0644); err != nil {
		return 0, fmt.Errorf("write memory: %w", err)
	}

	return count, nil
}

// Append appends content to the end of a memory file.
// Creates the file (and parent directories) if it does not exist.
func (m *Store) Append(path string, content string) error {
	full, err := m.resolvePath(path)
	if err != nil {
		return err
	}

	// Create parent directories
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open for append: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("append: %w", err)
	}

	return nil
}

// Insert inserts content at a specific line number in a memory file.
// line is 1-indexed. Content is inserted before the specified line.
// If line exceeds the file length, content is appended at the end.
func (m *Store) Insert(path string, line int, content string) (int, error) {
	full, err := m.resolvePath(path)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("memory file %q does not exist", path)
		}
		return 0, fmt.Errorf("read memory: %w", err)
	}

	if line < 1 {
		line = 1
	}

	lines := strings.Split(string(data), "\n")

	// strings.Split produces a trailing empty element for files ending in \n.
	// Track whether we need to preserve it so the line count stays accurate.
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	if trailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	totalLines := len(lines)

	// If inserting past the end, append
	insertIdx := line - 1
	if insertIdx > totalLines {
		insertIdx = totalLines
	}

	// Split the content to insert into lines
	insertLines := strings.Split(content, "\n")

	// Build new content: before + inserted + after
	newLines := make([]string, 0, totalLines+len(insertLines))
	newLines = append(newLines, lines[:insertIdx]...)
	newLines = append(newLines, insertLines...)
	newLines = append(newLines, lines[insertIdx:]...)

	result := strings.Join(newLines, "\n")
	if trailingNewline && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	if err := os.WriteFile(full, []byte(result), 0644); err != nil {
		return 0, fmt.Errorf("write memory: %w", err)
	}

	return len(newLines), nil
}

// Write writes content to a memory file, creating directories as needed.
func (m *Store) Write(path string, content string) error {
	full, err := m.resolvePath(path)
	if err != nil {
		return err
	}

	// Create parent directories
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}

	return nil
}

// List lists files and directories under the given path.
func (m *Store) List(dir string) ([]MemoryEntry, error) {
	full, err := m.resolveAnyPath(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return []MemoryEntry{}, nil
		}
		return nil, fmt.Errorf("list memory: %w", err)
	}

	var result []MemoryEntry
	for _, e := range entries {
		// Hide the trash directory from normal listings
		if e.Name() == trashDir {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		entry := MemoryEntry{
			Name:    e.Name(),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		result = append(result, entry)
	}

	return result, nil
}

// MemoryEntry represents a file or directory in memory.
type MemoryEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Delete soft-deletes a file or directory by moving it to the .trash/ directory.
// The original directory structure is mirrored inside .trash/ so files can be
// restored to their original location. If a collision occurs in the trash
// (a file with the same path was previously trashed), a timestamp suffix is
// appended to avoid overwriting.
func (m *Store) Delete(path string) error {
	if path == "" || path == "/" || path == "." {
		return fmt.Errorf("cannot delete the root memory directory")
	}

	clean := cleanPath(path)
	if IsTrashPath(clean) {
		return fmt.Errorf("cannot delete from the trash directory; use empty_trash instead")
	}

	full, err := m.resolveExisting(path)
	if err != nil {
		return err
	}

	// Extra safety: never delete the base directory itself
	if full == m.baseDir {
		return fmt.Errorf("cannot delete the root memory directory")
	}

	// Determine the relative path from baseDir for mirroring in trash
	rel, err := filepath.Rel(m.baseDir, full)
	if err != nil {
		return fmt.Errorf("resolve relative path: %w", err)
	}

	trashDest := filepath.Join(m.trashDir, rel)

	// Handle collision: if the trash destination already exists, rename the
	// older copy with a timestamp suffix so the clean path always holds the
	// most recently deleted version (making restore intuitive).
	if existingInfo, err := os.Stat(trashDest); err == nil {
		ext := filepath.Ext(trashDest)
		base := strings.TrimSuffix(trashDest, ext)
		// Use the existing file's mod time so the suffix reflects when that
		// version was trashed, not the current wall clock.
		ts := existingInfo.ModTime().Format("20060102-150405")
		renamed := fmt.Sprintf("%s.%s%s", base, ts, ext)
		if err := os.Rename(trashDest, renamed); err != nil {
			return fmt.Errorf("rename existing trash file: %w", err)
		}
	}

	// Create parent directories in trash
	if err := os.MkdirAll(filepath.Dir(trashDest), 0755); err != nil {
		return fmt.Errorf("create trash directory: %w", err)
	}

	return os.Rename(full, trashDest)
}

// Restore moves a file or directory from the .trash/ directory back to its
// original location in the memory store. The path should be relative to the
// trash root (mirroring the original memory path). Tries the exact path first,
// then with .md extension appended, matching the resolution behavior of
// normal memory operations.
//
// If the trashed file has a timestamp suffix from a collision rename (e.g.
// "climate.20260427-143022.md"), the file is restored to the original path
// without the timestamp ("climate.md").
func (m *Store) Restore(trashPath string) (string, error) {
	if trashPath == "" || trashPath == "/" || trashPath == "." {
		return "", fmt.Errorf("path is required")
	}

	clean := cleanPath(trashPath)

	// Resolve the full path inside .trash/, trying exact then with .md
	trashFull, err := m.resolveTrashPath(clean)
	if err != nil {
		return "", err
	}

	// Recompute clean relative to trashDir (resolveTrashPath may have added .md)
	clean, err = filepath.Rel(m.trashDir, trashFull)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}

	// Strip any collision-timestamp suffix to recover the original memory path.
	// Pattern: "name.YYYYMMDD-HHMMSS.md" -> "name.md"
	restoreName := stripTimestampSuffix(clean)

	restoreDest := filepath.Join(m.baseDir, restoreName)

	// Don't overwrite existing files
	if _, err := os.Stat(restoreDest); err == nil {
		return "", fmt.Errorf("cannot restore: %q already exists in memory", restoreName)
	}

	// Create parent directories at the destination
	if err := os.MkdirAll(filepath.Dir(restoreDest), 0755); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}

	if err := os.Rename(trashFull, restoreDest); err != nil {
		return "", fmt.Errorf("restore failed: %w", err)
	}

	// Clean up empty parent directories left in .trash/
	m.cleanEmptyTrashDirs(filepath.Dir(trashFull))

	return restoreName, nil
}

// resolveTrashPath resolves a path inside the trash directory. Tries the exact
// path first, then with .md extension appended.
func (m *Store) resolveTrashPath(relPath string) (string, error) {
	relPath = cleanPath(relPath)
	full := filepath.Join(m.trashDir, relPath)

	// Verify the path stays within the trash directory
	rel, err := filepath.Rel(m.trashDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes trash directory", relPath)
	}

	if _, err := os.Stat(full); err == nil {
		return full, nil
	}

	// Try with .md extension
	if !strings.HasSuffix(relPath, ".md") {
		fullMD := filepath.Join(m.trashDir, relPath+".md")
		if _, err := os.Stat(fullMD); err == nil {
			return fullMD, nil
		}
	}

	return "", fmt.Errorf("path %q does not exist in trash", relPath)
}

// timestampSuffixRe matches the collision-rename timestamp pattern inserted
// before the file extension: ".YYYYMMDD-HHMMSS"
var timestampSuffixRe = regexp.MustCompile(`\.\d{8}-\d{6}(\.[^.]+)$`)

// stripTimestampSuffix removes a collision-rename timestamp suffix from a path.
// "research/climate.20260427-143022.md" -> "research/climate.md"
// Paths without the suffix are returned unchanged.
func stripTimestampSuffix(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if m := timestampSuffixRe.FindStringSubmatch(base); m != nil {
		// m[0] is the full match e.g. ".20260427-143022.md"
		// m[1] is the extension e.g. ".md"
		base = strings.TrimSuffix(base, m[0]) + m[1]
	}
	if dir == "." {
		return base
	}
	return filepath.Join(dir, base)
}

// ListTrash lists files and directories in the trash.
func (m *Store) ListTrash(dir string) ([]TrashEntry, error) {
	dir = cleanPath(dir)
	full := m.trashDir
	if dir != "" && dir != "." {
		full = filepath.Join(m.trashDir, dir)
	}

	// Verify within trash
	rel, err := filepath.Rel(m.trashDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("path %q escapes trash directory", dir)
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return []TrashEntry{}, nil
		}
		return nil, fmt.Errorf("list trash: %w", err)
	}

	var result []TrashEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Build the relative path from trash root for display
		entryRel := e.Name()
		if dir != "" && dir != "." {
			entryRel = filepath.Join(dir, e.Name())
		}
		result = append(result, TrashEntry{
			Name:         e.Name(),
			OriginalPath: entryRel,
			IsDir:        e.IsDir(),
			Size:         info.Size(),
			ModTime:      info.ModTime(),
		})
	}

	return result, nil
}

// TrashEntry represents a file or directory in the trash.
type TrashEntry struct {
	Name         string    `json:"name"`
	OriginalPath string    `json:"original_path"`
	IsDir        bool      `json:"is_dir"`
	Size         int64     `json:"size"`
	ModTime      time.Time `json:"mod_time"`
}

// EmptyTrash permanently removes all files and directories from the trash.
func (m *Store) EmptyTrash() error {
	if err := os.RemoveAll(m.trashDir); err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}
	// Recreate the empty trash directory
	return os.MkdirAll(m.trashDir, 0755)
}

// cleanEmptyTrashDirs walks up from dir, removing empty directories until
// it reaches the trash root.
func (m *Store) cleanEmptyTrashDirs(dir string) {
	for dir != m.trashDir {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}

// Move renames/moves a file or directory within the memory store.
func (m *Store) Move(src, dst string) error {
	if src == "" || src == "/" || src == "." {
		return fmt.Errorf("cannot move the root memory directory")
	}

	// Prevent moving to/from the trash directory -- use delete/restore instead
	if IsTrashPath(cleanPath(src)) {
		return fmt.Errorf("cannot move from trash; use memory_restore instead")
	}
	if IsTrashPath(cleanPath(dst)) {
		return fmt.Errorf("cannot move to trash; use memory_delete instead")
	}

	// Resolve source to an existing path
	srcFull, err := m.resolveExisting(src)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	if srcFull == m.baseDir {
		return fmt.Errorf("cannot move the root memory directory")
	}

	srcInfo, err := os.Stat(srcFull)
	if err != nil {
		return fmt.Errorf("source %q does not exist", src)
	}

	// Resolve destination based on source type
	var dstFull string
	if srcInfo.IsDir() {
		dstFull, err = m.resolveAnyPath(dst)
	} else {
		dstFull, err = m.resolvePath(dst)
	}
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	// Create parent directories for destination
	if err := os.MkdirAll(filepath.Dir(dstFull), 0755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	return os.Rename(srcFull, dstFull)
}

// Stat returns metadata about a file or directory without reading its content.
func (m *Store) Stat(path string) (*MemoryEntry, error) {
	full, err := m.resolveExisting(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("path %q does not exist", path)
	}

	return &MemoryEntry{
		Name:    filepath.Base(full),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

// DirSize returns the total size and file count for a directory (recursive).
func (m *Store) DirSize(dirPath string) (totalSize int64, fileCount int, err error) {
	full, err := m.resolveAnyPath(dirPath)
	if err != nil {
		return 0, 0, err
	}

	err = filepath.WalkDir(full, func(_ string, d stdfs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		// Skip the trash directory entirely
		if d.IsDir() && d.Name() == trashDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		totalSize += info.Size()
		fileCount++
		return nil
	})
	return totalSize, fileCount, err
}

// GrepMatch represents a single line matching a search pattern.
type GrepMatch struct {
	LineNum int    `json:"line_num"`
	Line    string `json:"line"`
}

// Grep searches for a regex pattern within a specific memory file.
// Returns matching lines with their line numbers.
func (m *Store) Grep(path string, pattern string) ([]GrepMatch, error) {
	content, err := m.Read(path)
	if err != nil {
		return nil, err
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	var matches []GrepMatch
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if re.MatchString(line) {
			matches = append(matches, GrepMatch{LineNum: i + 1, Line: line})
		}
	}
	return matches, nil
}

// AllFiles returns all .md files in the memory directory with their content.
// Used for building the search index.
func (m *Store) AllFiles() (map[string]string, error) {
	files := make(map[string]string)

	err := filepath.WalkDir(m.baseDir, func(path string, d stdfs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		// Skip the trash directory entirely
		if d.IsDir() && d.Name() == trashDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		rel, err := filepath.Rel(m.baseDir, path)
		if err != nil {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		files[rel] = string(data)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walk memory directory: %w", err)
	}

	return files, nil
}

// RecentFiles returns the N most recently modified .md files.
func (m *Store) RecentFiles(n int) ([]bm25.Result, error) {
	type fileInfo struct {
		path    string
		modTime time.Time
	}

	var allFiles []fileInfo

	err := filepath.WalkDir(m.baseDir, func(path string, d stdfs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Skip the trash directory entirely
		if d.IsDir() && d.Name() == trashDir {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(m.baseDir, path)
		if err != nil {
			return nil
		}
		allFiles = append(allFiles, fileInfo{path: rel, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort by modification time, most recent first
	for i := 0; i < len(allFiles); i++ {
		for j := i + 1; j < len(allFiles); j++ {
			if allFiles[j].modTime.After(allFiles[i].modTime) {
				allFiles[i], allFiles[j] = allFiles[j], allFiles[i]
			}
		}
	}

	if len(allFiles) > n {
		allFiles = allFiles[:n]
	}

	var results []bm25.Result
	for _, f := range allFiles {
		content, err := m.Read(f.path)
		if err != nil {
			continue
		}
		results = append(results, bm25.Result{
			Path:    f.path,
			Content: content,
			Score:   0,
		})
	}

	return results, nil
}
