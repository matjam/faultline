package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// StdioClient is the command-based MCP client. It implements Caller.
type StdioClient struct {
	servers     map[string]ServerConfig
	runner      StdioRunner
	idleTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*stdioSession
	closed   bool
}

const (
	defaultStdioIdleTimeout = 10 * time.Minute
	stdioCloseTimeout       = 2 * time.Second
	stderrTailLimit         = 32 * 1024
)

// StdioCommand is a container process request for a stdio MCP server.
type StdioCommand struct {
	Name    string
	Command string
	Args    []string
	WorkDir string
	Env     map[string]string
}

// StdioRunner starts an interactive process for a stdio MCP server.
type StdioRunner interface {
	Start(ctx context.Context, cmd StdioCommand) (StdioProcess, error)
}

// StdioProcess is an interactive stdin/stdout process.
type StdioProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
}

// NewStdioClient returns a client for command-based MCP servers.
func NewStdioClient(servers []ServerConfig, runner StdioRunner, idleTimeout time.Duration) *StdioClient {
	if idleTimeout <= 0 {
		idleTimeout = defaultStdioIdleTimeout
	}
	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	return &StdioClient{
		servers:     byName,
		runner:      runner,
		idleTimeout: idleTimeout,
		sessions:    make(map[string]*stdioSession),
	}
}

// Close stops all live stdio MCP sessions.
func (c *StdioClient) Close() error {
	c.mu.Lock()
	c.closed = true
	sessions := make([]*stdioSession, 0, len(c.sessions))
	for _, session := range c.sessions {
		sessions = append(sessions, session)
	}
	c.sessions = make(map[string]*stdioSession)
	c.mu.Unlock()

	var firstErr error
	for _, session := range sessions {
		if err := session.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Discover runs tools/list for one configured stdio server.
func (c *StdioClient) Discover(ctx context.Context, serverName string) (DiscoveredServer, error) {
	server, err := c.server(serverName)
	if err != nil {
		return DiscoveredServer{}, err
	}

	result, err := c.call(ctx, server, "tools/list", nil)
	if err != nil {
		return DiscoveredServer{}, err
	}

	var parsed struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			InputSchema any    `json:"inputSchema,omitempty"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return DiscoveredServer{}, fmt.Errorf("parse tools/list result: %w", err)
	}

	tools := make([]DiscoveredTool, 0, len(parsed.Tools))
	for _, tool := range parsed.Tools {
		tools = append(tools, DiscoveredTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return DiscoveredServer{Server: server, Tools: tools}, nil
}

// CallTool runs tools/call for one configured stdio server.
func (c *StdioClient) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
	server, err := c.server(serverName)
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	params, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("marshal tools/call params: %w", err)
	}

	result, err := c.call(ctx, server, "tools/call", params)
	if err != nil {
		return "", err
	}
	return formatCallResult(result)
}

func (c *StdioClient) server(name string) (ServerConfig, error) {
	server, ok := c.servers[name]
	if !ok {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not configured", name)
	}
	if server.Transport != "stdio" {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not a stdio server", name)
	}
	return server, nil
}

func (c *StdioClient) call(ctx context.Context, server ServerConfig, method string, params json.RawMessage) (json.RawMessage, error) {
	session, err := c.session(ctx, server)
	if err != nil {
		return nil, err
	}
	result, err := session.call(ctx, method, params)
	if err != nil {
		c.dropSession(server.Name, session)
		return nil, err
	}
	return result, nil
}

func (c *StdioClient) session(ctx context.Context, server ServerConfig) (*stdioSession, error) {
	if c.runner == nil {
		return nil, fmt.Errorf("stdio MCP requires sandbox execution, but no stdio runner is configured")
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("stdio MCP client is closed")
	}
	if session := c.sessions[server.Name]; session != nil {
		if !session.doneClosed() {
			c.mu.Unlock()
			return session, nil
		}
		delete(c.sessions, server.Name)
	}
	proc, err := c.runner.Start(ctx, StdioCommand{
		Name:    server.Name,
		Command: server.Command,
		Args:    append([]string(nil), server.Args...),
		WorkDir: server.SandboxWorkDir(),
		Env:     cloneStringMap(server.Env),
	})
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("start mcp server: %w", err)
	}

	session := newStdioSession(proc, c.idleTimeout)
	c.sessions[server.Name] = session
	c.mu.Unlock()
	return session, nil
}

func (c *StdioClient) dropSession(name string, session *stdioSession) {
	c.mu.Lock()
	if c.sessions[name] == session {
		delete(c.sessions, name)
	}
	c.mu.Unlock()
	_ = session.Close()
}

type stdioRequest struct {
	id     int
	method string
	params json.RawMessage
	resp   chan stdioResponse
}

type stdioResponse struct {
	result json.RawMessage
	err    error
}

type stdioSession struct {
	proc        StdioProcess
	stdin       io.WriteCloser
	reader      *bufio.Reader
	requests    chan stdioRequest
	stop        chan struct{}
	ready       chan struct{}
	done        chan struct{}
	nextID      atomic.Int64
	idleTimeout time.Duration
	stderr      *stderrTail
	stderrDone  chan struct{}

	initErr      error
	closeOnce    sync.Once
	processOnce  sync.Once
	processError error
}

func newStdioSession(proc StdioProcess, idleTimeout time.Duration) *stdioSession {
	session := &stdioSession{
		proc:        proc,
		stdin:       proc.Stdin(),
		reader:      bufio.NewReader(proc.Stdout()),
		requests:    make(chan stdioRequest),
		stop:        make(chan struct{}),
		ready:       make(chan struct{}),
		done:        make(chan struct{}),
		idleTimeout: idleTimeout,
		stderr:      &stderrTail{limit: stderrTailLimit},
		stderrDone:  make(chan struct{}),
	}
	session.nextID.Store(1)
	go func() {
		defer close(session.stderrDone)
		_, _ = io.Copy(session.stderr, proc.Stderr())
	}()
	go session.run()
	return session
}

func (s *stdioSession) run() {
	defer close(s.done)
	if err := s.initialize(); err != nil {
		_ = s.closeProcess()
		s.initErr = s.withStderr(err)
		close(s.ready)
		return
	}
	close(s.ready)

	idle := time.NewTimer(s.idleTimeout)
	defer idle.Stop()
	for {
		select {
		case req := <-s.requests:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			result, err := s.writeAndRead(req.id, req.method, req.params)
			if err != nil {
				_ = s.closeProcess()
				err = s.withStderr(err)
			}
			req.resp <- stdioResponse{result: result, err: err}
			if err != nil {
				return
			}
			idle.Reset(s.idleTimeout)
		case <-idle.C:
			_ = s.closeProcess()
			return
		case <-s.stop:
			_ = s.closeProcess()
			return
		}
	}
}

func (s *stdioSession) initialize() error {
	if err := writeJSONLine(s.stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"` + defaultProtocolVersion + `","capabilities":{},"clientInfo":{"name":"faultline","version":"dev"}}`),
	}); err != nil {
		return err
	}
	if _, err := readResponse(s.reader, 1); err != nil {
		return err
	}

	if err := writeJSONLine(s.stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}); err != nil {
		return err
	}
	return nil
}

func (s *stdioSession) doneClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *stdioSession) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	select {
	case <-s.ready:
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}
	if s.initErr != nil {
		return nil, s.initErr
	}

	req := stdioRequest{
		id:     int(s.nextID.Add(1)),
		method: method,
		params: params,
		resp:   make(chan stdioResponse, 1),
	}
	select {
	case s.requests <- req:
	case <-s.done:
		return nil, fmt.Errorf("stdio MCP session closed")
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}

	select {
	case resp := <-req.resp:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-s.done:
		return nil, fmt.Errorf("stdio MCP session closed")
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}
}

func (s *stdioSession) writeAndRead(id int, method string, params json.RawMessage) (json.RawMessage, error) {
	if err := writeJSONLine(s.stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}); err != nil {
		return nil, err
	}
	result, err := readResponse(s.reader, id)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *stdioSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
	})
	err := s.closeProcess()
	select {
	case <-s.done:
	case <-time.After(stdioCloseTimeout):
	}
	return err
}

func (s *stdioSession) closeProcess() error {
	s.processOnce.Do(func() {
		_ = s.stdin.Close()
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- s.proc.Wait()
		}()
		select {
		case s.processError = <-waitDone:
		case <-time.After(stdioCloseTimeout):
			if killer, ok := s.proc.(interface{ Kill() error }); ok {
				_ = killer.Kill()
				s.processError = <-waitDone
			} else {
				s.processError = fmt.Errorf("stdio MCP process did not exit after %s", stdioCloseTimeout)
			}
		}
		select {
		case <-s.stderrDone:
		case <-time.After(100 * time.Millisecond):
		}
	})
	return s.processError
}

func (s *stdioSession) withStderr(err error) error {
	if err == nil {
		return nil
	}
	if stderr := s.stderr.String(); stderr != "" {
		return fmt.Errorf("%w: %s", err, stderr)
	}
	return err
}

type stderrTail struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := len(p)
	_, _ = t.buf.Write(p)
	if t.limit > 0 && t.buf.Len() > t.limit {
		data := t.buf.Bytes()
		trimmed := append([]byte(nil), data[len(data)-t.limit:]...)
		t.buf.Reset()
		_, _ = t.buf.Write(trimmed)
	}
	return n, nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func writeJSONLine(w io.Writer, req jsonRPCRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal json-rpc request: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write mcp request: %w", err)
	}
	return nil
}

func readResponse(r *bufio.Reader, id int) (json.RawMessage, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read mcp response: %w", err)
		}

		var rpcResp jsonRPCResponse
		if err := json.Unmarshal(line, &rpcResp); err != nil {
			return nil, fmt.Errorf("parse json-rpc response: %w", err)
		}
		if rpcResp.ID != id {
			continue
		}
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}
		return rpcResp.Result, nil
	}
}
