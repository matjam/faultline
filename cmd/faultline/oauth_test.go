package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/config"
)

func TestBuildOAuthCallbackServerDisabledWithoutPublicBaseURL(t *testing.T) {
	srv, err := buildOAuthCallbackServer(config.OAuthConfig{}, nil, silentOAuthTestLogger())
	if err != nil {
		t.Fatalf("buildOAuthCallbackServer: %v", err)
	}
	if srv != nil {
		t.Fatal("expected nil server when oauth public_base_url is empty")
	}
}

func TestOAuthCallbackServerCompletesWithoutAdmin(t *testing.T) {
	completer := &fakeOAuthCompleter{result: mcp.OAuthCompleteResult{ServerName: "coralogix", Status: "connected"}}
	srv, err := buildOAuthCallbackServer(config.OAuthConfig{
		Bind:          "127.0.0.1:0",
		PublicBaseURL: "http://127.0.0.1:0",
		CallbackPath:  "/oauth/callback",
	}, completer, silentOAuthTestLogger())
	if err != nil {
		t.Fatalf("buildOAuthCallbackServer: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/callback", srv.handleCallback)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/oauth/callback?state=state-123&code=code-456")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if completer.state != "state-123" || completer.code != "code-456" {
		t.Fatalf("state/code = %q/%q", completer.state, completer.code)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "coralogix is connected") {
		t.Fatalf("body missing success message: %s", body)
	}
}

type fakeOAuthCompleter struct {
	result mcp.OAuthCompleteResult
	state  string
	code   string
}

func (f *fakeOAuthCompleter) Complete(ctx context.Context, state, code string) (mcp.OAuthCompleteResult, error) {
	_ = ctx
	f.state = state
	f.code = code
	return f.result, nil
}

func silentOAuthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestOAuthCallbackServerShutdownBeforeStartIsSafe(t *testing.T) {
	srv := &oauthCallbackServer{logger: silentOAuthTestLogger()}
	done := make(chan struct{})
	go func() {
		srv.Shutdown()
		srv.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown should not block when server was never started")
	}
}
