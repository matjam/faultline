// Package bm25 is a pure-Go BM25 search index over text documents keyed
// by string paths. It has no I/O dependencies and is rebuilt from disk on
// each agent startup. Used as the search backend for the memory adapter.
package bm25

import (
	"math"
	"strings"
	"unicode"
)

// Result represents a search hit from the index. The Score field is
// populated by Search; for non-scored uses (the memory adapter's
// RecentFiles, AllFiles) it is left at zero.
type Result struct {
	Path    string  `json:"path"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// Index is a BM25-based search index over text documents.
type Index struct {
	// documents maps path -> tokenized content
	documents map[string][]string
	// rawContent maps path -> original content
	rawContent map[string]string
	// docFreqs maps term -> number of documents containing it
	docFreqs map[string]int
	// avgDocLen is the average document length in tokens
	avgDocLen float64
	// k1 and b are BM25 tuning parameters
	k1 float64
	b  float64
}

// New creates a new empty search index.
func New() *Index {
	return &Index{
		documents:  make(map[string][]string),
		rawContent: make(map[string]string),
		docFreqs:   make(map[string]int),
		k1:         1.5,
		b:          0.75,
	}
}

// Build rebuilds the index from a set of documents.
func (idx *Index) Build(docs map[string]string) {
	idx.documents = make(map[string][]string, len(docs))
	idx.rawContent = make(map[string]string, len(docs))
	idx.docFreqs = make(map[string]int)

	totalLen := 0
	for path, content := range docs {
		tokens := tokenize(content)
		idx.documents[path] = tokens
		idx.rawContent[path] = content
		totalLen += len(tokens)

		// Count document frequencies
		seen := make(map[string]bool)
		for _, t := range tokens {
			if !seen[t] {
				idx.docFreqs[t]++
				seen[t] = true
			}
		}
	}

	if len(docs) > 0 {
		idx.avgDocLen = float64(totalLen) / float64(len(docs))
	}
}

// Update adds or updates a single document in the index.
func (idx *Index) Update(path, content string) {
	// Remove old document frequencies if it existed
	if oldTokens, exists := idx.documents[path]; exists {
		seen := make(map[string]bool)
		for _, t := range oldTokens {
			if !seen[t] {
				idx.docFreqs[t]--
				if idx.docFreqs[t] <= 0 {
					delete(idx.docFreqs, t)
				}
				seen[t] = true
			}
		}
	}

	tokens := tokenize(content)
	idx.documents[path] = tokens
	idx.rawContent[path] = content

	// Update document frequencies
	seen := make(map[string]bool)
	for _, t := range tokens {
		if !seen[t] {
			idx.docFreqs[t]++
			seen[t] = true
		}
	}

	// Recalculate average document length
	totalLen := 0
	for _, toks := range idx.documents {
		totalLen += len(toks)
	}
	if len(idx.documents) > 0 {
		idx.avgDocLen = float64(totalLen) / float64(len(idx.documents))
	}
}

// Remove removes a document from the index.
func (idx *Index) Remove(path string) {
	tokens, exists := idx.documents[path]
	if !exists {
		return
	}

	// Remove document frequencies
	seen := make(map[string]bool)
	for _, t := range tokens {
		if !seen[t] {
			idx.docFreqs[t]--
			if idx.docFreqs[t] <= 0 {
				delete(idx.docFreqs, t)
			}
			seen[t] = true
		}
	}

	delete(idx.documents, path)
	delete(idx.rawContent, path)

	// Recalculate average document length
	totalLen := 0
	for _, toks := range idx.documents {
		totalLen += len(toks)
	}
	if len(idx.documents) > 0 {
		idx.avgDocLen = float64(totalLen) / float64(len(idx.documents))
	} else {
		idx.avgDocLen = 0
	}
}

// RemovePrefix removes all documents whose path starts with the given prefix.
func (idx *Index) RemovePrefix(prefix string) {
	var toRemove []string
	for path := range idx.documents {
		if strings.HasPrefix(path, prefix) {
			toRemove = append(toRemove, path)
		}
	}
	for _, path := range toRemove {
		idx.Remove(path)
	}
}

// Search finds the most relevant documents for a query.
// An optional filter function can be provided to exclude documents before
// scoring. If filter is non-nil, only documents for which filter(path)
// returns true are considered.
func (idx *Index) Search(query string, maxResults int, filter func(path string) bool) []Result {
	if len(idx.documents) == 0 {
		return nil
	}

	queryTokens := tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}

	n := float64(len(idx.documents))
	type scored struct {
		path  string
		score float64
	}

	var results []scored

	for path, docTokens := range idx.documents {
		if filter != nil && !filter(path) {
			continue
		}

		// Count term frequencies in this document
		tf := make(map[string]int)
		for _, t := range docTokens {
			tf[t]++
		}

		docLen := float64(len(docTokens))
		score := 0.0

		for _, qt := range queryTokens {
			freq := float64(tf[qt])
			if freq == 0 {
				continue
			}

			// BM25 IDF
			nq := float64(idx.docFreqs[qt])
			idf := math.Log((n-nq+0.5)/(nq+0.5) + 1)

			// BM25 term score
			num := freq * (idx.k1 + 1)
			denom := freq + idx.k1*(1-idx.b+idx.b*docLen/idx.avgDocLen)
			score += idf * num / denom
		}

		if score > 0 {
			results = append(results, scored{path: path, score: score})
		}
	}

	// Sort by score descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].score > results[i].score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > maxResults {
		results = results[:maxResults]
	}

	var out []Result
	for _, r := range results {
		content := idx.rawContent[r.path]
		out = append(out, Result{
			Path:    r.path,
			Content: content,
			Score:   r.score,
		})
	}

	return out
}

// stopWords is a set of common English words to exclude from indexing.
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "by": true, "for": true, "from": true,
	"has": true, "he": true, "in": true, "is": true, "it": true,
	"its": true, "of": true, "on": true, "or": true, "that": true,
	"the": true, "to": true, "was": true, "were": true, "will": true,
	"with": true, "this": true, "but": true, "they": true, "have": true,
	"had": true, "not": true, "been": true, "she": true, "her": true,
	"his": true, "their": true, "which": true, "would": true, "there": true,
	"what": true, "about": true, "if": true, "up": true, "out": true,
	"do": true, "no": true, "so": true, "can": true, "who": true,
	"get": true, "my": true, "me": true, "we": true, "you": true,
	"your": true, "i": true, "am": true, "just": true, "than": true,
	"then": true, "also": true, "into": true, "could": true, "more": true,
	"some": true, "when": true, "very": true, "how": true, "all": true,
}

// tokenize splits text into lowercase tokens, removing stop words and punctuation.
func tokenize(text string) []string {
	lower := strings.ToLower(text)

	// Split on non-alphanumeric characters
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	var tokens []string
	for _, w := range words {
		if len(w) < 2 {
			continue
		}
		if stopWords[w] {
			continue
		}
		tokens = append(tokens, w)
	}

	return tokens
}
