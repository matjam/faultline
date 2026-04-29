package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "lowercases",
			in:   "Hello World",
			want: []string{"hello", "world"},
		},
		{
			name: "drops stop words",
			in:   "the quick brown fox",
			want: []string{"quick", "brown", "fox"},
		},
		{
			name: "drops short tokens",
			in:   "a aa aaa",
			want: []string{"aa", "aaa"},
		},
		{
			name: "splits on punctuation",
			in:   "hello,world!foo.bar?baz",
			want: []string{"hello", "world", "foo", "bar", "baz"},
		},
		{
			name: "preserves digits",
			in:   "year 2026 was hot",
			want: []string{"year", "2026", "hot"},
		},
		{
			name: "empty input returns empty",
			in:   "",
			want: nil,
		},
		{
			name: "stop-words-only input returns empty",
			in:   "the and or but",
			want: nil,
		},
		{
			name: "unicode letters preserved",
			in:   "café résumé",
			want: []string{"café", "résumé"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSearchIndex_EmptyAndQuery(t *testing.T) {
	idx := NewSearchIndex()

	if got := idx.Search("anything", 10, nil); got != nil {
		t.Errorf("empty index Search returned %v, want nil", got)
	}

	idx.Build(map[string]string{"a.md": "hello world"})
	if got := idx.Search("", 10, nil); got != nil {
		t.Errorf("empty query Search returned %v, want nil", got)
	}
	if got := idx.Search("the and or", 10, nil); got != nil {
		t.Errorf("stop-words-only query Search returned %v, want nil", got)
	}
}

func TestSearchIndex_RanksByRelevance(t *testing.T) {
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"climate.md":   "climate change is accelerating; warming oceans drive storms",
		"cooking.md":   "boil water, add pasta, stir frequently",
		"unrelated.md": "the quick brown fox jumps over the lazy dog",
	})

	results := idx.Search("climate warming", 10, nil)
	if len(results) == 0 {
		t.Fatal("expected results for 'climate warming', got none")
	}
	if results[0].Path != "climate.md" {
		t.Errorf("expected climate.md first, got %s (full results: %+v)", results[0].Path, results)
	}
}

func TestSearchIndex_MaxResults(t *testing.T) {
	idx := NewSearchIndex()
	docs := map[string]string{}
	for i := 'a'; i <= 'e'; i++ {
		docs[string(i)+".md"] = "shared keyword unique" + string(i)
	}
	idx.Build(docs)

	results := idx.Search("shared", 3, nil)
	if len(results) != 3 {
		t.Errorf("expected 3 results capped, got %d", len(results))
	}
}

func TestSearchIndex_Filter(t *testing.T) {
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"public/a.md":  "topic apple",
		"private/b.md": "topic apple",
	})

	results := idx.Search("apple", 10, func(path string) bool {
		return !strings.HasPrefix(path, "private/")
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 filtered result, got %d", len(results))
	}
	if results[0].Path != "public/a.md" {
		t.Errorf("expected public/a.md, got %s", results[0].Path)
	}
}

func TestSearchIndex_UpdateChangesScore(t *testing.T) {
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"a.md": "apple banana",
		"b.md": "cherry date",
	})

	if results := idx.Search("apple", 10, nil); len(results) != 1 || results[0].Path != "a.md" {
		t.Fatalf("baseline: expected a.md, got %+v", results)
	}

	// Update b.md to mention apple repeatedly; it should now appear
	idx.Update("b.md", "apple apple apple cherry date")
	results := idx.Search("apple", 10, nil)
	if len(results) != 2 {
		t.Fatalf("after update expected 2 hits, got %d (%+v)", len(results), results)
	}
}

func TestSearchIndex_Remove(t *testing.T) {
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"a.md": "apple banana",
		"b.md": "apple cherry",
	})

	idx.Remove("a.md")
	results := idx.Search("apple", 10, nil)
	if len(results) != 1 || results[0].Path != "b.md" {
		t.Errorf("after remove expected only b.md, got %+v", results)
	}

	// Remove of unknown path is a no-op, not an error
	idx.Remove("nonexistent.md")
}

func TestSearchIndex_RemovePrefix(t *testing.T) {
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"trash/a.md": "apple",
		"trash/b.md": "apple",
		"keep/c.md":  "apple",
	})

	idx.RemovePrefix("trash/")
	results := idx.Search("apple", 10, nil)
	if len(results) != 1 || results[0].Path != "keep/c.md" {
		t.Errorf("after RemovePrefix expected only keep/c.md, got %+v", results)
	}
}

func TestSearchIndex_DocFreqsCleanedOnRemove(t *testing.T) {
	// Regression: removing a document must decrement docFreqs so subsequent
	// IDF calculations don't treat removed terms as still present.
	idx := NewSearchIndex()
	idx.Build(map[string]string{
		"a.md": "uniqueterm",
		"b.md": "uniqueterm",
	})
	if got := idx.docFreqs["uniqueterm"]; got != 2 {
		t.Fatalf("docFreqs[uniqueterm] = %d, want 2", got)
	}
	idx.Remove("a.md")
	if got := idx.docFreqs["uniqueterm"]; got != 1 {
		t.Errorf("after remove, docFreqs[uniqueterm] = %d, want 1", got)
	}
	idx.Remove("b.md")
	if _, present := idx.docFreqs["uniqueterm"]; present {
		t.Errorf("after removing all docs, docFreqs still has uniqueterm")
	}
}
