package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStdioClientDiscoverSendsToolsListRequest(t *testing.T) {
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/list")},
		&scriptedStdioRunner{wantMethod: "tools/list"},
		time.Minute,
	)
	defer client.Close()

	discovered, err := client.Discover(context.Background(), "local")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovered.Tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(discovered.Tools))
	}
	if discovered.Tools[0].Name != "local_search" {
		t.Fatalf("tool name = %q, want local_search", discovered.Tools[0].Name)
	}
}

func TestStdioClientCallToolSendsToolsCallRequest(t *testing.T) {
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/call")},
		&scriptedStdioRunner{wantMethod: "tools/call"},
		time.Minute,
	)
	defer client.Close()

	result, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "local results" {
		t.Fatalf("result = %q, want local results", result)
	}
}

func TestStdioClientReusesProcessAcrossToolCalls(t *testing.T) {
	runner := &statefulStdioRunner{}
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/call")},
		runner,
		time.Minute,
	)
	defer client.Close()

	first, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"one"}`))
	if err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	second, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"two"}`))
	if err != nil {
		t.Fatalf("second CallTool: %v", err)
	}

	if first != "call 1" || second != "call 2" {
		t.Fatalf("results = %q, %q; want call 1, call 2", first, second)
	}
	if got := runner.starts.Load(); got != 1 {
		t.Fatalf("runner starts = %d, want 1", got)
	}
}

func TestStdioClientClosesIdleSession(t *testing.T) {
	runner := &statefulStdioRunner{}
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/call")},
		runner,
		10*time.Millisecond,
	)
	defer client.Close()

	if _, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"one"}`)); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		return runner.closed.Load() == 1
	})
	if _, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"two"}`)); err != nil {
		t.Fatalf("second CallTool: %v", err)
	}

	if got := runner.starts.Load(); got != 2 {
		t.Fatalf("runner starts = %d, want 2", got)
	}
}

func TestStdioClientIncludesStderrWhenResponseEOF(t *testing.T) {
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/list")},
		failingStdioRunner{},
		time.Minute,
	)

	_, err := client.Discover(context.Background(), "local")
	if err == nil {
		t.Fatal("expected discovery error")
	}
	if !strings.Contains(err.Error(), "playwright missing browser") {
		t.Fatalf("error = %q, want stderr detail", err)
	}
}

func TestStdioClientIncludesRawStdoutWhenResponseIsNotJSON(t *testing.T) {
	client := NewStdioClient(
		[]ServerConfig{helperStdioServerConfig(t, "tools/list")},
		noisyStdoutStdioRunner{},
		time.Minute,
	)

	_, err := client.Discover(context.Background(), "local")
	if err == nil {
		t.Fatal("expected discovery error")
	}
	if !strings.Contains(err.Error(), `raw stdout line: "GitHub MCP Server"`) {
		t.Fatalf("error = %q, want raw stdout detail", err)
	}
}

func helperStdioServerConfig(t *testing.T, wantMethod string) ServerConfig {
	t.Helper()
	return ServerConfig{
		Name:      "local",
		Transport: "stdio",
		Command:   "local-mcp",
		WorkDir:   "/mcp/local",
		Env: map[string]string{
			"MCP_WANT_METHOD": wantMethod,
		},
	}
}

type noisyStdoutStdioRunner struct{}

func (noisyStdoutStdioRunner) Start(ctx context.Context, cmd StdioCommand) (StdioProcess, error) {
	return &failingStdioProcess{
		stdin:  nopWriteCloser{},
		stdout: strings.NewReader("GitHub MCP Server\n"),
		stderr: strings.NewReader(""),
	}, nil
}

type failingStdioRunner struct{}

func (failingStdioRunner) Start(ctx context.Context, cmd StdioCommand) (StdioProcess, error) {
	return &failingStdioProcess{
		stdin:  nopWriteCloser{},
		stdout: strings.NewReader(""),
		stderr: strings.NewReader("playwright missing browser\n"),
	}, nil
}

type failingStdioProcess struct {
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (p *failingStdioProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *failingStdioProcess) Stdout() io.Reader     { return p.stdout }
func (p *failingStdioProcess) Stderr() io.Reader     { return p.stderr }
func (p *failingStdioProcess) Wait() error           { return errors.New("exit status 1") }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

type scriptedStdioRunner struct {
	wantMethod string
}

func (r *scriptedStdioRunner) Start(ctx context.Context, cmd StdioCommand) (StdioProcess, error) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	proc := &scriptedStdioProcess{
		stdin:  stdinWriter,
		stdout: stdoutReader,
		stderr: strings.NewReader(""),
		done:   make(chan error, 1),
	}

	go func() {
		defer stdoutWriter.Close()
		defer stdinReader.Close()
		scanner := bufio.NewScanner(stdinReader)
		var methods []string
		for scanner.Scan() {
			var req jsonRPCRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				proc.done <- err
				return
			}
			methods = append(methods, req.Method)
			switch req.Method {
			case "initialize":
				var params struct {
					Capabilities map[string]any `json:"capabilities"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					proc.done <- err
					return
				}
				if params.Capabilities == nil {
					proc.done <- errors.New("initialize params missing capabilities")
					return
				}
				_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` + "\n"))
			case "notifications/initialized":
			case "tools/list":
				if r.wantMethod != "tools/list" {
					proc.done <- &unexpectedMethodError{method: req.Method}
					return
				}
				_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"local_search","description":"Search locally."}]}}` + "\n"))
			case "tools/call":
				if r.wantMethod != "tools/call" {
					proc.done <- &unexpectedMethodError{method: req.Method}
					return
				}
				var params struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					proc.done <- err
					return
				}
				if params.Name != "local_search" || string(params.Arguments) != `{"query":"faultline"}` {
					proc.done <- &unexpectedMethodError{method: req.Method}
					return
				}
				_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"local results"}]}}` + "\n"))
			default:
				proc.done <- &unexpectedMethodError{method: req.Method}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			proc.done <- err
			return
		}
		want := []string{"initialize", "notifications/initialized", r.wantMethod}
		if strings.Join(methods, ",") != strings.Join(want, ",") {
			proc.done <- &unexpectedMethodError{method: strings.Join(methods, ",")}
			return
		}
		proc.done <- nil
	}()

	return proc, nil
}

type scriptedStdioProcess struct {
	stdin     io.WriteCloser
	stdout    io.Reader
	stderr    io.Reader
	done      chan error
	waitOnce  sync.Once
	waitError error
}

func (p *scriptedStdioProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *scriptedStdioProcess) Stdout() io.Reader     { return p.stdout }
func (p *scriptedStdioProcess) Stderr() io.Reader     { return p.stderr }
func (p *scriptedStdioProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitError = <-p.done
	})
	return p.waitError
}

type unexpectedMethodError struct {
	method string
}

func (e *unexpectedMethodError) Error() string {
	return "unexpected method " + e.method
}

type statefulStdioRunner struct {
	starts atomic.Int32
	closed atomic.Int32
}

func (r *statefulStdioRunner) Start(ctx context.Context, cmd StdioCommand) (StdioProcess, error) {
	r.starts.Add(1)
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	proc := &statefulStdioProcess{
		stdin:  stdinWriter,
		stdout: stdoutReader,
		stderr: strings.NewReader(""),
		done:   make(chan error, 1),
		closed: &r.closed,
	}

	go func() {
		defer stdoutWriter.Close()
		defer stdinReader.Close()
		scanner := bufio.NewScanner(stdinReader)
		callCount := 0
		for scanner.Scan() {
			var req jsonRPCRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				proc.done <- err
				return
			}
			switch req.Method {
			case "initialize":
				_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` + "\n"))
			case "notifications/initialized":
			case "tools/call":
				callCount++
				response := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"call %d"}]}}`+"\n", req.ID, callCount)
				_, _ = stdoutWriter.Write([]byte(response))
			default:
				proc.done <- &unexpectedMethodError{method: req.Method}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			proc.done <- err
			return
		}
		proc.done <- nil
	}()

	return proc, nil
}

type statefulStdioProcess struct {
	stdin     io.WriteCloser
	stdout    io.Reader
	stderr    io.Reader
	done      chan error
	closed    *atomic.Int32
	waitOnce  sync.Once
	waitError error
}

func (p *statefulStdioProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *statefulStdioProcess) Stdout() io.Reader     { return p.stdout }
func (p *statefulStdioProcess) Stderr() io.Reader     { return p.stderr }
func (p *statefulStdioProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitError = <-p.done
		p.closed.Add(1)
	})
	return p.waitError
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestStdioMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	var methods []string
	for scanner.Scan() {
		var req jsonRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		methods = append(methods, req.Method)

		switch req.Method {
		case "initialize":
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` + "\n")
		case "notifications/initialized":
		case "tools/list":
			if os.Getenv("MCP_WANT_METHOD") != "tools/list" {
				t.Fatalf("unexpected tools/list for %q", os.Getenv("MCP_WANT_METHOD"))
			}
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"local_search","description":"Search locally."}]}}` + "\n")
		case "tools/call":
			if os.Getenv("MCP_WANT_METHOD") != "tools/call" {
				t.Fatalf("unexpected tools/call for %q", os.Getenv("MCP_WANT_METHOD"))
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal params: %v", err)
			}
			if params.Name != "local_search" {
				t.Fatalf("name = %q, want local_search", params.Name)
			}
			if string(params.Arguments) != `{"query":"faultline"}` {
				t.Fatalf("arguments = %s, want original arguments", params.Arguments)
			}
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"local results"}]}}` + "\n")
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan stdin: %v", err)
	}
	want := []string{"initialize", "notifications/initialized", os.Getenv("MCP_WANT_METHOD")}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	os.Exit(0)
}
