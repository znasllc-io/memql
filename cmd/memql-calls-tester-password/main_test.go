package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// verifies checks a hash line against a password from the documented format
// alone (v1$salt$digest, SHA-256 of the salt and the canonical password), so the
// command and the engine cannot drift apart unnoticed.
func verifies(t *testing.T, line, password string) bool {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(line), "$")
	require.Len(t, parts, 3)
	require.Equal(t, "v1", parts[0])
	salt, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	digest, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	canonical := strings.ToUpper(strings.ReplaceAll(password, "-", ""))
	sum := sha256.Sum256(append(append([]byte{}, salt...), canonical...))
	return subtle.ConstantTimeCompare(sum[:], digest) == 1
}

func TestTesterPasswordCommandWritesPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	pwPath, hashPath := filepath.Join(dir, "password"), filepath.Join(dir, "password.hash")
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, run([]string{"--password-file=" + pwPath, "--hash-file=" + hashPath}, &stdout, &stderr), stderr.String())

	for _, p := range []string{pwPath, hashPath} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), p)
	}
	password, err := os.ReadFile(pwPath)
	require.NoError(t, err)
	line, err := os.ReadFile(hashPath)
	require.NoError(t, err)
	require.Regexp(t, `^[A-HJ-NP-Z2-9]{5}(-[A-HJ-NP-Z2-9]{5}){3}\n$`, string(password))
	require.Regexp(t, `^v1\$[A-Za-z0-9_-]{22}\$[A-Za-z0-9_-]{43}\n$`, string(line))
	require.True(t, verifies(t, string(line), strings.TrimSpace(string(password))))

	printed := stdout.String() + stderr.String()
	require.Contains(t, printed, pwPath)
	require.Contains(t, printed, hashPath)
	require.NotContains(t, printed, strings.TrimSpace(string(password)))
	require.NotContains(t, printed, strings.TrimSpace(string(line)))
}

func TestTesterPasswordCommandRefusesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	pwPath, hashPath := filepath.Join(dir, "password"), filepath.Join(dir, "password.hash")
	require.NoError(t, os.WriteFile(hashPath, []byte("keep"), 0o600))
	var stdout, stderr bytes.Buffer
	require.NotEqual(t, 0, run([]string{"--password-file=" + pwPath, "--hash-file=" + hashPath}, &stdout, &stderr))
	kept, err := os.ReadFile(hashPath)
	require.NoError(t, err)
	require.Equal(t, "keep", string(kept))
	_, err = os.Stat(pwPath)
	require.True(t, os.IsNotExist(err), "no password without its hash")
}

func TestTesterPasswordCommandRequiresBothFiles(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{nil, {"--password-file=" + filepath.Join(dir, "p")}, {"--hash-file=" + filepath.Join(dir, "h")}, {"--password-file=" + filepath.Join(dir, "same"), "--hash-file=" + filepath.Join(dir, "same")}} {
		var stdout, stderr bytes.Buffer
		require.NotEqual(t, 0, run(args, &stdout, &stderr), args)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
