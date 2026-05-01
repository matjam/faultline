package users

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=") {
		t.Fatalf("encoded hash has wrong prefix: %q", encoded)
	}
	if err := VerifyPassword("correct horse battery staple", encoded); err != nil {
		t.Fatalf("VerifyPassword on correct password: %v", err)
	}
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := VerifyPassword("incorrect horse battery staple", encoded); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, got %v", err)
	}
}

func TestVerifyPassword_EmptyPassword(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := VerifyPassword("", encoded); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("empty password: expected ErrPasswordMismatch, got %v", err)
	}
}

func TestHashPassword_Rejects(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     error
	}{
		{"empty", "", ErrEmptyPassword},
		{"too-short", "1234567", ErrPasswordTooShort},
		{"too-long", strings.Repeat("a", 257), ErrPasswordTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := HashPassword(tc.password); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestVerifyPassword_MalformedHash(t *testing.T) {
	cases := []struct {
		name    string
		encoded string
		want    error
	}{
		{"empty", "", ErrInvalidHash},
		{"wrong-segments", "$argon2id$bogus", ErrInvalidHash},
		{"non-argon2id", "$bcrypt$v=19$m=65536,t=3,p=4$AAAA$BBBB", ErrUnsupportedHash},
		{"bad-version-header", "$argon2id$xxx$m=65536,t=3,p=4$AAAA$BBBB", ErrInvalidHash},
		{"bad-salt-base64", "$argon2id$v=19$m=65536,t=3,p=4$!!!!$BBBB", ErrInvalidHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyPassword("anything goes", tc.encoded); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestHashPassword_DifferentSaltsEachTime(t *testing.T) {
	a, err := HashPassword("same-password-everywhere")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword("same-password-everywhere")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Fatalf("two hashes of the same password collided; salt is not random")
	}
}
