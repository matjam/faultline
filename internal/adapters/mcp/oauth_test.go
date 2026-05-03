package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOAuthManagerStartRequiresPublicBaseURL(t *testing.T) {
	mgr := NewOAuthManager([]ServerConfig{oauthTestServerConfig("coralogix", "https://issuer.example.invalid")}, OAuthOptions{}, newMemoryCredentialStore(), nil)

	if _, err := mgr.Start(context.Background(), "coralogix"); err == nil {
		t.Fatal("expected missing public_base_url to fail")
	}
}

func TestOAuthManagerStartBuildsAuthorizationURLWithPKCE(t *testing.T) {
	provider := newOAuthProvider(t)
	defer provider.Close()
	mgr := NewOAuthManager(
		[]ServerConfig{oauthTestServerConfig("coralogix", provider.URL)},
		OAuthOptions{PublicBaseURL: "https://faultline.example.com", CallbackPath: "/oauth/callback", StateTTL: time.Minute},
		newMemoryCredentialStore(),
		provider.Client(),
	)

	start, err := mgr.Start(context.Background(), "coralogix")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	authURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("Parse authorization URL: %v", err)
	}
	values := authURL.Query()
	if got := authURL.String(); !strings.HasPrefix(got, provider.URL+"/authorize?") {
		t.Fatalf("AuthorizationURL = %q, want provider authorize endpoint", got)
	}
	for _, key := range []string{"state", "code_challenge"} {
		if values.Get(key) == "" {
			t.Fatalf("AuthorizationURL missing %s in %q", key, start.AuthorizationURL)
		}
	}
	if values.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", values.Get("code_challenge_method"))
	}
	if values.Get("redirect_uri") != "https://faultline.example.com/oauth/callback" {
		t.Fatalf("redirect_uri = %q", values.Get("redirect_uri"))
	}
}

func TestOAuthManagerCompleteExchangesCodeAndStoresCredential(t *testing.T) {
	provider := newOAuthProvider(t)
	defer provider.Close()
	store := newMemoryCredentialStore()
	mgr := NewOAuthManager(
		[]ServerConfig{oauthTestServerConfig("coralogix", provider.URL)},
		OAuthOptions{PublicBaseURL: "https://faultline.example.com", CallbackPath: "/oauth/callback", StateTTL: time.Minute},
		store,
		provider.Client(),
	)
	mgr.now = func() time.Time { return time.Unix(1000, 0).UTC() }

	start, err := mgr.Start(context.Background(), "coralogix")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	authURL, _ := url.Parse(start.AuthorizationURL)
	result, err := mgr.Complete(context.Background(), authURL.Query().Get("state"), "code-123")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Status != "connected" {
		t.Fatalf("Status = %q, want connected", result.Status)
	}
	cred, ok, err := store.Get(context.Background(), "mcp/coralogix")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected credential to be stored")
	}
	if cred.AccessToken != "access-code-123" || cred.RefreshToken != "refresh-code-123" {
		t.Fatalf("stored credential = %#v", cred)
	}
	if got, want := cred.ExpiresAt, time.Unix(4600, 0).UTC(); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got, want)
	}
}

func TestOAuthManagerAccessTokenRefreshesExpiredCredential(t *testing.T) {
	provider := newOAuthProvider(t)
	defer provider.Close()
	store := newMemoryCredentialStore()
	if err := store.Put(context.Background(), "mcp/coralogix", OAuthCredential{
		AccessToken:  "old-access",
		RefreshToken: "refresh-abc",
		ExpiresAt:    time.Unix(1000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	mgr := NewOAuthManager(
		[]ServerConfig{oauthTestServerConfig("coralogix", provider.URL)},
		OAuthOptions{PublicBaseURL: "https://faultline.example.com", StateTTL: time.Minute},
		store,
		provider.Client(),
	)
	mgr.now = func() time.Time { return time.Unix(2000, 0).UTC() }

	token, err := mgr.AccessToken(context.Background(), "coralogix")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "access-refresh-abc" {
		t.Fatalf("AccessToken = %q, want access-refresh-abc", token)
	}
	cred, _, err := store.Get(context.Background(), "mcp/coralogix")
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "access-refresh-abc" || cred.RefreshToken != "refresh-abc" {
		t.Fatalf("refreshed credential = %#v", cred)
	}
}

func TestOAuthManagerStartDiscoversProtectedResourceAndRegistersClient(t *testing.T) {
	provider := newOAuthProvider(t)
	defer provider.Close()
	store := newMemoryCredentialStore()
	server := ServerConfig{
		Name:      "coralogix",
		Transport: "http",
		URL:       provider.URL + "/v1/mcp",
		Auth: &AuthConfig{
			Type:          "oauth_authorization_code",
			CredentialRef: "mcp/coralogix",
		},
	}
	mgr := NewOAuthManager(
		[]ServerConfig{server},
		OAuthOptions{PublicBaseURL: "https://faultline.example.com", CallbackPath: "/oauth/callback", StateTTL: time.Minute},
		store,
		provider.Client(),
	)

	start, err := mgr.Start(context.Background(), "coralogix")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	authURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("Parse authorization URL: %v", err)
	}
	values := authURL.Query()
	if values.Get("client_id") != "registered-client" {
		t.Fatalf("client_id = %q, want registered-client", values.Get("client_id"))
	}
	if values.Get("resource") != provider.URL+"/v1/mcp" {
		t.Fatalf("resource = %q", values.Get("resource"))
	}
	if values.Get("scope") != "openid offline_access" {
		t.Fatalf("scope = %q", values.Get("scope"))
	}
	cred, ok, err := store.Get(context.Background(), "mcp/coralogix")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cred.ClientID != "registered-client" {
		t.Fatalf("stored registration = %#v", cred)
	}
}

func TestOAuthManagerStartPrefersConfiguredScopesOverDiscoveredScopes(t *testing.T) {
	provider := newOAuthProvider(t)
	defer provider.Close()
	server := ServerConfig{
		Name:      "slack",
		Transport: "http",
		URL:       provider.URL + "/v1/mcp",
		Auth: &AuthConfig{
			Type:          "oauth_authorization_code",
			CredentialRef: "mcp/slack",
			ClientID:      "client-id",
			Scopes:        []string{"channels:history", "users:read"},
		},
	}
	mgr := NewOAuthManager(
		[]ServerConfig{server},
		OAuthOptions{PublicBaseURL: "https://faultline.example.com", CallbackPath: "/oauth/callback", StateTTL: time.Minute},
		newMemoryCredentialStore(),
		provider.Client(),
	)

	start, err := mgr.Start(context.Background(), "slack")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	authURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("Parse authorization URL: %v", err)
	}
	if got := authURL.Query().Get("scope"); got != "channels:history users:read" {
		t.Fatalf("scope = %q, want configured scopes", got)
	}
}

func TestFileCredentialStorePersistsWithRestrictedPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "oauth-tokens.json")
	store := NewFileCredentialStore(path)
	want := OAuthCredential{AccessToken: "access", RefreshToken: "refresh"}
	if err := store.Put(context.Background(), "mcp/coralogix", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := NewFileCredentialStore(path).Get(context.Background(), "mcp/coralogix")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected credential after reload")
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Fatalf("credential = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", gotMode)
	}
}

func TestAuthorizationServerMetadataURLsIncludePathIssuerForms(t *testing.T) {
	got := authorizationServerMetadataURLs("https://auth.example.com/tenant")
	for _, want := range []string{
		"https://auth.example.com/tenant/.well-known/oauth-authorization-server",
		"https://auth.example.com/.well-known/oauth-authorization-server/tenant",
		"https://auth.example.com/tenant/.well-known/openid-configuration",
		"https://auth.example.com/.well-known/openid-configuration/tenant",
	} {
		if !containsString(got, want) {
			t.Fatalf("authorizationServerMetadataURLs missing %q from %#v", want, got)
		}
	}
}

func oauthTestServerConfig(name, issuer string) ServerConfig {
	return ServerConfig{
		Name:      name,
		Transport: "http",
		URL:       issuer + "/mcp",
		Auth: &AuthConfig{
			Type:          "oauth_authorization_code",
			CredentialRef: "mcp/" + name,
			Issuer:        issuer,
			ClientID:      "faultline",
			Scopes:        []string{"openid", "offline_access"},
		},
	}
}

func newOAuthProvider(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"registration_endpoint":  server.URL + "/register",
			"scopes_supported":       []string{"openid", "offline_access"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"registration_endpoint":  server.URL + "/register",
			"scopes_supported":       []string{"openid", "offline_access"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/v1/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              server.URL + "/v1/mcp",
			"authorization_servers": []string{server.URL},
			"scopes_supported":      []string{"openid", "offline_access"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Decode register request: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if len(req.RedirectURIs) != 1 || req.RedirectURIs[0] != "https://faultline.example.com/oauth/callback" {
			t.Errorf("redirect_uris = %#v", req.RedirectURIs)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "registered-client"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if r.PostForm.Get("code_verifier") == "" {
				t.Error("authorization_code request missing code_verifier")
			}
			code := r.PostForm.Get("code")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-" + code,
				"refresh_token": "refresh-" + code,
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case "refresh_token":
			refresh := r.PostForm.Get("refresh_token")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-" + refresh,
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
		}
	})
	return server
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type memoryCredentialStore struct {
	m map[string]OAuthCredential
}

func newMemoryCredentialStore() *memoryCredentialStore {
	return &memoryCredentialStore{m: make(map[string]OAuthCredential)}
}

func (s *memoryCredentialStore) Get(ctx context.Context, ref string) (OAuthCredential, bool, error) {
	_ = ctx
	cred, ok := s.m[ref]
	return cred, ok, nil
}

func (s *memoryCredentialStore) Put(ctx context.Context, ref string, cred OAuthCredential) error {
	_ = ctx
	s.m[ref] = cred
	return nil
}

func (s *memoryCredentialStore) Delete(ctx context.Context, ref string) error {
	_ = ctx
	delete(s.m, ref)
	return nil
}
