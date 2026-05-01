package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/config"
)

func setupMCP(ctx context.Context, cfg config.MCPConfig, logger *slog.Logger) (mcp.Caller, []mcp.DiscoveredServer, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}

	mcpCfg, err := mcp.LoadConfig(cfg.ConfigFile)
	if err != nil {
		return nil, nil, err
	}

	var httpServers []mcp.ServerConfig
	var stdioServers []mcp.ServerConfig
	for _, server := range mcpCfg.Servers {
		switch server.Transport {
		case "http":
			httpServers = append(httpServers, server)
		case "stdio":
			stdioServers = append(stdioServers, server)
		}
	}

	router := mcp.NewRouter()
	var discovered []mcp.DiscoveredServer
	client := mcp.NewHTTPClient(httpServers, &http.Client{Timeout: 30 * time.Second})
	for _, server := range httpServers {
		router.Add(server.Name, client)
		entry, err := client.Discover(ctx, server.Name)
		if err != nil {
			logger.Warn("mcp discovery failed", "server", server.Name, "error", err)
			discovered = append(discovered, mcp.DiscoveredServer{Server: server, DiscoveryError: err.Error()})
			continue
		}
		discovered = append(discovered, entry)
	}

	stdioClient := mcp.NewStdioClient(stdioServers)
	for _, server := range stdioServers {
		router.Add(server.Name, stdioClient)
		entry, err := stdioClient.Discover(ctx, server.Name)
		if err != nil {
			logger.Warn("mcp discovery failed", "server", server.Name, "error", err)
			discovered = append(discovered, mcp.DiscoveredServer{Server: server, DiscoveryError: err.Error()})
			continue
		}
		discovered = append(discovered, entry)
	}

	logger.Info("mcp enabled", "servers", len(mcpCfg.Servers), "http_servers", len(httpServers), "stdio_servers", len(stdioServers), "discovered", len(discovered))
	return router, discovered, nil
}
