package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestMemory returns a MemoryStore rooted in a fresh temp directory.
func newTestMemory(t *testing.T) *MemoryStore {
	t.Helper()
	m, err := NewMemoryStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	return m
}

func TestCleanPath(t *testing.T) {
	tests := map[string]string{
		"Foo":            "foo",
		"/leading/slash": "leading/slash",
		"a/../b":         "b",
		"./relative":     "relative",
		"NESTED/PATH.md": "nested/path.md",
		"":               ".",
	}
	for in, want := range tests {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTrashPath(t *testing.T) {
	if !isTrashPath(".trash") {
		t.Error(".trash should be trash path")
	}
	if !isTrashPath(".trash/foo.md") {
		t.Error(".trash/foo.md should be trash path")
	}
	if !isTrashPath(".TRASH/Foo") {
		t.Error("case-insensitive match expected for .TRASH/Foo")
	}
	if isTrashPath("notes.md") {
		t.Error("notes.md should not be trash path")
	}
	if isTrashPath("trash/foo.md") {
		t.Error("trash/foo.md (no leading dot) should not be trash path")
	}
}

func TestStripTimestampSuffix(t *testing.T) {
	tests := map[string]string{
		"climate.20260427-143022.md":            "climate.md",
		"research/notes.20260101-000000.md":     "research/notes.md",
		"plain.md":                              "plain.md",
		"no-extension.20260101-000000":          "no-extension.20260101-000000",
		"deeply/nested/path.20260427-143022.md": "deeply/nested/path.md",
	}
	for in, want := range tests {
		if got := stripTimestampSuffix(in); got != want {
			t.Errorf("stripTimestampSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMemoryStore_PathTraversalRejected(t *testing.T) {
	m := newTestMemory(t)

	// All of these should fail to escape the base directory.
	bad := []string{
		"../escape",
		"../../etc/passwd",
		"foo/../../escape",
		"/absolute/escape", // gets stripped of leading slash but should still resolve safely
	}
	for _, p := range bad {
		// Write should refuse to escape
		err := m.Write(p, "should not exist")
		if err == nil {
			// Confirm the file landed inside baseDir if it was created
			full := filepath.Join(m.baseDir, cleanPath(p))
			if !strings.HasPrefix(full, m.baseDir) {
				t.Errorf("Write(%q) escaped baseDir to %s", p, full)
			}
		}
		// Read of a bad path should likewise stay inside or fail
		if _, err := m.Read(p); err == nil {
			// If read succeeded, verify it didn't read outside the tree
			full, resolveErr := m.resolvePath(p)
			if resolveErr == nil && !strings.HasPrefix(full, m.baseDir) {
				t.Errorf("Read(%q) resolved outside baseDir to %s", p, full)
			}
		}
	}
}

func TestMemoryStore_WriteThenRead(t *testing.T) {
	m := newTestMemory(t)

	if err := m.Write("hello", "world"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := m.Read("hello")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "world" {
		t.Errorf("Read = %q, want %q", got, "world")
	}

	// .md extension is auto-appended; reading either form works
	got2, err := m.Read("hello.md")
	if err != nil {
		t.Fatalf("Read with .md: %v", err)
	}
	if got2 != "world" {
		t.Errorf("Read with .md = %q, want %q", got2, "world")
	}

	// Case insensitivity
	got3, err := m.Read("HELLO")
	if err != nil {
		t.Fatalf("Read uppercase: %v", err)
	}
	if got3 != "world" {
		t.Errorf("Read uppercase = %q, want %q", got3, "world")
	}
}

func TestMemoryStore_ReadMissingFile(t *testing.T) {
	m := newTestMemory(t)
	_, err := m.Read("nope")
	if err == nil {
		t.Error("expected error reading missing file")
	}
}

func TestMemoryStore_WriteCreatesParentDirs(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("a/b/c/note", "deep"); err != nil {
		t.Fatalf("Write nested: %v", err)
	}
	got, err := m.Read("a/b/c/note")
	if err != nil {
		t.Fatalf("Read nested: %v", err)
	}
	if got != "deep" {
		t.Errorf("got %q, want %q", got, "deep")
	}
}

func TestMemoryStore_ReadLines(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("multi", "line1\nline2\nline3\nline4\nline5"); err != nil {
		t.Fatal(err)
	}

	t.Run("full read", func(t *testing.T) {
		got, total, err := m.ReadLines("multi", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
		want := "1: line1\n2: line2\n3: line3\n4: line4\n5: line5\n"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("offset and limit", func(t *testing.T) {
		got, _, err := m.ReadLines("multi", 2, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := "2: line2\n3: line3\n"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("offset past end", func(t *testing.T) {
		got, _, err := m.ReadLines("multi", 99, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "no lines in range") {
			t.Errorf("expected out-of-range message, got %q", got)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		if err := m.Write("empty", ""); err != nil {
			t.Fatal(err)
		}
		got, _, err := m.ReadLines("empty", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got != "(empty file)" {
			t.Errorf("got %q, want '(empty file)'", got)
		}
	})
}

func TestMemoryStore_Edit(t *testing.T) {
	m := newTestMemory(t)

	if err := m.Write("note", "hello world"); err != nil {
		t.Fatal(err)
	}

	t.Run("single replace", func(t *testing.T) {
		count, err := m.Edit("note", "world", "earth", false)
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		got, _ := m.Read("note")
		if got != "hello earth" {
			t.Errorf("got %q, want %q", got, "hello earth")
		}
	})

	t.Run("missing oldString errors", func(t *testing.T) {
		_, err := m.Edit("note", "absent", "x", false)
		if err == nil {
			t.Error("expected error for missing oldString")
		}
	})

	t.Run("ambiguous match without replace_all errors", func(t *testing.T) {
		if err := m.Write("dup", "foo foo foo"); err != nil {
			t.Fatal(err)
		}
		_, err := m.Edit("dup", "foo", "bar", false)
		if err == nil {
			t.Error("expected error for multiple matches without replace_all")
		}
	})

	t.Run("replace_all replaces every occurrence", func(t *testing.T) {
		if err := m.Write("dup2", "foo foo foo"); err != nil {
			t.Fatal(err)
		}
		count, err := m.Edit("dup2", "foo", "bar", true)
		if err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Errorf("count = %d, want 3", count)
		}
		got, _ := m.Read("dup2")
		if got != "bar bar bar" {
			t.Errorf("got %q, want %q", got, "bar bar bar")
		}
	})
}

func TestMemoryStore_Append(t *testing.T) {
	m := newTestMemory(t)

	if err := m.Append("log", "first"); err != nil {
		t.Fatalf("Append (creates): %v", err)
	}
	if err := m.Append("log", "\nsecond"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := m.Read("log")
	if got != "first\nsecond" {
		t.Errorf("got %q, want %q", got, "first\nsecond")
	}
}

func TestMemoryStore_Insert(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}

	t.Run("insert at start", func(t *testing.T) {
		_, err := m.Insert("note", 1, "ZERO")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := m.Read("note")
		if got != "ZERO\na\nb\nc\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("insert past end appends", func(t *testing.T) {
		if err := m.Write("note2", "x\ny\n"); err != nil {
			t.Fatal(err)
		}
		_, err := m.Insert("note2", 99, "END")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := m.Read("note2")
		if got != "x\ny\nEND\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("trailing newline preserved", func(t *testing.T) {
		if err := m.Write("note3", "a\nb\n"); err != nil {
			t.Fatal(err)
		}
		_, err := m.Insert("note3", 2, "MIDDLE")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := m.Read("note3")
		if got != "a\nMIDDLE\nb\n" {
			t.Errorf("got %q, want %q", got, "a\nMIDDLE\nb\n")
		}
	})
}

func TestMemoryStore_DeleteAndRestore(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("project/notes", "important"); err != nil {
		t.Fatal(err)
	}

	if err := m.Delete("project/notes"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// File should be gone from main tree
	if _, err := m.Read("project/notes"); err == nil {
		t.Error("expected Read to fail after Delete")
	}

	// File should appear in trash listing
	entries, err := m.ListTrash("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one entry in trash")
	}

	// Restore by the trashed path mirroring the original location
	restored, err := m.Restore("project/notes")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored != "project/notes.md" {
		t.Errorf("restored path = %q, want %q", restored, "project/notes.md")
	}

	got, err := m.Read("project/notes")
	if err != nil {
		t.Fatalf("Read after Restore: %v", err)
	}
	if got != "important" {
		t.Errorf("got %q, want %q", got, "important")
	}
}

func TestMemoryStore_DeleteCollision(t *testing.T) {
	// When the same path is deleted twice, the older trash entry is renamed
	// with a timestamp suffix so the canonical trash location holds the
	// most recently deleted version.
	m := newTestMemory(t)

	if err := m.Write("note", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("note"); err != nil {
		t.Fatal(err)
	}

	if err := m.Write("note", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("note"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	// Both versions should now exist in trash; the canonical one is v2
	trashCanonical := filepath.Join(m.trashDir, "note.md")
	data, err := os.ReadFile(trashCanonical)
	if err != nil {
		t.Fatalf("read canonical trash: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("canonical trash content = %q, want v2", string(data))
	}

	// And a timestamped sibling should hold v1
	matches, _ := filepath.Glob(filepath.Join(m.trashDir, "note.*-*.md"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 timestamped trash sibling, got %d (%v)", len(matches), matches)
	}
	old, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "v1" {
		t.Errorf("timestamped trash content = %q, want v1", string(old))
	}
}

func TestMemoryStore_DeleteRoot(t *testing.T) {
	m := newTestMemory(t)
	for _, p := range []string{"", "/", "."} {
		if err := m.Delete(p); err == nil {
			t.Errorf("Delete(%q) should fail (root protection)", p)
		}
	}
}

func TestMemoryStore_DeleteFromTrashRejected(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "x"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("note"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(".trash/note"); err == nil {
		t.Error("Delete(.trash/...) should be rejected")
	}
}

func TestMemoryStore_RestoreCollidesWithExisting(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "original"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("note"); err != nil {
		t.Fatal(err)
	}
	// Recreate with new content - restore should refuse to overwrite
	if err := m.Write("note", "new"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Restore("note")
	if err == nil {
		t.Error("Restore should fail when destination already exists")
	}
}

func TestMemoryStore_EmptyTrash(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "x"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("note"); err != nil {
		t.Fatal(err)
	}

	if err := m.EmptyTrash(); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}

	entries, err := m.ListTrash("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty trash, got %d entries", len(entries))
	}

	// Trash dir should still exist for subsequent deletes
	if _, err := os.Stat(m.trashDir); err != nil {
		t.Errorf("trash dir missing after EmptyTrash: %v", err)
	}
}

func TestMemoryStore_Move(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("from", "content"); err != nil {
		t.Fatal(err)
	}

	if err := m.Move("from", "to"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	if _, err := m.Read("from"); err == nil {
		t.Error("source should no longer exist")
	}
	got, err := m.Read("to")
	if err != nil {
		t.Fatalf("Read destination: %v", err)
	}
	if got != "content" {
		t.Errorf("got %q, want %q", got, "content")
	}
}

func TestMemoryStore_MoveRestrictions(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "x"); err != nil {
		t.Fatal(err)
	}

	if err := m.Move("", "x"); err == nil {
		t.Error("Move from empty source should fail")
	}
	if err := m.Move(".trash/note", "x"); err == nil {
		t.Error("Move from trash should fail")
	}
	if err := m.Move("note", ".trash/x"); err == nil {
		t.Error("Move into trash should fail")
	}
}

func TestMemoryStore_List(t *testing.T) {
	m := newTestMemory(t)
	for _, p := range []string{"a", "b", "sub/c"} {
		if err := m.Write(p, "x"); err != nil {
			t.Fatal(err)
		}
	}

	root, err := m.List("")
	if err != nil {
		t.Fatal(err)
	}
	// Expect a.md, b.md, sub/ - trash directory should be hidden
	names := map[string]bool{}
	for _, e := range root {
		names[e.Name] = true
		if e.Name == trashDir {
			t.Error("List should hide .trash directory")
		}
	}
	if !names["a.md"] || !names["b.md"] || !names["sub"] {
		t.Errorf("List root missing expected entries: %v", names)
	}
}

func TestMemoryStore_Grep(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "apple\nbanana\ncherry apple\nplum"); err != nil {
		t.Fatal(err)
	}

	matches, err := m.Grep("note", `apple`)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	if matches[0].LineNum != 1 || matches[1].LineNum != 3 {
		t.Errorf("line numbers wrong: got %d and %d, want 1 and 3", matches[0].LineNum, matches[1].LineNum)
	}

	if _, err := m.Grep("note", `[`); err == nil {
		t.Error("invalid regex should error")
	}
}

func TestMemoryStore_AllFiles(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("a", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := m.Write("sub/b", "beta"); err != nil {
		t.Fatal(err)
	}

	// Trashed file should be excluded
	if err := m.Write("trashed", "gone"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("trashed"); err != nil {
		t.Fatal(err)
	}

	// Non-md file should be excluded too
	if err := os.WriteFile(filepath.Join(m.baseDir, "ignore.txt"), []byte("no"), 0644); err != nil {
		t.Fatal(err)
	}

	files, err := m.AllFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("AllFiles returned %d files, want 2: %v", len(files), files)
	}
	if files["a.md"] != "alpha" {
		t.Errorf("AllFiles[a.md] = %q, want alpha", files["a.md"])
	}
	subKey := filepath.Join("sub", "b.md")
	if files[subKey] != "beta" {
		t.Errorf("AllFiles[%s] = %q, want beta", subKey, files[subKey])
	}
}

func TestMemoryStore_RecentFiles(t *testing.T) {
	m := newTestMemory(t)

	// Write files with controlled mod times
	for _, p := range []string{"old", "mid", "new"} {
		if err := m.Write(p, "x"); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	mustChtime := func(rel string, mod time.Time) {
		full := filepath.Join(m.baseDir, rel+".md")
		if err := os.Chtimes(full, mod, mod); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}
	mustChtime("old", now.Add(-3*time.Hour))
	mustChtime("mid", now.Add(-1*time.Hour))
	mustChtime("new", now)

	results, err := m.RecentFiles(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Path != "new.md" {
		t.Errorf("first result = %q, want new.md", results[0].Path)
	}
	if results[1].Path != "mid.md" {
		t.Errorf("second result = %q, want mid.md", results[1].Path)
	}
}

func TestMemoryStore_Stat(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("note", "hello"); err != nil {
		t.Fatal(err)
	}

	entry, err := m.Stat("note")
	if err != nil {
		t.Fatal(err)
	}
	if entry.IsDir {
		t.Error("expected file, got dir")
	}
	if entry.Size != 5 {
		t.Errorf("Size = %d, want 5", entry.Size)
	}

	if _, err := m.Stat("missing"); err == nil {
		t.Error("Stat on missing file should error")
	}
}

func TestMemoryStore_DirSize(t *testing.T) {
	m := newTestMemory(t)
	if err := m.Write("a", "12345"); err != nil {
		t.Fatal(err)
	}
	if err := m.Write("sub/b", "1234567890"); err != nil {
		t.Fatal(err)
	}

	// Trashed file should not be counted
	if err := m.Write("trashed", "ZZZZZZ"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("trashed"); err != nil {
		t.Fatal(err)
	}

	size, count, err := m.DirSize("")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if size != 15 {
		t.Errorf("size = %d, want 15", size)
	}
}
