package users

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_BootstrapsAdminOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	store, boot, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if boot == nil {
		t.Fatalf("expected bootstrap result on missing file, got nil")
	}
	if boot.Username != "admin" {
		t.Fatalf("expected bootstrap username 'admin', got %q", boot.Username)
	}
	if len(boot.Password) < 16 {
		t.Fatalf("bootstrap password is too short: %q", boot.Password)
	}

	// Verify the password we got back actually authenticates.
	if _, err := store.Verify("admin", boot.Password); err != nil {
		t.Fatalf("Verify(admin, bootstrap-password): %v", err)
	}

	// File on disk must contain the comment with the plaintext
	// password so an operator who missed the log can recover it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read users.toml: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "Initial admin password: "+boot.Password) {
		t.Fatalf("users.toml does not embed the bootstrap password as a comment.\n--- file ---\n%s\n", body)
	}
	if !strings.Contains(body, "[[user]]") {
		t.Fatalf("users.toml missing [[user]] table:\n%s", body)
	}

	// File must be 0600 so we don't leak a password hash via
	// loose perms.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("users.toml perms = %o, want 0600", got)
	}
}

func TestNew_DoesNotRebootstrapOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	_, boot, err := New(path)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if boot == nil {
		t.Fatalf("expected bootstrap on first run")
	}

	// Snapshot the file. Re-opening the store must not modify it.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first New: %v", err)
	}

	store, boot2, err := New(path)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if boot2 != nil {
		t.Fatalf("expected no bootstrap on second run; got %+v", boot2)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second New: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("users.toml changed on second New\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// Verify still works after reload.
	if _, err := store.Verify("admin", boot.Password); err != nil {
		t.Fatalf("Verify after reload: %v", err)
	}
}

func TestStore_SetPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	store, boot, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := store.SetPassword("admin", "NewPasswordWithEnoughChars!"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Old password must no longer work.
	if _, err := store.Verify("admin", boot.Password); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("old password should be rejected, got %v", err)
	}
	// New one must work.
	if _, err := store.Verify("admin", "NewPasswordWithEnoughChars!"); err != nil {
		t.Fatalf("new password verify: %v", err)
	}

	// File survives a reload with the new hash.
	store2, _, err := New(path)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if _, err := store2.Verify("admin", "NewPasswordWithEnoughChars!"); err != nil {
		t.Fatalf("post-reload verify: %v", err)
	}
}

func TestStore_VerifyUnknownUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	store, _, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Verify("not-a-user", "anything-here"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestStore_SetPasswordUnknownUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	store, _, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.SetPassword("ghost", "ValidPassword123"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestStore_VerifyCaseInsensitiveUsername(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.toml")

	store, boot, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Verify("ADMIN", boot.Password); err != nil {
		t.Fatalf("Verify uppercase: %v", err)
	}
	if _, err := store.Verify("  Admin  ", boot.Password); err != nil {
		t.Fatalf("Verify with surrounding whitespace: %v", err)
	}
}
