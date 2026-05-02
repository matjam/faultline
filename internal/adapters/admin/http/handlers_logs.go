package adminhttp

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logsPage is the shell template's data; the live tail is loaded via
// HTMX into frag_logs.html.
type logsPage struct {
	pageData
	LogPath string
}

// fragLogsData backs frag_logs.html. Lines is the most recent N
// entries from today's log file, classified by severity for terminal-
// style coloring.
type fragLogsData struct {
	Today string
	Lines []logLine
	Empty bool
	Err   string
}

type logLine struct {
	Level string
	Text  string
}

// logTailLines is how many trailing lines we read on each poll.
const logTailLines = 400

func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	data := logsPage{
		pageData: s.basePageData(r, "logs"),
		LogPath:  s.todayLogPath(),
	}
	s.render(w, "logs.html", data)
}

func (s *Server) handleFragLogs(w http.ResponseWriter, _ *http.Request) {
	data := fragLogsData{Today: time.Now().Format("2006-01-02")}

	path := s.todayLogPath()
	if path == "" {
		data.Err = "log directory not configured"
		s.renderFragment(w, "frag_logs.html", data)
		return
	}

	lines, err := tailFile(path, logTailLines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			data.Empty = true
		} else {
			data.Err = err.Error()
		}
		s.renderFragment(w, "frag_logs.html", data)
		return
	}
	if len(lines) == 0 {
		data.Empty = true
	}
	data.Lines = make([]logLine, 0, len(lines))
	for _, ln := range lines {
		data.Lines = append(data.Lines, logLine{
			Level: classifyLogLevel(ln),
			Text:  ln,
		})
	}
	s.renderFragment(w, "frag_logs.html", data)
}

// todayLogPath returns the configured directory's today's log file
// path, or "" when no LogDir was wired.
func (s *Server) todayLogPath() string {
	dir := s.deps.LogDir
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, time.Now().Format("2006-01-02")+".log")
}

// tailFile returns the last n lines of path. We read the whole file
// and slice — Faultline's daily-rotated logs are typically a few MiB
// at most, well within the budget for a 2-second poll.
//
// For very large files the operator can rotate sooner by tightening
// the log level; if this becomes a real issue we can swap in a
// reverse-chunked reader.
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path derived from operator config
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Cap the read at ~2 MiB so a runaway log doesn't blow the
	// poll up. Read from the end of the file.
	const maxBytes = 2 * 1024 * 1024
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	// Drop the (likely partial) first line when we seeked past
	// the start.
	if start > 0 && scanner.Scan() {
		_ = scanner.Text()
	}

	// Ring of last n lines.
	ring := make([]string, n)
	count := 0
	for scanner.Scan() {
		ring[count%n] = scanner.Text()
		count++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if count <= n {
		return ring[:count], nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ring[(count+i)%n])
	}
	return out, nil
}

// classifyLogLevel returns "error" / "warn" / "info" / "debug" by
// scanning for slog's typical level=KEY pattern. Falls back to "info".
func classifyLogLevel(line string) string {
	switch {
	case strings.Contains(line, "level=ERROR"):
		return "error"
	case strings.Contains(line, "level=WARN"):
		return "warn"
	case strings.Contains(line, "level=DEBUG"):
		return "debug"
	default:
		return "info"
	}
}
