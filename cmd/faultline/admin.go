// Admin UI composition. Splits the admin-server wiring out of
// main.go so the composition root stays focused on the agent loop;
// only enabled when [admin] is configured.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	adminhttp "github.com/matjam/faultline/internal/adapters/admin/http"
	"github.com/matjam/faultline/internal/adapters/auth/users"
	"github.com/matjam/faultline/internal/config"
)

// adminServer bundles the admin-side state that needs to outlive the
// agent loop's request boundary. Returned from buildAdmin; its Run
// method is goroutine-safe and shuts down on parent-context cancel.
type adminServer struct {
	srv      *adminhttp.Server
	sessions *users.SessionStore

	logger *slog.Logger
	wg     sync.WaitGroup
}

// buildAdmin constructs the admin HTTP server, the user store, and
// the session store. Returns nil if the feature is disabled. Errors
// here are fatal: misconfigured admin should not silently degrade
// to "agent runs without admin"; the operator asked for admin and
// deserves to know it failed.
func buildAdmin(ctx context.Context, cfg *config.Config, startedAt time.Time, logger *slog.Logger) (*adminServer, error) {
	if !cfg.Admin.Active() {
		logger.Info("admin UI disabled")
		return nil, nil
	}

	store, boot, err := users.New(cfg.Admin.UsersFile)
	if err != nil {
		return nil, fmt.Errorf("admin users store: %w", err)
	}
	if boot != nil {
		// First-run bootstrap path. Surface the plaintext password
		// loudly on the operator's stderr stream; the file itself
		// also carries the same line as a comment for operators
		// who missed the log.
		logger.Warn(
			"admin UI bootstrapped a new admin user; this password is shown ONCE",
			"username", boot.Username,
			"password", boot.Password,
			"users_file", cfg.Admin.UsersFile,
		)
	}

	sessions := users.NewSessionStore(ctx, cfg.Admin.SessionTTL.Duration())

	srv, err := adminhttp.New(adminhttp.Deps{
		Bind:      cfg.Admin.Bind,
		Users:     store,
		Sessions:  sessions,
		StartedAt: startedAt,
		Logger:    logger,
	})
	if err != nil {
		sessions.Close()
		return nil, fmt.Errorf("admin server: %w", err)
	}

	logger.Info("admin UI enabled",
		"bind", cfg.Admin.Bind,
		"users_file", cfg.Admin.UsersFile,
		"session_ttl", cfg.Admin.SessionTTL.Duration())

	return &adminServer{srv: srv, sessions: sessions, logger: logger}, nil
}

// Start spawns the admin server in a goroutine. Run blocks until
// ctx is canceled or the server stops on its own. Any non-graceful
// error is logged but not propagated — the agent loop is the
// authority on process exit.
func (a *adminServer) Start(ctx context.Context) {
	if a == nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.srv.Run(ctx); err != nil {
			a.logger.Error("admin server stopped with error", "error", err)
			return
		}
		a.logger.Info("admin server stopped")
	}()
}

// Wait blocks until the admin server goroutine has exited. Should
// be called after the parent context is canceled (or shutdownCh
// is closed) so the server has a reason to stop.
func (a *adminServer) Wait() {
	if a == nil {
		return
	}
	a.wg.Wait()
}

// Close releases the session store. Shutdown of the HTTP server is
// driven by the parent context; calling Close after Wait is the
// clean order.
func (a *adminServer) Close() {
	if a == nil {
		return
	}
	a.sessions.Close()
}
