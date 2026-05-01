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
