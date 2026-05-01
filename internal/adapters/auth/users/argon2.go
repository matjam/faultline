// Package users implements the local authentication adapter for the
// admin UI: argon2id password hashing, users.toml load/save with
// first-run admin bootstrap, in-memory session store with idle TTL
// eviction, and per-session CSRF tokens.
//
// This is a driving-side adapter dependency — the admin HTTP adapter
// uses it directly, the agent domain has no knowledge of users or
// sessions. Nothing here imports internal/agent.
package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. OWASP-recommended-ish defaults sized for an
// admin login (rare, interactive). m=64 MiB and t=3 give comfortably
// over 100ms per verify on a modern x86 core; if that's too slow on a
// pi or similar, the operator can regenerate hashes with a different
// param block — verify reads params from the encoded hash, so old and
// new hashes can coexist.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // 64 MiB, expressed in KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen uint32 = 16
)

// ErrInvalidHash is returned when a stored hash cannot be parsed.
// Distinct from ErrPasswordMismatch so the operator can distinguish
// "corrupt users.toml" from "wrong password".
var (
	ErrInvalidHash      = errors.New("users: hash is not in the expected argon2id PHC format")
	ErrUnsupportedHash  = errors.New("users: hash uses an algorithm or version we don't support")
	ErrPasswordMismatch = errors.New("users: password does not match")
	ErrEmptyPassword    = errors.New("users: password must not be empty")
	ErrPasswordTooShort = errors.New("users: password must be at least 8 characters")
	ErrPasswordTooLong  = errors.New("users: password must be at most 256 characters")
)

// HashPassword derives an argon2id hash from password and encodes it
// in the standard PHC string format:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt-b64>$<hash-b64>
//
// Salt is generated with crypto/rand. The encoded form is
// self-describing — Verify reads the params back out, so changing the
// constants above does not invalidate existing hashes.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}
	if len(password) < 8 {
		return "", ErrPasswordTooShort
	}
	if len(password) > 256 {
		return "", ErrPasswordTooLong
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("users: read salt entropy: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// Standard PHC string. Padding stripped per the format spec.
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword returns nil iff password matches the stored encoded
// hash. ErrPasswordMismatch is returned for the common bad-password
// case; ErrInvalidHash / ErrUnsupportedHash for a corrupt or foreign
// hash format. Constant-time comparison is used to avoid leaking
// information about how much of the hash matched.
func VerifyPassword(password, encoded string) error {
	if encoded == "" {
		return ErrInvalidHash
	}
	if password == "" {
		// Treat empty input as a mismatch rather than an error so
		// the caller's flow is the same as any other wrong-password
		// case. Avoids exposing "user exists with empty password".
		return ErrPasswordMismatch
	}

	parts := strings.Split(encoded, "$")
	// Format: ["", "argon2id", "v=19", "m=65536,t=3,p=4", "<salt>", "<hash>"]
	if len(parts) != 6 {
		return ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return ErrUnsupportedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return ErrInvalidHash
	}
	if version != argon2.Version {
		return ErrUnsupportedHash
	}

	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrInvalidHash
	}
	if len(want) == 0 {
		return ErrInvalidHash
	}

	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}
