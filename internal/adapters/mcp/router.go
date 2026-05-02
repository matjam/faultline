package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Router delegates calls to the transport-specific caller for a server.
type Router struct {
	callers map[string]Caller
}

type stdioRestarter interface {
	Restart(ctx context.Context, serverName string) (DiscoveredServer, error)
}

// NewRouter creates a caller that can route by server name.
func NewRouter() *Router {
	return &Router{callers: make(map[string]Caller)}
}

// Add registers the caller for a server name.
func (r *Router) Add(serverName string, caller Caller) {
	r.callers[serverName] = caller
}

// CallTool delegates to the server's registered transport client.
func (r *Router) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
	caller, ok := r.callers[serverName]
	if !ok {
		return "", fmt.Errorf("mcp server %q is not configured", serverName)
	}
	return caller.CallTool(ctx, serverName, toolName, args)
}

// RestartStdioServer restarts a single stdio server session when the registered
// caller supports that transport-specific lifecycle operation.
func (r *Router) RestartStdioServer(ctx context.Context, serverName string) (DiscoveredServer, error) {
	caller, ok := r.callers[serverName]
	if !ok {
		return DiscoveredServer{}, fmt.Errorf("mcp server %q is not configured", serverName)
	}
	restarter, ok := caller.(stdioRestarter)
	if !ok {
		return DiscoveredServer{}, fmt.Errorf("mcp server %q is not a stdio server", serverName)
	}
	return restarter.Restart(ctx, serverName)
}

// Close releases transport clients that hold long-lived resources.
func (r *Router) Close() error {
	var firstErr error
	closed := map[Caller]struct{}{}
	for _, caller := range r.callers {
		if _, ok := closed[caller]; ok {
			continue
		}
		closed[caller] = struct{}{}
		closer, ok := caller.(interface{ Close() error })
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
