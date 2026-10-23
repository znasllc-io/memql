package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCredentialFileMode_AcceptsSecure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(path, []byte("token: abc"), 0o600); err != nil {
		t.Fatalf("write fake creds: %v", err)
	}
	if err := VerifyCredentialFileMode(path); err != nil {
		t.Fatalf("0600 file should be admitted; got %v", err)
	}
}

func TestVerifyCredentialFileMode_RejectsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(path, []byte("token: abc"), 0o640); err != nil {
		t.Fatalf("write fake creds: %v", err)
	}
	err := VerifyCredentialFileMode(path)
	if err == nil {
		t.Fatalf("0640 file should be rejected")
	}
	if !strings.Contains(err.Error(), "must be 0600") {
		t.Errorf("error %q does not name required mode", err.Error())
	}
}

func TestVerifyCredentialFileMode_RejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(path, []byte("token: abc"), 0o644); err != nil {
		t.Fatalf("write fake creds: %v", err)
	}
	if err := VerifyCredentialFileMode(path); err == nil {
		t.Fatalf("0644 file should be rejected")
	}
}

func TestVerifyCredentialFileMode_MissingFileIsErr(t *testing.T) {
	if err := VerifyCredentialFileMode("/nonexistent/path/that/does/not/exist"); err == nil {
		t.Fatalf("missing file should produce an error so the caller can decide")
	}
}
