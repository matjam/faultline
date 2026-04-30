package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"golang.org/x/net/html"
)

// webCacheEntry holds a cached web page fetch result.
type webCacheEntry struct {
	content   string
	fetchedAt time.Time
}

// webCache is a TTL cache for web_fetch results with proactive eviction.
type webCache struct {
	mu      sync.Mutex
	entries map[string]webCacheEntry
	ttl     time.Duration
	stop    chan struct{}
}

func newWebCache(ttl time.Duration) *webCache {
	c := &webCache{
		entries: make(map[string]webCacheEntry),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}
	go c.evictLoop()
	return c
}

// Close stops the background eviction goroutine.
func (c *webCache) Close() {
	close(c.stop)
}

// evictLoop periodically removes expired entries.
func (c *webCache) evictLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.evict()
		}
	}
}

func (c *webCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for url, entry := range c.entries {
		if now.Sub(entry.fetchedAt) > c.ttl {
			delete(c.entries, url)
		}
	}
}

// Get returns cached content and true if the entry exists and hasn't expired.
func (c *webCache) Get(url string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[url]
	if !ok || time.Since(entry.fetchedAt) > c.ttl {
		if ok {
			delete(c.entries, url)
		}
		return "", false
	}
	return entry.content, true
}

// Set stores content for a URL.
func (c *webCache) Set(url, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[url] = webCacheEntry{content: content, fetchedAt: time.Now()}
}

// ToolDefs returns the tool definitions for the OpenAI API.
// Tools are conditional on what capabilities are available.
func (te *ToolExecutor) ToolDefs() []openai.Tool {
	tools := []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "web_fetch",
				Description: "Fetch a webpage and return its content as readable text. HTML is converted to plain text. Returns a window of text from the page (default 12000 chars). Use offset to read further into the page. Results are cached briefly so repeated calls to the same URL are free.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "The URL to fetch (http:// or https://).",
						},
						"offset": map[string]interface{}{
							"type":        "integer",
							"description": "Character offset to start reading from. Defaults to 0 (start of page). Use this to paginate through long pages.",
						},
						"length": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of characters to return. Defaults to 12000.",
						},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "wiki_fetch",
				Description: "Fetch a Wikipedia article as clean plain text via the MediaWiki API. Returns the full article text with no HTML. Use offset to read further into long articles. Set intro=true for just the introduction. Results are cached briefly so repeated calls to the same article are free.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"title": map[string]interface{}{
							"type":        "string",
							"description": "The Wikipedia article title (e.g., 'Enactivism', 'Quantum computing').",
						},
						"offset": map[string]interface{}{
							"type":        "integer",
							"description": "Character offset to start reading from. Defaults to 0 (start of article). Use this to paginate through long articles.",
						},
						"length": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of characters to return. Defaults to 12000.",
						},
						"intro": map[string]interface{}{
							"type":        "boolean",
							"description": "If true, only fetch the introductory section. Defaults to false (full article).",
						},
					},
					"required": []string{"title"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_read",
				Description: "Read a memory file. Returns a metadata header (file size, last modified, total lines) followed by file content with line numbers. Supports reading a specific range of lines via optional offset and lines parameters.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the memory file relative to memory root (e.g., 'notes/analysis'). The .md extension is added automatically if missing.",
						},
						"offset": map[string]interface{}{
							"type":        "integer",
							"description": "Line number to start reading from (1-indexed). Defaults to 1 (start of file). Optional.",
						},
						"lines": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of lines to return. Defaults to all lines. Optional.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_write",
				Description: "Write or update a memory file. Creates parent directories automatically. Returns confirmation with the number of bytes written. Use this to store research, reflections, opinions, and any information you want to persist.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path for the memory file relative to memory root (e.g., 'politics/2026/april-analysis'). The .md extension is added automatically if missing.",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The markdown content to write to the file.",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_list",
				Description: "List files and directories in your memory. Returns one entry per line. Files show: name, size in bytes, and last modified timestamp. Directories show: name, total file count, and total size of all contents. Use '' or '/' for the root directory.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"directory": map[string]interface{}{
							"type":        "string",
							"description": "Directory path to list, relative to memory root. Use '' for the root.",
						},
					},
					"required": []string{"directory"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_search",
				Description: "Search across all memory files by keyword relevance (BM25). Returns up to 5 results, each with: file path, relevance score, and content. Long results are clipped with a hint pointing back at memory_read so you can load the full file. Use this to find memories by topic when you don't know the exact file path. Optionally filter by file modification date.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Search query - keywords describing what you're looking for.",
						},
						"modified_after": map[string]interface{}{
							"type":        "string",
							"description": "Only include files modified on or after this date (format: YYYY-MM-DD). Optional.",
						},
						"modified_before": map[string]interface{}{
							"type":        "string",
							"description": "Only include files modified on or before this date (format: YYYY-MM-DD, treated as end of day). Optional.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_delete",
				Description: "Delete a memory file or directory by moving it to the trash. Deleted files can be restored later with memory_restore. Deleting a directory moves it and ALL of its contents to the trash. Returns confirmation of what was deleted.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the file or directory to delete, relative to memory root (e.g., 'old-notes' or 'research/outdated'). For files, .md extension is added automatically if missing.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_restore",
				Description: "Restore a previously deleted file or directory from the trash back to its original location. Use memory_list_trash to see what is available to restore.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file or directory to restore, relative to the trash root (same as the original memory path). Use memory_list_trash to see available paths.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_list_trash",
				Description: "List files and directories currently in the trash. These are files that were previously deleted and can be restored with memory_restore. Use '' or '/' for the trash root.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"directory": map[string]interface{}{
							"type":        "string",
							"description": "Directory path to list within the trash. Use '' for the trash root.",
						},
					},
					"required": []string{"directory"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_empty_trash",
				Description: "Permanently delete ALL files and directories in the trash. This action is irreversible. Use this to free up space when you are sure you no longer need any trashed files.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_move",
				Description: "Move or rename a memory file or directory. Creates destination parent directories automatically. Returns confirmation with the source and destination paths. Use this to reorganize your memory structure.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"source": map[string]interface{}{
							"type":        "string",
							"description": "Current path of the file or directory, relative to memory root.",
						},
						"destination": map[string]interface{}{
							"type":        "string",
							"description": "New path for the file or directory, relative to memory root. For files, .md extension is added automatically if missing.",
						},
					},
					"required": []string{"source", "destination"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_grep",
				Description: "Search within a single memory file using a regex pattern. Returns matching lines with their line numbers. Use this to find specific content within a large file without reading the whole thing into context.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the memory file to search within, relative to memory root.",
						},
						"pattern": map[string]interface{}{
							"type":        "string",
							"description": "Regex pattern to search for (e.g., 'climate.*policy', 'TODO', '## .*Summary').",
						},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_edit",
				Description: "Edit a memory file by finding an exact string and replacing it. The old_string must match exactly (including whitespace and newlines). If the old_string appears multiple times, the operation fails unless replace_all is true. Use memory_read with offset/lines or memory_grep to find the exact text first.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the memory file relative to memory root. The .md extension is added automatically if missing.",
						},
						"old_string": map[string]interface{}{
							"type":        "string",
							"description": "The exact string to find in the file. Must match precisely, including whitespace and newlines.",
						},
						"new_string": map[string]interface{}{
							"type":        "string",
							"description": "The replacement string. Can be empty to delete the matched text.",
						},
						"replace_all": map[string]interface{}{
							"type":        "boolean",
							"description": "If true, replace all occurrences. If false (default), the old_string must appear exactly once or the operation fails.",
						},
					},
					"required": []string{"path", "old_string", "new_string"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_append",
				Description: "Append content to the end of a memory file without reading it first. Creates the file if it does not exist. Useful for journals, logs, running lists, and any file you frequently add to.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the memory file relative to memory root. The .md extension is added automatically if missing.",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The content to append to the end of the file.",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "memory_insert",
				Description: "Insert content at a specific line number in a memory file. The new content is inserted before the specified line, pushing existing lines down. If the line number exceeds the file length, content is appended at the end. Use memory_grep to find the target line number first.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path to the memory file relative to memory root. The .md extension is added automatically if missing.",
						},
						"line": map[string]interface{}{
							"type":        "integer",
							"description": "Line number to insert before (1-indexed). Content is inserted before this line.",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The content to insert.",
						},
					},
					"required": []string{"path", "line", "content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "context_status",
				Description: "Check your current context window usage. Returns: estimated tokens used, maximum token limit, percentage used, and tokens remaining. Use this to decide whether to save information to memory before your context fills up.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "get_time",
				Description: "Get the current date and time. Returns the time formatted as 'Monday, January 2, 2006 3:04:05 PM MST'.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	}

	if te.telegram != nil {
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "send_message",
				Description: "Send a message to your collaborator via Telegram. Use this to share interesting findings, ask questions, report on your progress, or communicate anything you want. Your collaborator may not respond immediately.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"text": map[string]interface{}{
							"type":        "string",
							"description": "The message text to send.",
						},
					},
					"required": []string{"text"},
				},
			},
		})
	}

	if te.email != nil {
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "email_fetch",
				Description: "Fetch emails from an IMAP mailbox. Returns email overviews (from, date, subject, size, flags) for recent messages, or full body for a specific UID.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"folder": map[string]interface{}{
							"type":        "string",
							"description": "Mailbox folder to read from (default: INBOX).",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of emails to fetch (default: 10).",
						},
						"uid": map[string]interface{}{
							"type":        "integer",
							"description": "If set, fetch full body of this specific email UID instead of recent overviews.",
						},
					},
				},
			},
		})
	}

	if te.sandbox != nil {
		tools = append(tools,
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_write",
					Description: "Create or overwrite a file in the sandbox. Writes the full file content. Use folder 'scripts' for Python scripts, 'input' for input data, 'output' for output data. All filenames must be lowercase, flat (no subfolders), and contain only [a-z0-9._-].",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Target folder: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename (lowercase, no subfolders). Example: 'analyze_data.py', 'config.json'.",
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "The full file content to write.",
							},
						},
						"required": []string{"folder", "filename", "content"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_read",
					Description: "Read a file from the sandbox. Returns the file content with line numbers. Use folder 'scripts' for Python scripts, 'input' for input data, 'output' for output data. All filenames must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Source folder: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename to read (lowercase, no subfolders).",
							},
							"offset": map[string]interface{}{
								"type":        "integer",
								"description": "Line number to start reading from (1-indexed). Defaults to 1 (start of file). Optional.",
							},
							"lines": map[string]interface{}{
								"type":        "integer",
								"description": "Maximum number of lines to return. Defaults to all lines. Optional.",
							},
						},
						"required": []string{"folder", "filename"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_delete",
					Description: "Delete a file from the sandbox. This is permanent. All filenames must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Folder containing the file: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename to delete (lowercase, no subfolders).",
							},
						},
						"required": []string{"folder", "filename"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_rename",
					Description: "Rename a file within the same sandbox folder. Both old and new names must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Folder containing the file: 'scripts', 'input', or 'output'.",
							},
							"old_name": map[string]interface{}{
								"type":        "string",
								"description": "Current filename (lowercase, no subfolders).",
							},
							"new_name": map[string]interface{}{
								"type":        "string",
								"description": "New filename (lowercase, no subfolders).",
							},
						},
						"required": []string{"folder", "old_name", "new_name"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_list",
					Description: "List all files in a sandbox folder. Returns filename, size, and last modified time for each file. All filenames are lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Folder to list: 'scripts', 'input', or 'output'.",
							},
						},
						"required": []string{"folder"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_edit",
					Description: "Edit a sandbox file by finding an exact string and replacing it. The old_string must match exactly (including whitespace and newlines). If old_string appears multiple times, the operation fails unless replace_all is true. Use sandbox_read with offset/lines to find the exact text first. All filenames must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Folder containing the file: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename to edit (lowercase, no subfolders).",
							},
							"old_string": map[string]interface{}{
								"type":        "string",
								"description": "The exact string to find in the file. Must match precisely.",
							},
							"new_string": map[string]interface{}{
								"type":        "string",
								"description": "The replacement string. Can be empty to delete the matched text.",
							},
							"replace_all": map[string]interface{}{
								"type":        "boolean",
								"description": "If true, replace all occurrences. If false (default), old_string must appear exactly once.",
							},
						},
						"required": []string{"folder", "filename", "old_string", "new_string"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_append",
					Description: "Append content to the end of a sandbox file. Creates the file if it does not exist. Useful for building up data files incrementally. All filenames must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Target folder: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename to append to (lowercase, no subfolders).",
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "The content to append to the end of the file.",
							},
						},
						"required": []string{"folder", "filename", "content"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_insert",
					Description: "Insert content at a specific line number in a sandbox file. Content is inserted before the specified line, pushing existing lines down. If the line number exceeds file length, content is appended at the end. All filenames must be lowercase.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"folder": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"scripts", "input", "output"},
								"description": "Folder containing the file: 'scripts', 'input', or 'output'.",
							},
							"filename": map[string]interface{}{
								"type":        "string",
								"description": "Filename to insert into (lowercase, no subfolders).",
							},
							"line": map[string]interface{}{
								"type":        "integer",
								"description": "Line number to insert before (1-indexed).",
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "The content to insert.",
							},
						},
						"required": []string{"folder", "filename", "line", "content"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_execute",
					Description: "Execute a Python script in the sandbox. The script must exist in the scripts/ folder. Dependencies are synced automatically before execution. The script runs in a Docker container with read-only access to /scripts and /input, and read-write access to /output. Returns combined stdout/stderr output. Output beyond the configured cap is clipped with a hint telling you the full size; for large output, write results to /output/ from your script and read them back with sandbox_read.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"script": map[string]interface{}{
								"type":        "string",
								"description": "Script filename in the scripts/ folder (lowercase). Example: 'analyze_data.py'.",
							},
							"args": map[string]interface{}{
								"type":        "array",
								"items":       map[string]interface{}{"type": "string"},
								"description": "Command-line arguments to pass to the script. Optional.",
							},
						},
						"required": []string{"script"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_install_package",
					Description: "Install a Python package into the sandbox environment using uv. The package is added to pyproject.toml and available to all scripts. Example: 'requests', 'pandas>=2.0'.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"package": map[string]interface{}{
								"type":        "string",
								"description": "Package name (and optional version constraint). Example: 'requests', 'numpy>=1.26'.",
							},
						},
						"required": []string{"package"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_upgrade_package",
					Description: "Upgrade a Python package in the sandbox environment to the latest compatible version.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"package": map[string]interface{}{
								"type":        "string",
								"description": "Package name to upgrade. Example: 'requests', 'numpy'.",
							},
						},
						"required": []string{"package"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_remove_package",
					Description: "Remove a Python package from the sandbox environment. Removes it from pyproject.toml.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"package": map[string]interface{}{
								"type":        "string",
								"description": "Package name to remove.",
							},
						},
						"required": []string{"package"},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_list_packages",
					Description: "List all Python packages installed in the sandbox environment. Reads from pyproject.toml.",
					Parameters: map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					},
				},
			},
			openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "sandbox_shell",
					Description: "Run an arbitrary shell command inside the sandbox Docker container. Use this to execute commands like git, ls, cat, wc, grep, find, or any other command available in the container. The command runs with the same mounts as sandbox scripts (/scripts read-only, /input read-only, /output read-write). Output beyond the configured cap is clipped with a hint telling you the full size; redirect large output to /output/ and read it back with sandbox_read.",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"command": map[string]interface{}{
								"type":        "string",
								"description": "Shell command to execute. Example: 'ls -la /scripts/', 'git log --oneline', 'wc -l /output/results.txt'.",
							},
						},
						"required": []string{"command"},
					},
				},
			},
		)
	}

	return tools
}

// indexKey returns the canonical search index key for a memory path.
// The index is keyed by lowercase relative paths with .md extension,
// matching the format returned by MemoryStore.AllFiles().
func indexKey(path string) string {
	path = strings.ToLower(path)
	path = strings.TrimPrefix(filepath.Clean(path), "/")
	if path != "" && !strings.HasSuffix(path, ".md") {
		path = path + ".md"
	}
	return path
}

// ToolExecutor handles executing tool calls.
type ToolExecutor struct {
	memory        *MemoryStore
	index         *SearchIndex
	telegram      *Telegram
	sandbox       *Sandbox
	email         *EmailConfig
	kobold        *KoboldExtras // optional; nil means no perf info in context_status
	logger        *slog.Logger
	http          *http.Client
	cache         *webCache
	maxTokens     int
	currentTokens int
	limits        LimitsConfig
}

// NewToolExecutor creates a new tool executor. kobold may be nil.
func NewToolExecutor(memory *MemoryStore, index *SearchIndex, telegram *Telegram, sandbox *Sandbox, email *EmailConfig, kobold *KoboldExtras, logger *slog.Logger, maxTokens int, limits LimitsConfig) *ToolExecutor {
	if sandbox != nil {
		sandbox.SetOutputLimit(limits.SandboxOutputChars)
	}
	return &ToolExecutor{
		memory:    memory,
		index:     index,
		telegram:  telegram,
		sandbox:   sandbox,
		email:     email,
		kobold:    kobold,
		logger:    logger,
		maxTokens: maxTokens,
		limits:    limits,
		cache:     newWebCache(60 * time.Second),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: false,
				},
			},
		},
	}
}

// Close releases resources held by the tool executor.
func (te *ToolExecutor) Close() {
	te.cache.Close()
}

// reindexDir re-indexes all .md files under a memory directory path.
// Used after directory-level operations (delete, move, restore) to keep the
// search index consistent.
func (te *ToolExecutor) reindexDir(dirPath string) {
	entries, err := te.memory.List(dirPath)
	if err != nil {
		return
	}
	for _, e := range entries {
		subPath := dirPath + "/" + e.Name
		if e.IsDir {
			te.reindexDir(subPath)
		} else {
			content, readErr := te.memory.Read(subPath)
			if readErr == nil {
				te.index.Update(indexKey(subPath), content)
			}
		}
	}
}

// SetContextInfo updates the current token usage estimate.
// Called by the agent before each batch of tool executions.
func (te *ToolExecutor) SetContextInfo(currentTokens int) {
	te.currentTokens = currentTokens
}

// Execute runs a tool call and returns the result string.
// ctx is used for operations that need cancellation/timeout (e.g. sandbox Docker commands).
func (te *ToolExecutor) Execute(ctx context.Context, call openai.ToolCall) string {
	name := call.Function.Name
	args := call.Function.Arguments

	te.logger.Info("tool call", "name", name, "args_len", len(args))

	switch name {
	case "web_fetch":
		return te.webFetch(args)
	case "wiki_fetch":
		return te.wikiFetch(args)
	case "memory_read":
		return te.memoryRead(args)
	case "memory_write":
		return te.memoryWrite(args)
	case "memory_list":
		return te.memoryList(args)
	case "memory_search":
		return te.memorySearch(args)
	case "memory_delete":
		return te.memoryDelete(args)
	case "memory_restore":
		return te.memoryRestore(args)
	case "memory_list_trash":
		return te.memoryListTrash(args)
	case "memory_empty_trash":
		return te.memoryEmptyTrash()
	case "memory_move":
		return te.memoryMove(args)
	case "memory_grep":
		return te.memoryGrep(args)
	case "memory_edit":
		return te.memoryEdit(args)
	case "memory_append":
		return te.memoryAppend(args)
	case "memory_insert":
		return te.memoryInsert(args)
	case "context_status":
		return te.contextStatus()
	case "get_time":
		return time.Now().Format("Monday, January 2, 2006 3:04:05 PM MST")
	case "send_message":
		return te.sendMessage(args)
	case "email_fetch":
		return te.emailFetch(args)
	// Sandbox tools
	case "sandbox_write":
		return te.sandboxWrite(args)
	case "sandbox_read":
		return te.sandboxRead(args)
	case "sandbox_delete":
		return te.sandboxDelete(args)
	case "sandbox_rename":
		return te.sandboxRename(args)
	case "sandbox_list":
		return te.sandboxList(args)
	case "sandbox_edit":
		return te.sandboxEdit(args)
	case "sandbox_append":
		return te.sandboxAppend(args)
	case "sandbox_insert":
		return te.sandboxInsert(args)
	case "sandbox_execute":
		return te.sandboxExecute(ctx, args)
	case "sandbox_install_package":
		return te.sandboxInstallPackage(ctx, args)
	case "sandbox_upgrade_package":
		return te.sandboxUpgradePackage(ctx, args)
	case "sandbox_remove_package":
		return te.sandboxRemovePackage(ctx, args)
	case "sandbox_list_packages":
		return te.sandboxListPackages()
	case "sandbox_shell":
		return te.sandboxShell(ctx, args)
	default:
		return fmt.Sprintf("Unknown tool: %s", name)
	}
}

func (te *ToolExecutor) sandboxShell(ctx context.Context, argsJSON string) string {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if te.sandbox == nil {
		return "Sandbox is not configured."
	}

	output, err := te.sandbox.ShellExec(ctx, args.Command)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return output
}

func (te *ToolExecutor) webFetch(argsJSON string) string {
	var args struct {
		URL    string `json:"url"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.URL == "" {
		return "Error: url is required"
	}

	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		args.URL = "https://" + args.URL
	}

	const defaultLength = 12000
	if args.Length <= 0 {
		args.Length = defaultLength
	}
	if args.Offset < 0 {
		args.Offset = 0
	}

	// Check cache first
	text, cached := te.cache.Get(args.URL)
	if cached {
		te.logger.Info("web_fetch cache hit", "url", args.URL)
	} else {
		te.logger.Info("fetching URL", "url", args.URL)

		req, err := http.NewRequest("GET", args.URL, nil)
		if err != nil {
			return fmt.Sprintf("Error creating request: %s", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,*/*")

		resp, err := te.http.Do(req)
		if err != nil {
			return fmt.Sprintf("Error fetching URL: %s", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)
		}

		// Limit response size to 1MB
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if err != nil {
			return fmt.Sprintf("Error reading response: %s", err)
		}

		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml") {
			text = extractTextFromHTML(string(body), args.URL)
		} else {
			text = string(body)
		}

		if text == "" {
			return "Page fetched but no readable text content was extracted."
		}

		// Cache the full extracted text
		te.cache.Set(args.URL, text)
	}

	totalLen := len(text)

	// Apply offset
	if args.Offset >= totalLen {
		return fmt.Sprintf("[%d total chars, offset %d is past end of content]", totalLen, args.Offset)
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
	header := fmt.Sprintf("[%d total chars | showing %d–%d]", totalLen, args.Offset, endPos)
	if truncated {
		header += fmt.Sprintf(" [use offset=%d to continue]", endPos)
	}

	return header + "\n\n" + text
}

func (te *ToolExecutor) memoryRead(argsJSON string) string {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Lines  int    `json:"lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	// Get file metadata
	stat, statErr := te.memory.Stat(args.Path)

	content, totalLines, err := te.memory.ReadLines(args.Path, args.Offset, args.Lines)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Prepend metadata header if stat succeeded
	var header string
	if statErr == nil {
		header = fmt.Sprintf("[%s | %d bytes | %d lines | modified %s]\n\n",
			stat.Name, stat.Size, totalLines, stat.ModTime.Format("2006-01-02 15:04"))
	}

	return header + content
}

func (te *ToolExecutor) memoryWrite(argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Content == "" {
		return "Error: content is required"
	}

	if err := te.memory.Write(args.Path, args.Content); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Update search index
	te.index.Update(indexKey(args.Path), args.Content)

	te.logger.Info("memory written", "path", args.Path, "size", len(args.Content))
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path)
}

func (te *ToolExecutor) memoryList(argsJSON string) string {
	var args struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	entries, err := te.memory.List(args.Directory)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	if len(entries) == 0 {
		return "Directory is empty or does not exist."
	}

	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir {
			// Compute recursive size and file count for directories
			dirPath := args.Directory
			if dirPath != "" && dirPath != "/" {
				dirPath = dirPath + "/" + e.Name
			} else {
				dirPath = e.Name
			}
			totalSize, fileCount, sizeErr := te.memory.DirSize(dirPath)
			if sizeErr == nil {
				fmt.Fprintf(&sb, "  [dir]  %s/ (%d files, %s, modified %s)\n",
					e.Name, fileCount, formatBytes(totalSize),
					e.ModTime.Format("2006-01-02 15:04"))
			} else {
				fmt.Fprintf(&sb, "  [dir]  %s/ (modified %s)\n",
					e.Name, e.ModTime.Format("2006-01-02 15:04"))
			}
		} else {
			fmt.Fprintf(&sb, "  [file] %s (%s, modified %s)\n",
				e.Name, formatBytes(e.Size), e.ModTime.Format("2006-01-02 15:04"))
		}
	}

	return sb.String()
}

// formatBytes formats a byte count into a human-readable string.
func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

func (te *ToolExecutor) memorySearch(argsJSON string) string {
	var args struct {
		Query          string `json:"query"`
		ModifiedAfter  string `json:"modified_after"`
		ModifiedBefore string `json:"modified_before"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Query == "" {
		return "Error: query is required"
	}

	// Build a date filter if either bound is provided.
	var filter func(string) bool
	if args.ModifiedAfter != "" || args.ModifiedBefore != "" {
		var after, before time.Time
		var err error

		if args.ModifiedAfter != "" {
			after, err = time.Parse("2006-01-02", args.ModifiedAfter)
			if err != nil {
				return fmt.Sprintf("Error: invalid modified_after date %q (expected YYYY-MM-DD)", args.ModifiedAfter)
			}
		}
		if args.ModifiedBefore != "" {
			before, err = time.Parse("2006-01-02", args.ModifiedBefore)
			if err != nil {
				return fmt.Sprintf("Error: invalid modified_before date %q (expected YYYY-MM-DD)", args.ModifiedBefore)
			}
			// Treat as end of day
			before = before.Add(24*time.Hour - time.Nanosecond)
		}

		filter = func(path string) bool {
			stat, statErr := te.memory.Stat(path)
			if statErr != nil {
				return false
			}
			if !after.IsZero() && stat.ModTime.Before(after) {
				return false
			}
			if !before.IsZero() && stat.ModTime.After(before) {
				return false
			}
			return true
		}
	}

	results := te.index.Search(args.Query, 5, filter)
	if len(results) == 0 {
		return "No matching memories found."
	}

	var sb strings.Builder
	limit := te.limits.MemorySearchResultChars
	for i, r := range results {
		content := r.Content
		total := len(content)
		var tail string
		if limit > 0 && total > limit {
			content = content[:limit]
			tail = fmt.Sprintf("\n[truncated: showing first %d of %d chars; call memory_read with path=%q to read the full file, or with offset=%d to continue from where this preview ends]",
				limit, total, r.Path, lineCountFor(content)+1)
		}
		fmt.Fprintf(&sb, "--- Result %d: %s (score: %.2f) ---\n%s%s\n\n",
			i+1, r.Path, r.Score, content, tail)
	}

	return sb.String()
}

func (te *ToolExecutor) memoryDelete(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}

	// Check what we're deleting for the confirmation message
	stat, _ := te.memory.Stat(args.Path)

	if err := te.memory.Delete(args.Path); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Remove from search index -- for directories, remove all entries under the path
	if stat != nil && stat.IsDir {
		te.index.RemovePrefix(indexKey(args.Path + "/"))
	} else {
		te.index.Remove(indexKey(args.Path))
	}

	te.logger.Info("memory deleted (moved to trash)", "path", args.Path)

	if stat != nil && stat.IsDir {
		return fmt.Sprintf("Deleted directory '%s' and all its contents (moved to trash; use memory_restore to recover).", args.Path)
	}
	return fmt.Sprintf("Deleted file '%s' (moved to trash; use memory_restore to recover).", args.Path)
}

func (te *ToolExecutor) memoryRestore(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}

	restoredPath, err := te.memory.Restore(args.Path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Re-index the restored file(s) -- could be a single file or a directory
	stat, statErr := te.memory.Stat(restoredPath)
	if statErr == nil && stat.IsDir {
		te.reindexDir(restoredPath)
	} else {
		content, readErr := te.memory.Read(restoredPath)
		if readErr == nil {
			te.index.Update(indexKey(restoredPath), content)
		}
	}

	te.logger.Info("memory restored from trash", "path", restoredPath)
	return fmt.Sprintf("Restored '%s' from trash.", restoredPath)
}

func (te *ToolExecutor) memoryListTrash(argsJSON string) string {
	var args struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	entries, err := te.memory.ListTrash(args.Directory)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	if len(entries) == 0 {
		return "Trash is empty."
	}

	var sb strings.Builder
	sb.WriteString("Trash contents:\n")
	for _, e := range entries {
		if e.IsDir {
			fmt.Fprintf(&sb, "  [dir]  %s/ (modified %s)\n",
				e.OriginalPath, e.ModTime.Format("2006-01-02 15:04"))
		} else {
			fmt.Fprintf(&sb, "  [file] %s (%s, modified %s)\n",
				e.OriginalPath, formatBytes(e.Size), e.ModTime.Format("2006-01-02 15:04"))
		}
	}
	sb.WriteString("\nUse memory_restore with the path shown above to restore a file.")

	return sb.String()
}

func (te *ToolExecutor) memoryEmptyTrash() string {
	if err := te.memory.EmptyTrash(); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	te.logger.Info("trash emptied")
	return "Trash has been permanently emptied. All trashed files are gone."
}

func (te *ToolExecutor) memoryMove(argsJSON string) string {
	var args struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Source == "" {
		return "Error: source is required"
	}
	if args.Destination == "" {
		return "Error: destination is required"
	}

	// Check if source is a directory before moving
	stat, _ := te.memory.Stat(args.Source)
	isDir := stat != nil && stat.IsDir

	// Read content before move for index update (only for files)
	var oldContent string
	var readErr error
	if !isDir {
		oldContent, readErr = te.memory.Read(args.Source)
	}

	if err := te.memory.Move(args.Source, args.Destination); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Update search index: remove old entries, add new ones
	if isDir {
		te.index.RemovePrefix(indexKey(args.Source + "/"))
		te.reindexDir(args.Destination)
	} else {
		te.index.Remove(indexKey(args.Source))
		if readErr == nil {
			te.index.Update(indexKey(args.Destination), oldContent)
		}
	}

	te.logger.Info("memory moved", "from", args.Source, "to", args.Destination)
	return fmt.Sprintf("Moved '%s' to '%s'.", args.Source, args.Destination)
}

func (te *ToolExecutor) memoryGrep(argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Pattern == "" {
		return "Error: pattern is required"
	}

	matches, err := te.memory.Grep(args.Path, args.Pattern)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No matches for pattern '%s' in %s.", args.Pattern, args.Path)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) for '%s' in %s:\n\n", len(matches), args.Pattern, args.Path)
	for _, m := range matches {
		line := m.Line
		if len(line) > 200 {
			line = line[:200] + "..."
		}
		fmt.Fprintf(&sb, "  L%d: %s\n", m.LineNum, line)
	}

	return sb.String()
}

func (te *ToolExecutor) contextStatus() string {
	used := te.currentTokens
	max := te.maxTokens
	if max == 0 {
		return "Context window information is not available."
	}

	remaining := max - used
	if remaining < 0 {
		remaining = 0
	}
	pct := float64(used) / float64(max) * 100

	var urgency string
	switch {
	case pct >= 90:
		urgency = "CRITICAL - Save important work to memory immediately. Compaction is imminent."
	case pct >= 75:
		urgency = "HIGH - Consider saving work to memory. Context compaction may occur soon."
	case pct >= 50:
		urgency = "MODERATE - Plenty of room, but be mindful of large reads."
	default:
		urgency = "LOW - Context is mostly free."
	}

	// Token counts shown here are the real tokenizer values when KoboldCpp
	// extras are available; otherwise they are a 4-chars-per-token heuristic
	// that under-counts code/JSON. Reflect that in the label.
	tokenLabel := "~"
	if te.kobold.Detected() {
		tokenLabel = ""
	}

	out := fmt.Sprintf("Context window status:\n  Used:      %s%d tokens\n  Maximum:   %d tokens\n  Remaining: %s%d tokens\n  Usage:     %.1f%%\n  Urgency:   %s",
		tokenLabel, used, max, tokenLabel, remaining, pct, urgency)

	// Append backend perf info when available. Bounded by a short timeout
	// so a slow backend never wedges the tool call.
	if te.kobold.Detected() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if perf, err := te.kobold.Perf(ctx); err == nil && perf != nil {
			out += fmt.Sprintf("\n\nBackend (KoboldCpp %s):\n  Last call:  %d in / %d out tokens, %.1fs process + %.1fs eval (%.0f tok/s eval)\n  Last stop:  %s\n  Total gens: %d, queue: %d, idle: %s, uptime: %s",
				te.kobold.Version(),
				perf.LastInputCount, perf.LastTokenCount,
				perf.LastProcessTime, perf.LastEvalTime, perf.LastEvalSpd,
				stopReasonString(perf.StopReason),
				perf.TotalGens, perf.Queue,
				idleStateString(perf.Idle),
				formatUptime(perf.Uptime),
			)
		}
	}

	return out
}

// idleStateString translates the integer idle field into a human label.
// KoboldCpp uses 0=busy, 1=idle (and other values for queued states).
func idleStateString(state int) string {
	switch state {
	case 0:
		return "busy"
	case 1:
		return "idle"
	default:
		return fmt.Sprintf("state %d", state)
	}
}

// formatUptime renders a seconds count as a short human-readable string.
func formatUptime(seconds int) string {
	d := time.Duration(seconds) * time.Second
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

func (te *ToolExecutor) sendMessage(argsJSON string) string {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Text == "" {
		return "Error: text is required"
	}

	if te.telegram == nil {
		return "Error: messaging is not configured. No collaborator channel available."
	}

	if err := te.telegram.Send(args.Text); err != nil {
		return fmt.Sprintf("Error sending message: %s", err)
	}

	te.logger.Info("message sent to collaborator", "length", len(args.Text))
	return "Message sent to collaborator."
}

func (te *ToolExecutor) memoryEdit(argsJSON string) string {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}
	if args.OldString == "" {
		return "Error: old_string is required"
	}

	count, err := te.memory.Edit(args.Path, args.OldString, args.NewString, args.ReplaceAll)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Update search index with new content
	content, readErr := te.memory.Read(args.Path)
	if readErr == nil {
		te.index.Update(indexKey(args.Path), content)
	}

	te.logger.Info("memory edited", "path", args.Path, "replacements", count)
	if count == 1 {
		return fmt.Sprintf("Replaced 1 occurrence in %s.", args.Path)
	}
	return fmt.Sprintf("Replaced %d occurrences in %s.", count, args.Path)
}

func (te *ToolExecutor) memoryAppend(argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Content == "" {
		return "Error: content is required"
	}

	if err := te.memory.Append(args.Path, args.Content); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Update search index with new content
	content, readErr := te.memory.Read(args.Path)
	if readErr == nil {
		te.index.Update(indexKey(args.Path), content)
	}

	te.logger.Info("memory appended", "path", args.Path, "size", len(args.Content))
	return fmt.Sprintf("Appended %d bytes to %s.", len(args.Content), args.Path)
}

func (te *ToolExecutor) memoryInsert(argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Line < 1 {
		return "Error: line must be >= 1"
	}
	if args.Content == "" {
		return "Error: content is required"
	}

	newTotal, err := te.memory.Insert(args.Path, args.Line, args.Content)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Update search index with new content
	content, readErr := te.memory.Read(args.Path)
	if readErr == nil {
		te.index.Update(indexKey(args.Path), content)
	}

	te.logger.Info("memory insert", "path", args.Path, "at_line", args.Line)
	return fmt.Sprintf("Inserted content at line %d in %s. File now has %d lines.", args.Line, args.Path, newTotal)
}

// ---------------------------------------------------------------------------
// Sandbox tool handlers
// ---------------------------------------------------------------------------

func (te *ToolExecutor) sandboxWrite(argsJSON string) string {
	var args struct {
		Folder   string `json:"folder"`
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if err := te.sandbox.WriteFile(args.Folder, args.Filename, args.Content); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Successfully wrote %d bytes to %s/%s", len(args.Content), args.Folder, args.Filename)
}

func (te *ToolExecutor) sandboxRead(argsJSON string) string {
	var args struct {
		Folder   string `json:"folder"`
		Filename string `json:"filename"`
		Offset   int    `json:"offset"`
		Lines    int    `json:"lines"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	content, err := te.sandbox.ReadFile(args.Folder, args.Filename, args.Offset, args.Lines)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return content
}

func (te *ToolExecutor) sandboxDelete(argsJSON string) string {
	var args struct {
		Folder   string `json:"folder"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if err := te.sandbox.DeleteFile(args.Folder, args.Filename); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Deleted %s/%s", args.Folder, args.Filename)
}

func (te *ToolExecutor) sandboxRename(argsJSON string) string {
	var args struct {
		Folder  string `json:"folder"`
		OldName string `json:"old_name"`
		NewName string `json:"new_name"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if err := te.sandbox.RenameFile(args.Folder, args.OldName, args.NewName); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Renamed %s/%s to %s/%s", args.Folder, args.OldName, args.Folder, args.NewName)
}

func (te *ToolExecutor) sandboxList(argsJSON string) string {
	var args struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	files, err := te.sandbox.ListFiles(args.Folder)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if len(files) == 0 {
		return fmt.Sprintf("%s/ is empty.", args.Folder)
	}
	var sb strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sb, "  %s (%s, modified %s)\n",
			f.Name, formatBytes(f.Size), f.ModTime.Format("2006-01-02 15:04"))
	}
	return sb.String()
}

func (te *ToolExecutor) sandboxEdit(argsJSON string) string {
	var args struct {
		Folder     string `json:"folder"`
		Filename   string `json:"filename"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if args.OldString == "" {
		return "Error: old_string is required"
	}
	count, err := te.sandbox.EditFile(args.Folder, args.Filename, args.OldString, args.NewString, args.ReplaceAll)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	if count == 1 {
		return fmt.Sprintf("Replaced 1 occurrence in %s/%s.", args.Folder, args.Filename)
	}
	return fmt.Sprintf("Replaced %d occurrences in %s/%s.", count, args.Folder, args.Filename)
}

func (te *ToolExecutor) sandboxAppend(argsJSON string) string {
	var args struct {
		Folder   string `json:"folder"`
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if args.Content == "" {
		return "Error: content is required"
	}
	if err := te.sandbox.AppendFile(args.Folder, args.Filename, args.Content); err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Appended %d bytes to %s/%s.", len(args.Content), args.Folder, args.Filename)
}

func (te *ToolExecutor) sandboxInsert(argsJSON string) string {
	var args struct {
		Folder   string `json:"folder"`
		Filename string `json:"filename"`
		Line     int    `json:"line"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	if args.Line < 1 {
		return "Error: line must be >= 1"
	}
	if args.Content == "" {
		return "Error: content is required"
	}
	newTotal, err := te.sandbox.InsertFile(args.Folder, args.Filename, args.Line, args.Content)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Inserted content at line %d in %s/%s. File now has %d lines.", args.Line, args.Folder, args.Filename, newTotal)
}

func (te *ToolExecutor) sandboxExecute(ctx context.Context, argsJSON string) string {
	var args struct {
		Script string   `json:"script"`
		Args   []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	output, err := te.sandbox.Execute(ctx, args.Script, args.Args)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return output
}

func (te *ToolExecutor) sandboxInstallPackage(ctx context.Context, argsJSON string) string {
	var args struct {
		Package string `json:"package"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	output, err := te.sandbox.InstallPackage(ctx, args.Package)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Package installed successfully.\n%s", output)
}

func (te *ToolExecutor) sandboxUpgradePackage(ctx context.Context, argsJSON string) string {
	var args struct {
		Package string `json:"package"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	output, err := te.sandbox.UpgradePackage(ctx, args.Package)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Package upgraded successfully.\n%s", output)
}

func (te *ToolExecutor) sandboxRemovePackage(ctx context.Context, argsJSON string) string {
	var args struct {
		Package string `json:"package"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	output, err := te.sandbox.RemovePackage(ctx, args.Package)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return fmt.Sprintf("Package removed successfully.\n%s", output)
}

func (te *ToolExecutor) sandboxListPackages() string {
	if te.sandbox == nil {
		return "Error: sandbox is not enabled"
	}
	output, err := te.sandbox.ListPackages()
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return output
}

// ---------------------------------------------------------------------------
// HTML-to-markdown extraction
// ---------------------------------------------------------------------------

// htmlExtractor walks an HTML node tree and produces clean markdown text.
type htmlExtractor struct {
	sb        strings.Builder
	baseURL   *url.URL
	listStack []listContext
	inPre     bool
	cellIndex int
}

type listContext struct {
	ordered bool
	index   int
}

// extractTextFromHTML parses HTML and renders it as readable markdown text.
// baseURL is used to resolve relative links.
func extractTextFromHTML(rawHTML, baseURL string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return basicStripTags(rawHTML)
	}

	base, _ := url.Parse(baseURL)

	root := findContentRoot(doc)
	ext := &htmlExtractor{baseURL: base}
	ext.walkChildren(root)

	return cleanupText(ext.sb.String())
}

// findContentRoot returns the best content node: <main>, then a sole
// <article>, then <body>, then the document itself.
func findContentRoot(doc *html.Node) *html.Node {
	if n := findElement(doc, "main"); n != nil {
		return n
	}
	articles := findElements(doc, "article")
	if len(articles) == 1 {
		return articles[0]
	}
	if n := findElement(doc, "body"); n != nil {
		return n
	}
	return doc
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, tag); found != nil {
			return found
		}
	}
	return nil
}

func findElements(n *html.Node, tag string) []*html.Node {
	var results []*html.Node
	if n.Type == html.ElementNode && n.Data == tag {
		results = append(results, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		results = append(results, findElements(c, tag)...)
	}
	return results
}

func (e *htmlExtractor) walk(n *html.Node) {
	if n.Type == html.ElementNode {
		// Skip noise elements
		switch n.Data {
		case "script", "style", "noscript", "nav", "footer", "aside",
			"form", "svg", "iframe", "button", "input", "select", "textarea":
			return
		}
		for _, attr := range n.Attr {
			if attr.Key == "hidden" {
				return
			}
			if attr.Key == "aria-hidden" && attr.Val == "true" {
				return
			}
			if attr.Key == "style" && strings.Contains(attr.Val, "display:none") {
				return
			}
		}
	}

	// Text nodes
	if n.Type == html.TextNode {
		text := n.Data
		if e.inPre {
			e.sb.WriteString(text)
		} else {
			e.sb.WriteString(collapseWhitespace(text))
		}
		return
	}

	if n.Type != html.ElementNode {
		e.walkChildren(n)
		return
	}

	// Element handling
	switch n.Data {
	// --- Headings ---
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.Data[1] - '0')
		e.sb.WriteString("\n\n")
		e.sb.WriteString(strings.Repeat("#", level))
		e.sb.WriteString(" ")
		e.walkChildren(n)
		e.sb.WriteString("\n\n")

	// --- Block elements ---
	case "p":
		e.sb.WriteString("\n\n")
		e.walkChildren(n)
		e.sb.WriteString("\n\n")
	case "div", "article", "section", "main", "header", "figure":
		e.sb.WriteString("\n")
		e.walkChildren(n)
		e.sb.WriteString("\n")
	case "figcaption":
		e.sb.WriteString("\n*")
		e.walkChildren(n)
		e.sb.WriteString("*\n")

	// --- Blockquote ---
	case "blockquote":
		inner := &htmlExtractor{baseURL: e.baseURL}
		inner.walkChildren(n)
		text := strings.TrimSpace(inner.sb.String())
		e.sb.WriteString("\n\n")
		for _, line := range strings.Split(text, "\n") {
			e.sb.WriteString("> ")
			e.sb.WriteString(line)
			e.sb.WriteString("\n")
		}
		e.sb.WriteString("\n")

	// --- Preformatted / code ---
	case "pre":
		e.sb.WriteString("\n\n```\n")
		e.inPre = true
		e.walkChildren(n)
		e.inPre = false
		e.sb.WriteString("\n```\n\n")
	case "code":
		if !e.inPre {
			e.sb.WriteString("`")
			e.walkChildren(n)
			e.sb.WriteString("`")
		} else {
			e.walkChildren(n)
		}

	// --- Links ---
	case "a":
		href := getAttr(n, "href")
		text := nodeText(n)
		if strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
			href = ""
		}
		if href != "" && text != "" {
			e.sb.WriteString("[")
			e.sb.WriteString(text)
			e.sb.WriteString("](")
			e.sb.WriteString(e.resolveHref(href))
			e.sb.WriteString(")")
		} else if text != "" {
			e.sb.WriteString(text)
		}

	// --- Inline formatting ---
	case "strong", "b":
		e.sb.WriteString("**")
		e.walkChildren(n)
		e.sb.WriteString("**")
	case "em", "i":
		e.sb.WriteString("*")
		e.walkChildren(n)
		e.sb.WriteString("*")
	case "del", "s":
		e.sb.WriteString("~~")
		e.walkChildren(n)
		e.sb.WriteString("~~")

	// --- Line break / horizontal rule ---
	case "br":
		e.sb.WriteString("\n")
	case "hr":
		e.sb.WriteString("\n\n---\n\n")

	// --- Images ---
	case "img":
		alt := getAttr(n, "alt")
		if alt != "" {
			e.sb.WriteString("[image: ")
			e.sb.WriteString(alt)
			e.sb.WriteString("]")
		}

	// --- Lists ---
	case "ul":
		e.sb.WriteString("\n")
		e.listStack = append(e.listStack, listContext{ordered: false})
		e.walkChildren(n)
		e.listStack = e.listStack[:len(e.listStack)-1]
		e.sb.WriteString("\n")
	case "ol":
		e.sb.WriteString("\n")
		e.listStack = append(e.listStack, listContext{ordered: true, index: 0})
		e.walkChildren(n)
		e.listStack = e.listStack[:len(e.listStack)-1]
		e.sb.WriteString("\n")
	case "li":
		depth := len(e.listStack)
		indent := ""
		if depth > 1 {
			indent = strings.Repeat("  ", depth-1)
		}
		if depth > 0 {
			ctx := &e.listStack[depth-1]
			if ctx.ordered {
				ctx.index++
				fmt.Fprintf(&e.sb, "\n%s%d. ", indent, ctx.index)
			} else {
				fmt.Fprintf(&e.sb, "\n%s- ", indent)
			}
		} else {
			e.sb.WriteString("\n- ")
		}
		e.walkChildren(n)

	// --- Definition lists ---
	case "dt":
		e.sb.WriteString("\n**")
		e.walkChildren(n)
		e.sb.WriteString("**")
	case "dd":
		e.sb.WriteString("\n: ")
		e.walkChildren(n)

	// --- Tables ---
	case "table":
		e.sb.WriteString("\n\n")
		e.walkChildren(n)
		e.sb.WriteString("\n")
	case "thead", "tbody", "tfoot":
		e.walkChildren(n)
	case "tr":
		e.cellIndex = 0
		e.sb.WriteString("| ")
		e.walkChildren(n)
		e.sb.WriteString("|\n")
	case "td", "th":
		if e.cellIndex > 0 {
			e.sb.WriteString(" | ")
		}
		e.cellIndex++
		e.walkChildren(n)
		e.sb.WriteString(" ")

	default:
		e.walkChildren(n)
	}
}

func (e *htmlExtractor) walkChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walk(c)
	}
}

func (e *htmlExtractor) resolveHref(href string) string {
	if e.baseURL == nil {
		return href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	return e.baseURL.ResolveReference(parsed).String()
}

// getAttr returns the value of an attribute on an HTML element node.
func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// nodeText extracts plain text from a node tree, collapsing whitespace.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	nodeTextWalk(&sb, n)
	return strings.TrimSpace(collapseWhitespace(sb.String()))
}

func nodeTextWalk(sb *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		nodeTextWalk(sb, c)
	}
}

// collapseWhitespace replaces runs of whitespace with a single space.
func collapseWhitespace(s string) string {
	var sb strings.Builder
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			if !inSpace {
				sb.WriteRune(' ')
				inSpace = true
			}
		} else {
			sb.WriteRune(r)
			inSpace = false
		}
	}
	return sb.String()
}

// cleanupText normalizes whitespace in the final output: trims lines,
// collapses runs of blank lines to at most two, trims overall.
func cleanupText(text string) string {
	lines := strings.Split(text, "\n")
	var cleaned []string
	blankRun := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blankRun++
			if blankRun <= 2 {
				cleaned = append(cleaned, "")
			}
		} else {
			blankRun = 0
			cleaned = append(cleaned, line)
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

// basicStripTags is a fallback HTML tag remover used when parsing fails.
func basicStripTags(s string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			sb.WriteRune(' ')
			continue
		}
		if !inTag {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
