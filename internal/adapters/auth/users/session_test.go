package users

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSessionStore_NewAndGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewSessionStore(ctx, time.Hour)
	defer store.Close()

	sess, err := store.New("admin")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sess.Username != "admin" {
		t.Fatalf("Username = %q", sess.Username)
	}
	if sess.Token == "" {
		t.Fatal("empty session token")
	}
	if sess.CSRFToken == "" {
		t.Fatal("empty csrf token")
	}
	if sess.Token == sess.CSRFToken {
		t.Fatal("session token and csrf token must be distinct")
	}

	got, ok := store.Get(sess.Token)
	if !ok {
		t.Fatal("session not retrievable")
	}
	if got.Username != "admin" {
		t.Fatalf("retrieved Username = %q", got.Username)
	}

	if _, ok := store.Get("not-a-token"); ok {
		t.Fatal("Get with unknown token should return false")
	}
	if _, ok := store.Get(""); ok {
		t.Fatal("Get with empty token should return false")
	}
}

func TestSessionStore_Delete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewSessionStore(ctx, time.Hour)
	defer store.Close()

	sess, _ := store.New("admin")
	if !store.Delete(sess.Token) {
		t.Fatal("Delete should report true on existing session")
	}
	if _, ok := store.Get(sess.Token); ok {
		t.Fatal("session still retrievable after Delete")
	}
	if store.Delete(sess.Token) {
		t.Fatal("Delete on already-deleted should report false")
	}
}

func TestSessionStore_TTLEviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tiny TTL so we can observe eviction without sleeping long.
	store := NewSessionStore(ctx, 30*time.Millisecond)
	defer store.Close()

	sess, _ := store.New("admin")
	// Immediately retrievable.
	if _, ok := store.Get(sess.Token); !ok {
		t.Fatal("freshly-minted session not retrievable")
	}

	// Wait past TTL; Get should report expired and evict eagerly.
	time.Sleep(60 * time.Millisecond)
	if _, ok := store.Get(sess.Token); ok {
		t.Fatal("expired session still retrievable")
	}
}

func TestSessionStore_GetRefreshesLastSeen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewSessionStore(ctx, 100*time.Millisecond)
	defer store.Close()

	sess, _ := store.New("admin")
	first := sess.LastSeen

	// Active access: each Get within TTL should bump LastSeen and
	// keep the session alive indefinitely.
	for i := 0; i < 5; i++ {
		time.Sleep(40 * time.Millisecond)
		if _, ok := store.Get(sess.Token); !ok {
			t.Fatalf("session evicted on iteration %d despite being active", i)
		}
	}

	got, ok := store.Get(sess.Token)
	if !ok {
		t.Fatal("session not retrievable after refresh loop")
	}
	if !got.LastSeen.After(first) {
		t.Fatalf("LastSeen not updated: first=%v after=%v", first, got.LastSeen)
	}
}

func TestSessionStore_Concurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewSessionStore(ctx, time.Hour)
	defer store.Close()

	const N = 32
	tokens := make([]string, N)
	for i := 0; i < N; i++ {
		s, err := store.New("u")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		tokens[i] = s.Token
	}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, ok := store.Get(tok); !ok {
					t.Errorf("Get failed during race")
					return
				}
			}
		}(tokens[i])
	}
	wg.Wait()

	if got := store.Count(); got != N {
		t.Fatalf("Count = %d, want %d", got, N)
	}
}

func TestVerifyCSRF(t *testing.T) {
	sess := &Session{CSRFToken: "abcdef"}
	if err := VerifyCSRF(sess, "abcdef"); err != nil {
		t.Fatalf("matching token: %v", err)
	}
	if err := VerifyCSRF(sess, "wrong"); err == nil {
		t.Fatal("mismatched token: want error")
	}
	if err := VerifyCSRF(sess, ""); err == nil {
		t.Fatal("empty supplied: want error")
	}
	if err := VerifyCSRF(nil, "anything"); err == nil {
		t.Fatal("nil session: want error")
	}
}

func TestNewToken_NonEmpty(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(tok) < 32 {
		t.Fatalf("token too short: %q", tok)
	}
}
