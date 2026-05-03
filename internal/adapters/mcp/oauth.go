package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OAuthOptions configures OAuth callback and pending-session behaviour.
type OAuthOptions struct {
	PublicBaseURL string
	CallbackPath  string
	StateTTL      time.Duration
}

// OAuthManager coordinates authorization-code OAuth for HTTP MCP servers.
type OAuthManager struct {
	servers map[string]ServerConfig
	opts    OAuthOptions
	store   CredentialStore
	client  *http.Client
	now     func() time.Time

	mu       sync.Mutex
	sessions map[string]oauthSession
}

type oauthSession struct {
	ServerName    string
	CredentialRef string
	RedirectURI   string
	CodeVerifier  string
	ClientID      string
	ClientSecret  string
	ExpiresAt     time.Time
}

// OAuthStartResult is safe to return to the LLM/operator.
type OAuthStartResult struct {
	ServerName       string    `json:"server_name"`
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// OAuthCompleteResult is safe browser callback output.
type OAuthCompleteResult struct {
	ServerName string `json:"server_name"`
	Status     string `json:"status"`
}

// OAuthStatus is safe to return to the LLM/operator.
type OAuthStatus struct {
	ServerName string    `json:"server_name"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

// OAuthCredential is the persisted token set for one configured credential ref.
type OAuthCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

// CredentialStore persists OAuth credentials outside mcp.json.
type CredentialStore interface {
	Get(ctx context.Context, ref string) (OAuthCredential, bool, error)
	Put(ctx context.Context, ref string, cred OAuthCredential) error
	Delete(ctx context.Context, ref string) error
}

// NewOAuthManager constructs an OAuth manager for the configured MCP servers.
func NewOAuthManager(servers []ServerConfig, opts OAuthOptions, store CredentialStore, client *http.Client) *OAuthManager {
	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.CallbackPath == "" {
		opts.CallbackPath = "/oauth/callback"
	}
	if opts.StateTTL <= 0 {
		opts.StateTTL = 10 * time.Minute
	}
	return &OAuthManager{
		servers:  byName,
		opts:     opts,
		store:    store,
		client:   client,
		now:      time.Now,
		sessions: make(map[string]oauthSession),
	}
}

// SetServers replaces the configured MCP server map while preserving pending
// sessions and stored credentials.
func (m *OAuthManager) SetServers(servers []ServerConfig) {
	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	m.mu.Lock()
	m.servers = byName
	m.mu.Unlock()
}

// Start begins an authorization-code flow and returns the user-facing URL.
func (m *OAuthManager) Start(ctx context.Context, serverName string) (OAuthStartResult, error) {
	server, auth, err := m.oauthServer(serverName)
	if err != nil {
		return OAuthStartResult{}, err
	}
	if m.opts.PublicBaseURL == "" {
		return OAuthStartResult{}, fmt.Errorf("oauth public_base_url is required for Telegram-driven setup")
	}

	meta, err := m.metadata(ctx, server, *auth)
	if err != nil {
		return OAuthStartResult{}, err
	}
	client, err := m.clientRegistration(ctx, server, *auth, meta)
	if err != nil {
		return OAuthStartResult{}, err
	}
	state, err := randomURLToken(32)
	if err != nil {
		return OAuthStartResult{}, err
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return OAuthStartResult{}, err
	}
	challenge := codeChallenge(verifier)
	redirectURI := m.redirectURI()
	expiresAt := m.now().Add(m.opts.StateTTL)

	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", client.ClientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("state", state)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	if len(client.Scopes) > 0 {
		values.Set("scope", strings.Join(client.Scopes, " "))
	}
	if meta.Resource != "" {
		values.Set("resource", meta.Resource)
	}
	authURL, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return OAuthStartResult{}, fmt.Errorf("parse authorization endpoint: %w", err)
	}
	q := authURL.Query()
	for key, vals := range values {
		for _, value := range vals {
			q.Add(key, value)
		}
	}
	authURL.RawQuery = q.Encode()

	m.mu.Lock()
	m.sessions[state] = oauthSession{
		ServerName:    server.Name,
		CredentialRef: auth.CredentialRef,
		RedirectURI:   redirectURI,
		CodeVerifier:  verifier,
		ClientID:      client.ClientID,
		ClientSecret:  client.ClientSecret,
		ExpiresAt:     expiresAt,
	}
	m.mu.Unlock()

	return OAuthStartResult{ServerName: server.Name, AuthorizationURL: authURL.String(), ExpiresAt: expiresAt}, nil
}

// Complete validates state, exchanges the authorization code, and stores tokens.
func (m *OAuthManager) Complete(ctx context.Context, state, code string) (OAuthCompleteResult, error) {
	if state == "" || code == "" {
		return OAuthCompleteResult{}, fmt.Errorf("state and code are required")
	}
	session, err := m.consumeSession(state)
	if err != nil {
		return OAuthCompleteResult{}, err
	}
	server, auth, err := m.oauthServer(session.ServerName)
	if err != nil {
		return OAuthCompleteResult{}, err
	}
	meta, err := m.metadata(ctx, server, *auth)
	if err != nil {
		return OAuthCompleteResult{}, err
	}
	cred, err := m.exchangeCode(ctx, meta.TokenEndpoint, *auth, session, code)
	if err != nil {
		return OAuthCompleteResult{}, err
	}
	if err := m.store.Put(ctx, session.CredentialRef, cred); err != nil {
		return OAuthCompleteResult{}, err
	}
	return OAuthCompleteResult{ServerName: session.ServerName, Status: "connected"}, nil
}

// Status returns the current safe auth state for one MCP server.
func (m *OAuthManager) Status(ctx context.Context, serverName string) OAuthStatus {
	server, auth, err := m.oauthServer(serverName)
	if err != nil {
		return OAuthStatus{ServerName: serverName, Status: "not_configured"}
	}
	_ = server
	cred, ok, err := m.store.Get(ctx, auth.CredentialRef)
	if err != nil {
		return OAuthStatus{ServerName: serverName, Status: "refresh_failed"}
	}
	if !ok || cred.AccessToken == "" {
		if m.hasPendingSession(serverName) {
			return OAuthStatus{ServerName: serverName, Status: "pending"}
		}
		return OAuthStatus{ServerName: serverName, Status: "needs_authorization"}
	}
	if !cred.ExpiresAt.IsZero() && m.now().After(cred.ExpiresAt) {
		return OAuthStatus{ServerName: serverName, Status: "needs_authorization", ExpiresAt: cred.ExpiresAt}
	}
	return OAuthStatus{ServerName: serverName, Status: "connected", ExpiresAt: cred.ExpiresAt}
}

func (m *OAuthManager) hasPendingSession(serverName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, session := range m.sessions {
		if session.ServerName == serverName && now.Before(session.ExpiresAt) {
			return true
		}
	}
	return false
}

// AccessToken returns a valid access token, refreshing it when possible.
func (m *OAuthManager) AccessToken(ctx context.Context, serverName string) (string, error) {
	server, auth, err := m.oauthServer(serverName)
	if err != nil {
		return "", err
	}
	cred, ok, err := m.store.Get(ctx, auth.CredentialRef)
	if err != nil {
		return "", err
	}
	if !ok || cred.AccessToken == "" {
		return "", fmt.Errorf("oauth credentials for %q need authorization", serverName)
	}
	if cred.ExpiresAt.IsZero() || m.now().Before(cred.ExpiresAt.Add(-1*time.Minute)) {
		return cred.AccessToken, nil
	}
	if cred.RefreshToken == "" {
		return "", fmt.Errorf("oauth credentials for %q are expired and cannot refresh", serverName)
	}
	meta, err := m.metadata(ctx, server, *auth)
	if err != nil {
		return "", err
	}
	refreshed, err := m.refresh(ctx, meta.TokenEndpoint, *auth, cred)
	if err != nil {
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = cred.RefreshToken
	}
	if err := m.store.Put(ctx, auth.CredentialRef, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

// RefreshAccessToken refreshes an access token immediately. It is used after a
// server rejects a request with 401 even if the local expiry has not elapsed.
func (m *OAuthManager) RefreshAccessToken(ctx context.Context, serverName string) (string, error) {
	server, auth, err := m.oauthServer(serverName)
	if err != nil {
		return "", err
	}
	cred, ok, err := m.store.Get(ctx, auth.CredentialRef)
	if err != nil {
		return "", err
	}
	if !ok || cred.RefreshToken == "" {
		return "", fmt.Errorf("oauth credentials for %q cannot refresh", serverName)
	}
	meta, err := m.metadata(ctx, server, *auth)
	if err != nil {
		return "", err
	}
	refreshed, err := m.refresh(ctx, meta.TokenEndpoint, *auth, cred)
	if err != nil {
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = cred.RefreshToken
	}
	if err := m.store.Put(ctx, auth.CredentialRef, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

func (m *OAuthManager) oauthServer(serverName string) (ServerConfig, *AuthConfig, error) {
	server, ok := m.servers[serverName]
	if !ok {
		return ServerConfig{}, nil, fmt.Errorf("mcp server %q is not configured", serverName)
	}
	if server.Transport != "http" {
		return ServerConfig{}, nil, fmt.Errorf("mcp server %q is not an http server", serverName)
	}
	if server.Auth == nil || server.Auth.Type != "oauth_authorization_code" {
		return ServerConfig{}, nil, fmt.Errorf("mcp server %q is not configured for OAuth", serverName)
	}
	return server, server.Auth, nil
}

type oauthMetadata struct {
	Issuer                string   `json:"issuer,omitempty"`
	Resource              string   `json:"resource,omitempty"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint,omitempty"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
}

type oauthClientRegistration struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
}

func (m *OAuthManager) metadata(ctx context.Context, server ServerConfig, auth AuthConfig) (oauthMetadata, error) {
	meta := oauthMetadata{AuthorizationEndpoint: auth.AuthorizationURL, TokenEndpoint: auth.TokenURL, ScopesSupported: cloneStrings(auth.Scopes)}
	issuer := strings.TrimRight(auth.Issuer, "/")
	if issuer == "" {
		resourceMeta, err := m.discoverProtectedResource(ctx, server)
		if err != nil {
			return meta, err
		}
		meta.Resource = resourceMeta.Resource
		if len(meta.ScopesSupported) == 0 {
			meta.ScopesSupported = resourceMeta.ScopesSupported
		}
		if len(resourceMeta.AuthorizationServers) == 0 {
			if meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
				return meta, nil
			}
			return meta, fmt.Errorf("protected resource metadata for %q missing authorization_servers", server.Name)
		}
		issuer = strings.TrimRight(resourceMeta.AuthorizationServers[0], "/")
	}
	if meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
		return meta, nil
	}
	for _, discoveryURL := range authorizationServerMetadataURLs(issuer) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
		if err != nil {
			return meta, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := m.client.Do(req)
		if err != nil {
			return meta, fmt.Errorf("discover oauth metadata for %q: %w", server.Name, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}
		if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
			_ = resp.Body.Close()
			return meta, fmt.Errorf("parse oauth metadata: %w", err)
		}
		_ = resp.Body.Close()
		if len(meta.ScopesSupported) == 0 {
			meta.ScopesSupported = auth.Scopes
		}
		if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
			return meta, fmt.Errorf("oauth metadata missing authorization_endpoint or token_endpoint")
		}
		return meta, nil
	}
	return meta, fmt.Errorf("discover oauth metadata for %q returned no usable metadata", server.Name)
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

func (m *OAuthManager) discoverProtectedResource(ctx context.Context, server ServerConfig) (protectedResourceMetadata, error) {
	if challengeURL, err := m.resourceMetadataFromChallenge(ctx, server); err == nil && challengeURL != "" {
		meta, ok, err := m.getProtectedResourceMetadata(ctx, challengeURL)
		if err != nil {
			return protectedResourceMetadata{}, err
		}
		if ok {
			return meta, nil
		}
	}
	for _, discoveryURL := range protectedResourceMetadataURLs(server.URL) {
		meta, ok, err := m.getProtectedResourceMetadata(ctx, discoveryURL)
		if err != nil {
			return protectedResourceMetadata{}, err
		}
		if ok {
			return meta, nil
		}
	}
	return protectedResourceMetadata{}, fmt.Errorf("oauth authorization_url and token_url are required when protected resource metadata is unavailable")
}

func (m *OAuthManager) getProtectedResourceMetadata(ctx context.Context, metadataURL string) (protectedResourceMetadata, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return protectedResourceMetadata{}, false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return protectedResourceMetadata{}, false, fmt.Errorf("discover protected resource metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protectedResourceMetadata{}, false, nil
	}
	var meta protectedResourceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return protectedResourceMetadata{}, false, fmt.Errorf("parse protected resource metadata: %w", err)
	}
	return meta, true, nil
}

func (m *OAuthManager) resourceMetadataFromChallenge(ctx context.Context, server ServerConfig) (string, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"faultline","version":"dev"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("probe oauth challenge for %q: %w", server.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return "", nil
	}
	for _, challenge := range resp.Header.Values("WWW-Authenticate") {
		if metadataURL := bearerChallengeParam(challenge, "resource_metadata"); metadataURL != "" {
			return metadataURL, nil
		}
	}
	return "", nil
}

func authorizationServerMetadataURLs(issuer string) []string {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return []string{
			issuer + "/.well-known/oauth-authorization-server",
			issuer + "/.well-known/openid-configuration",
		}
	}
	root := parsed.Scheme + "://" + parsed.Host
	path := strings.Trim(parsed.EscapedPath(), "/")
	urls := []string{
		issuer + "/.well-known/oauth-authorization-server",
		root + "/.well-known/oauth-authorization-server",
		issuer + "/.well-known/openid-configuration",
		root + "/.well-known/openid-configuration",
	}
	if path != "" {
		urls = append(urls,
			root+"/.well-known/oauth-authorization-server/"+path,
			root+"/.well-known/openid-configuration/"+path,
		)
	}
	return uniqueStrings(urls)
}

func protectedResourceMetadataURLs(rawURL string) []string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	root := parsed.Scheme + "://" + parsed.Host
	urls := []string{}
	path := strings.Trim(parsed.EscapedPath(), "/")
	if path != "" {
		urls = append(urls, root+"/.well-known/oauth-protected-resource/"+path)
	}
	urls = append(urls, root+"/.well-known/oauth-protected-resource")
	return urls
}

func bearerChallengeParam(challenge, key string) string {
	challenge = strings.TrimSpace(challenge)
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return ""
	}
	params := strings.TrimSpace(challenge[len("Bearer "):])
	for _, part := range strings.Split(params, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

func (m *OAuthManager) clientRegistration(ctx context.Context, server ServerConfig, auth AuthConfig, meta oauthMetadata) (oauthClientRegistration, error) {
	if auth.ClientID != "" || auth.ClientIDEnv != "" {
		return oauthClientRegistration{ClientID: configuredClientID(auth), ClientSecret: configuredClientSecret(auth), Scopes: selectedScopes(auth, meta)}, nil
	}
	cred, ok, err := m.store.Get(ctx, auth.CredentialRef)
	if err != nil {
		return oauthClientRegistration{}, err
	}
	if ok && cred.ClientID != "" {
		return oauthClientRegistration{ClientID: cred.ClientID, ClientSecret: cred.ClientSecret, Scopes: selectedScopes(auth, meta)}, nil
	}
	if meta.RegistrationEndpoint == "" {
		return oauthClientRegistration{}, fmt.Errorf("oauth client_id is required and authorization server for %q does not advertise dynamic client registration", server.Name)
	}
	registered, err := m.dynamicClientRegistration(ctx, meta.RegistrationEndpoint, selectedScopes(auth, meta))
	if err != nil {
		return oauthClientRegistration{}, err
	}
	cred.ClientID = registered.ClientID
	cred.ClientSecret = registered.ClientSecret
	if err := m.store.Put(ctx, auth.CredentialRef, cred); err != nil {
		return oauthClientRegistration{}, err
	}
	return registered, nil
}

func (m *OAuthManager) dynamicClientRegistration(ctx context.Context, registrationURL string, scopes []string) (oauthClientRegistration, error) {
	body := map[string]interface{}{
		"client_name":                "Faultline",
		"redirect_uris":              []string{m.redirectURI()},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return oauthClientRegistration{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationURL, strings.NewReader(string(data)))
	if err != nil {
		return oauthClientRegistration{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return oauthClientRegistration{}, fmt.Errorf("dynamic client registration: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oauthClientRegistration{}, fmt.Errorf("dynamic client registration returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return oauthClientRegistration{}, fmt.Errorf("parse dynamic client registration response: %w", err)
	}
	if parsed.ClientID == "" {
		return oauthClientRegistration{}, fmt.Errorf("dynamic client registration response missing client_id")
	}
	return oauthClientRegistration{ClientID: parsed.ClientID, ClientSecret: parsed.ClientSecret, Scopes: scopes}, nil
}

func (m *OAuthManager) exchangeCode(ctx context.Context, tokenURL string, auth AuthConfig, session oauthSession, code string) (OAuthCredential, error) {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("code", code)
	values.Set("redirect_uri", session.RedirectURI)
	values.Set("code_verifier", session.CodeVerifier)
	addClientCredentials(values, configuredClientCredentials(auth, session.ClientID, session.ClientSecret))
	cred, err := m.tokenRequest(ctx, tokenURL, values)
	if err != nil {
		return OAuthCredential{}, err
	}
	cred.ClientID = session.ClientID
	cred.ClientSecret = session.ClientSecret
	return cred, nil
}

func (m *OAuthManager) refresh(ctx context.Context, tokenURL string, auth AuthConfig, cred OAuthCredential) (OAuthCredential, error) {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("refresh_token", cred.RefreshToken)
	addClientCredentials(values, configuredClientCredentials(auth, cred.ClientID, cred.ClientSecret))
	refreshed, err := m.tokenRequest(ctx, tokenURL, values)
	if err != nil {
		return OAuthCredential{}, err
	}
	refreshed.ClientID = cred.ClientID
	refreshed.ClientSecret = cred.ClientSecret
	return refreshed, nil
}

type clientCredentials struct {
	ClientID     string
	ClientSecret string
}

func configuredClientCredentials(auth AuthConfig, fallbackID, fallbackSecret string) clientCredentials {
	id := configuredClientID(auth)
	if id == "" {
		id = fallbackID
	}
	secret := configuredClientSecret(auth)
	if secret == "" {
		secret = fallbackSecret
	}
	return clientCredentials{ClientID: id, ClientSecret: secret}
}

func configuredClientID(auth AuthConfig) string {
	if auth.ClientID != "" {
		return auth.ClientID
	}
	if auth.ClientIDEnv != "" {
		return os.Getenv(auth.ClientIDEnv)
	}
	return ""
}

func configuredClientSecret(auth AuthConfig) string {
	if auth.ClientSecretEnv != "" {
		return os.Getenv(auth.ClientSecretEnv)
	}
	if auth.ClientSecretFile != "" {
		if data, err := os.ReadFile(auth.ClientSecretFile); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func addClientCredentials(values url.Values, creds clientCredentials) {
	if creds.ClientID != "" {
		values.Set("client_id", creds.ClientID)
	}
	if creds.ClientSecret != "" {
		values.Set("client_secret", creds.ClientSecret)
	}
}

func selectedScopes(auth AuthConfig, meta oauthMetadata) []string {
	if len(auth.Scopes) > 0 {
		return cloneStrings(auth.Scopes)
	}
	return cloneStrings(meta.ScopesSupported)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func (m *OAuthManager) tokenRequest(ctx context.Context, tokenURL string, values url.Values) (OAuthCredential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return OAuthCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return OAuthCredential{}, fmt.Errorf("oauth token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return OAuthCredential{}, fmt.Errorf("oauth token endpoint returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return OAuthCredential{}, fmt.Errorf("parse oauth token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return OAuthCredential{}, fmt.Errorf("oauth token response missing access_token")
	}
	cred := OAuthCredential{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    parsed.TokenType,
	}
	if parsed.ExpiresIn > 0 {
		cred.ExpiresAt = m.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return cred, nil
}

func (m *OAuthManager) consumeSession(state string) (oauthSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[state]
	if !ok {
		return oauthSession{}, fmt.Errorf("oauth state is not pending")
	}
	delete(m.sessions, state)
	if m.now().After(session.ExpiresAt) {
		return oauthSession{}, fmt.Errorf("oauth state expired")
	}
	return session, nil
}

func (m *OAuthManager) redirectURI() string {
	base := strings.TrimRight(m.opts.PublicBaseURL, "/")
	path := m.opts.CallbackPath
	if path == "" {
		path = "/oauth/callback"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func randomURLToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// FileCredentialStore stores OAuth credentials in a single JSON file.
type FileCredentialStore struct {
	path string
	mu   sync.Mutex
}

func NewFileCredentialStore(path string) *FileCredentialStore {
	return &FileCredentialStore{path: path}
}

func (s *FileCredentialStore) Get(ctx context.Context, ref string) (OAuthCredential, bool, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return OAuthCredential{}, false, err
	}
	cred, ok := all[ref]
	return cred, ok, nil
}

func (s *FileCredentialStore) Put(ctx context.Context, ref string, cred OAuthCredential) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return err
	}
	all[ref] = cred
	return s.save(all)
}

func (s *FileCredentialStore) Delete(ctx context.Context, ref string) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return err
	}
	delete(all, ref)
	return s.save(all)
}

func (s *FileCredentialStore) load() (map[string]OAuthCredential, error) {
	all := make(map[string]OAuthCredential)
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return all, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return all, nil
	}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("parse oauth credential file: %w", err)
	}
	return all, nil
}

func (s *FileCredentialStore) save(all map[string]OAuthCredential) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".oauth-tokens-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}
