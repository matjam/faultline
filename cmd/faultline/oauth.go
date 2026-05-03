package main

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/config"
)

var oauthCallbackPageTemplate = template.Must(template.New("oauth-callback").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>{{.Title}} :: faultline</title>
</head>
<body>
  <main>
    <h1>{{.Title}}</h1>
    <p>{{.Message}}</p>
    <p>You can close this tab and return to Telegram.</p>
  </main>
</body>
</html>`))

type oauthCallbackServer struct {
	cfg    config.OAuthConfig
	oauth  oauthCompleter
	logger *slog.Logger

	srv      *http.Server
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type oauthCompleter interface {
	Complete(ctx context.Context, state, code string) (mcp.OAuthCompleteResult, error)
}

func buildOAuthCallbackServer(cfg config.OAuthConfig, oauth oauthCompleter, logger *slog.Logger) (*oauthCallbackServer, error) {
	if !cfg.Active() {
		logger.Info("oauth callback server disabled")
		return nil, nil
	}
	if oauth == nil {
		return nil, fmt.Errorf("oauth manager is required")
	}
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1:8743"
	}
	if cfg.CallbackPath == "" {
		cfg.CallbackPath = "/oauth/callback"
	}
	return &oauthCallbackServer{cfg: cfg, oauth: oauth, logger: logger}, nil
}

func (s *oauthCallbackServer) Start(ctx context.Context) {
	if s == nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+s.cfg.CallbackPath, s.handleCallback)
	mux.HandleFunc("GET /oauth/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.srv = &http.Server{
		Addr:              s.cfg.Bind,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("oauth callback server listening",
			"bind", s.cfg.Bind,
			"public_base_url", s.cfg.PublicBaseURL,
			"callback_path", s.cfg.CallbackPath)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("oauth callback server failed", "error", err)
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		s.shutdown()
	}()
}

func (s *oauthCallbackServer) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

func (s *oauthCallbackServer) Shutdown() {
	if s != nil {
		s.shutdown()
	}
}

func (s *oauthCallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		writeOAuthCallbackPage(w, http.StatusBadRequest, "OAuth authorization failed", oauthErr)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	result, err := s.oauth.Complete(r.Context(), state, code)
	if err != nil {
		writeOAuthCallbackPage(w, http.StatusBadRequest, "OAuth authorization failed", err.Error())
		return
	}
	writeOAuthCallbackPage(w, http.StatusOK, "OAuth authorization complete", fmt.Sprintf("%s is %s.", result.ServerName, result.Status))
}

func (s *oauthCallbackServer) shutdown() {
	s.stopOnce.Do(func() {
		if s.srv == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(ctx); err != nil {
			s.logger.Warn("oauth callback server shutdown error", "error", err)
		}
	})
}

func writeOAuthCallbackPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = oauthCallbackPageTemplate.Execute(w, struct {
		Title   string
		Message string
	}{Title: title, Message: message})
}
