package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// wikiFetch fetches a Wikipedia article as clean plain text via the MediaWiki API.
// Uses action=query&prop=extracts&explaintext=true for direct plain text output
// - no HTML parsing needed.
func (te *ToolExecutor) wikiFetch(argsJSON string) string {
	var args struct {
		Title  string `json:"title"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
		Intro  bool   `json:"intro"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Title == "" {
		return "Error: title is required"
	}

	const defaultLength = 12000
	if args.Length <= 0 {
		args.Length = defaultLength
	}
	if args.Offset < 0 {
		args.Offset = 0
	}

	// Build the MediaWiki API URL
	apiURL := "https://en.wikipedia.org/w/api.php"
	params := url.Values{
		"action":      {"query"},
		"titles":      {args.Title},
		"prop":        {"extracts"},
		"explaintext": {"true"},
		"format":      {"json"},
		"redirects":   {"1"},
	}
	if args.Intro {
		params.Set("exintro", "true")
	} else {
		params.Set("exintro", "false")
	}

	fullURL := apiURL + "?" + params.Encode()

	// Check cache first
	text, cached := te.cache.Get(fullURL)
	if cached {
		te.logger.Info("wiki_fetch cache hit", "title", args.Title)
	} else {
		te.logger.Info("fetching Wikipedia article", "title", args.Title)

		req, err := http.NewRequest("GET", fullURL, nil)
		if err != nil {
			return fmt.Sprintf("Error creating request: %s", err)
		}
		req.Header.Set("User-Agent", "Faultline/1.0 (wren.mataroa.blog; research agent; https://wren.mataroa.blog)")
		req.Header.Set("Accept", "application/json")

		resp, err := te.http.Do(req)
		if err != nil {
			return fmt.Sprintf("Error fetching Wikipedia: %s", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		if err != nil {
			return fmt.Sprintf("Error reading response: %s", err)
		}

		// Parse JSON response
		var result struct {
			Query struct {
				Pages []struct {
					Missing   string `json:"missing,omitempty"`
					Title     string `json:"title"`
					PageID    int    `json:"pageid"`
					Extract   string `json:"extract"`
					Namespace struct {
						ID   int    `json:"ns"`
						Name string `json:"*"`
					} `json:"namespace"`
				} `json:"pages"`
			} `json:"query"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Sprintf("Error parsing Wikipedia API response: %s", err)
		}

		if len(result.Query.Pages) == 0 {
			return fmt.Sprintf("No results found for Wikipedia article: %s", args.Title)
		}

		page := result.Query.Pages[0]
		if page.Missing != "" {
			return fmt.Sprintf("Wikipedia article not found: %s", args.Title)
		}

		text = page.Extract
		if text == "" {
			return fmt.Sprintf("Page found (%s) but no text content was extracted.", page.Title)
		}

		// Cache the full extracted text
		te.cache.Set(fullURL, text)
	}

	totalLen := len(text)

	// Apply offset
	if args.Offset >= totalLen {
		return fmt.Sprintf("[%d total chars | offset %d is past end of content]", totalLen, args.Offset)
	}
	text = text[args.Offset:]

	// Apply length
	truncated := false
	if len(text) > args.Length {
		text = text[:args.Length]
		truncated = true
	}

	// Add position metadata
	endPos := args.Offset + len(text)
	header := fmt.Sprintf("[%d total chars | showing %d\u2013%d]", totalLen, args.Offset, endPos)
	if truncated {
		header += fmt.Sprintf(" [use offset=%d to continue]", endPos)
	}

	return header + "\n\n" + text
}
