package users

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// SessionTokenBytes is the entropy of a session token before base64
// encoding. 32 bytes -> 256 bits, far more than enough.
const SessionTokenBytes = 32

// Session represents one authenticated browser. Lived in memory only;
// all sessions are evicted on process restart, which the operator
// accepts as part of the "single binary, single user" model.
type Session struct {
	Token     string    // raw secret value; never logged
	Username  string    // matches the User.Name that authenticated
	CSRFToken string    // per-session CSRF guard
	Created   time.Time // wall-clock at New()
	LastSeen  time.Time // updated on every Touch()
}

// SessionStore is an in-memory map of token -> session, protected by
// a mutex, with a janitor goroutine that evicts idle sessions older
// than the configured TTL.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration

	// stop is closed by Close to terminate the janitor goroutine.
	// Sized for one signal; closing is idempotent via the once.
	stop     chan struct{}
	stopOnce sync.Once
}

// NewSessionStore constructs a store and starts the janitor in a
// goroutine. The janitor exits when Close is called or the supplied
// context is canceled.
func NewSessionStore(ctx context.Context, ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	s := &SessionStore{
		sessions: make(map[string]*Session),
		ttl:      ttl,
		stop:     make(chan struct{}),
	}
	go s.janitor(ctx)
	return s
}

// New mints a fresh session for the named user and returns it. The
// returned token is what the cookie should carry; the CSRF token is
// what forms must echo back.
func (s *SessionStore) New(username string) (*Session, error) {
	tok, err := randomToken(SessionTokenBytes)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(SessionTokenBytes)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sess := &Session{
		Token:     tok,
		Username:  username,
		CSRFToken: csrf,
		Created:   now,
		LastSeen:  now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[tok] = sess
	return sess, nil
}

// Get returns the session matching token, refreshing its LastSeen
// stamp under the same lock. Returns nil + false if not present or
// expired (an expired session is removed eagerly).
func (s *SessionStore) Get(token string) (*Session, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return nil, false
	}
	if s.expired(sess) {
		delete(s.sessions, token)
		return nil, false
	}
	sess.LastSeen = time.Now().UTC()
	return sess, true
}

// Delete removes the session, returning true iff it existed. Used by
// /admin/logout.
func (s *SessionStore) Delete(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[token]; !ok {
		return false
	}
	delete(s.sessions, token)
	return true
}

// Count returns the current session count. Useful for the admin
// dashboard's "you have N active sessions" widget once we add it.
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// Close stops the janitor goroutine. Idempotent.
func (s *SessionStore) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *SessionStore) expired(sess *Session) bool {
	return time.Since(sess.LastSeen) > s.ttl
}

func (s *SessionStore) janitor(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-tick.C:
			s.evict()
		}
	}
}

func (s *SessionStore) evict() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, sess := range s.sessions {
		if s.expired(sess) {
			delete(s.sessions, tok)
		}
	}
}

// VerifyCSRF returns nil iff supplied matches sess.CSRFToken.
// Constant-time compare to avoid any timing side-channel on the
// rare path where an attacker can guess parts of the value.
func VerifyCSRF(sess *Session, supplied string) error {
	if sess == nil {
		return errors.New("users: no session for csrf check")
	}
	if supplied == "" {
		return errors.New("users: missing csrf token")
	}
	if subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(supplied)) != 1 {
		return errors.New("users: csrf token mismatch")
	}
	return nil
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewToken mints a random URL-safe token of SessionTokenBytes
// entropy. Exported so the admin HTTP adapter can reuse the same
// primitive for short-lived non-session tokens (e.g. the login-form
// CSRF token issued before any session exists).
func NewToken() (string, error) {
	return randomToken(SessionTokenBytes)
}
