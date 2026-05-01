package users

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// User is one row in users.toml. Created and Updated are advisory; we
// don't currently use them for auth decisions but they're useful for
// the operator inspecting the file by hand.
type User struct {
	Name         string    `toml:"name"`
	PasswordHash string    `toml:"password_hash"`
	Created      time.Time `toml:"created"`
	Updated      time.Time `toml:"updated"`
}

// fileShape is the on-disk wire shape: a top-level [[user]] array of
// tables. Wrapping it in a struct rather than naked-decoding into
// []User makes it easy to add other top-level fields later
// (e.g. a global lockout policy) without breaking older files.
type fileShape struct {
	Users []User `toml:"user"`
}

// Store holds the loaded user list under a mutex. All mutations are
// flushed to disk synchronously; we never queue writes, so a crash
// can't lose an applied change. The file is small and rarely written.
type Store struct {
	path string
	mu   sync.RWMutex
	list []User
}

// ErrUserNotFound is returned by Lookup / SetPassword when the named
// user is not present.
var ErrUserNotFound = errors.New("users: user not found")

// New loads (or initializes) a Store at the given path. If the file
// does not exist, a single admin user is bootstrapped with a randomly
// generated 24-character password; the plaintext password is returned
// to the caller via the BootstrapResult so the composition root can
// emit it on stderr. The same plaintext is also stamped into the file
// as a comment line above the user entry, so an operator who missed
// the log can still recover it.
//
// On every subsequent start the file is loaded as-is; no comment is
// rewritten.
func New(path string) (*Store, *BootstrapResult, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		boot, err := s.bootstrap()
		if err != nil {
			return nil, nil, fmt.Errorf("bootstrap users file: %w", err)
		}
		return s, boot, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read users file: %w", err)
	}

	var shape fileShape
	if err := toml.Unmarshal(data, &shape); err != nil {
		return nil, nil, fmt.Errorf("parse users file: %w", err)
	}
	for i := range shape.Users {
		shape.Users[i].Name = strings.ToLower(strings.TrimSpace(shape.Users[i].Name))
	}
	s.list = shape.Users
	return s, nil, nil
}

// BootstrapResult is returned the first time Store is created against
// a missing users file. The composition root is expected to emit the
// plaintext password on stderr at WARN-or-higher and otherwise treat
// it as a one-time secret.
type BootstrapResult struct {
	Username  string
	Password  string // plaintext, intentionally surfaced exactly once
	Generated time.Time
}

// bootstrap creates the initial admin user and writes the file with
// a comment line embedding the plaintext password. The caller already
// holds (or has not yet shared) the Store, so we don't lock; this is
// only ever called from New before the Store is observable.
func (s *Store) bootstrap() (*BootstrapResult, error) {
	password, err := generatePassword(24)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash bootstrap password: %w", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	s.list = []User{{
		Name:         "admin",
		PasswordHash: hash,
		Created:      now,
		Updated:      now,
	}}
	if err := s.saveWithBootstrapComment(password); err != nil {
		return nil, fmt.Errorf("write bootstrap users file: %w", err)
	}
	return &BootstrapResult{
		Username:  "admin",
		Password:  password,
		Generated: now,
	}, nil
}

// saveWithBootstrapComment writes the users file with a leading
// comment block recording the auto-generated password. The comment
// is preserved on subsequent reads (we only ever rewrite the file
// when the operator changes a password, in which case the comment is
// dropped — the operator has by then logged in and the comment is
// stale, possibly misleading).
func (s *Store) saveWithBootstrapComment(plaintextPassword string) error {
	var b strings.Builder
	b.WriteString("# Faultline admin users.\n")
	b.WriteString("#\n")
	b.WriteString("# This file was auto-generated on first run with a single admin\n")
	b.WriteString("# user. The randomly generated password is recorded below as a\n")
	b.WriteString("# one-time secret. After your first successful login you should\n")
	b.WriteString("# change the password from the admin UI; that rewrites this file\n")
	b.WriteString("# and drops this comment block.\n")
	b.WriteString("#\n")
	b.WriteString("# Initial admin username: admin\n")
	fmt.Fprintf(&b, "# Initial admin password: %s\n", plaintextPassword)
	b.WriteString("#\n")
	b.WriteString("# DO NOT commit this file. It contains password material.\n")
	b.WriteString("\n")
	b.WriteString(s.encodeUsers())
	return atomicWrite(s.path, []byte(b.String()))
}

// save rewrites the users file without any header comment.
func (s *Store) save() error {
	return atomicWrite(s.path, []byte(s.encodeUsers()))
}

func (s *Store) encodeUsers() string {
	var b strings.Builder
	for i, u := range s.list {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[[user]]\n")
		fmt.Fprintf(&b, "name          = %q\n", u.Name)
		fmt.Fprintf(&b, "password_hash = %q\n", u.PasswordHash)
		fmt.Fprintf(&b, "created       = %s\n", u.Created.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "updated       = %s\n", u.Updated.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// atomicWrite writes to a sibling temp file and renames over the
// target. Same pattern as the agent's state file: a crash mid-write
// either leaves the old file or the new one, never a partial.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".users-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Restrictive permissions: this file holds password material.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// Verify checks the supplied credentials. Returns the matching User
// (by value, no pointer to internal slice memory) on success, or one
// of ErrUserNotFound / ErrPasswordMismatch / ErrInvalidHash on
// failure. The lookup is case-insensitive on username.
func (s *Store) Verify(username, password string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.list {
		if u.Name == username {
			if err := VerifyPassword(password, u.PasswordHash); err != nil {
				return User{}, err
			}
			return u, nil
		}
	}
	// No matching user. Run a dummy verify against a constant hash
	// so the timing of the failure does not betray whether the
	// username existed. The constant is the hash of the empty
	// string; we discard the result.
	_ = VerifyPassword(password, dummyHash)
	return User{}, ErrUserNotFound
}

// dummyHash is computed once at package init and used to keep the
// failure path of Verify the same wall-clock cost regardless of
// whether the username matched. Without this an attacker could
// distinguish "user does not exist" (cheap fast-path return) from
// "user exists, wrong password" (full argon2id derivation) by
// timing alone.
//
// The plaintext is a fixed constant; we don't care that it lives
// in memory because it has no security purpose.
var dummyHash string

func init() {
	h, err := HashPassword("faultline-timing-shield")
	if err != nil {
		// HashPassword cannot fail for a valid 8+ char input;
		// panicking at init is the right behavior if it does.
		panic("users: failed to compute dummy hash: " + err.Error())
	}
	dummyHash = h
}

// SetPassword updates the named user's password and persists. Returns
// ErrUserNotFound when the user is absent.
func (s *Store) SetPassword(username, newPassword string) error {
	username = strings.ToLower(strings.TrimSpace(username))
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].Name == username {
			s.list[i].PasswordHash = hash
			s.list[i].Updated = time.Now().UTC().Truncate(time.Second)
			return s.save()
		}
	}
	return ErrUserNotFound
}

// generatePassword returns n base32-ish characters drawn from a
// reduced alphabet that avoids visually-similar glyphs (no 0/O, 1/I/l).
// 24 characters gives ~115 bits of entropy at 32 distinct symbols,
// well above the threshold where dictionary or brute-force matters.
func generatePassword(n int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	var b strings.Builder
	b.Grow(n)
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("read password entropy: %w", err)
		}
		b.WriteByte(alphabet[idx.Int64()])
	}
	return b.String(), nil
}
