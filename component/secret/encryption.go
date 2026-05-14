// Package secret provides authenticated symmetric encryption helpers
// for values stored in memQL concept rows (v1:platform:partitionSecret and
// v1:platform:globalSecret).
//
// Scheme: NaCl secretbox (XSalsa20-Poly1305), the Go port of libsodium's
// secretbox primitive. The master key comes from the env var
// MEMQL_MASTER_KEY and must be 32 bytes, hex-encoded (so 64 hex
// characters). NaCl secretbox was chosen per the Phase 1 decision:
// simplest authenticated symmetric encryption that works everywhere the
// platform runs, with no KMS dependency.
//
// Ciphertext format written to *:secret.encryptedValue (and historical
// v1:router:apikey.encryptedKey rows):
//
//   base64( nonce (24B) || secretbox_seal(plaintext) )
//
// Decryption reverses it. Helpers are pure -- they don't mutate shared
// state and are safe to call concurrently.
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// EnvMasterKey names the environment variable that carries the 32-byte
// hex-encoded master encryption key. Exported so the config loader and
// docs can reference it by symbol rather than a bare string literal.
const EnvMasterKey = "MEMQL_MASTER_KEY"

const nonceLen = 24

// masterKey loads + validates the 32-byte encryption key from env.
// Returns an error when the env var is missing or not 64 hex chars --
// secret writes should fail loudly in that case rather than silently
// writing cleartext or a predictable ciphertext.
func masterKey() (*[32]byte, error) {
	raw := strings.TrimSpace(os.Getenv(EnvMasterKey))
	if raw == "" {
		return nil, fmt.Errorf("%s is not set; encrypted secrets cannot be used without it", EnvMasterKey)
	}
	bytes, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid hex: %w", EnvMasterKey, err)
	}
	if len(bytes) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", EnvMasterKey, len(bytes))
	}
	var key [32]byte
	copy(key[:], bytes)
	return &key, nil
}

// Encrypt encrypts a cleartext value and returns
// (base64Ciphertext, fingerprint, error). The fingerprint is the last
// four characters of the cleartext, used by the UI/CLI to differentiate
// rotated values without revealing the full secret.
//
// Callers must not log or display plaintext; only the returned
// ciphertext (safe) and fingerprint (last-4-chars, informational) are
// intended for persistence/display.
func Encrypt(plaintext string) (ciphertextB64 string, fingerprint string, err error) {
	trimmed := strings.TrimSpace(plaintext)
	if trimmed == "" {
		return "", "", fmt.Errorf("secret.Encrypt: plaintext is empty")
	}
	key, err := masterKey()
	if err != nil {
		return "", "", err
	}
	var nonce [nonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", fmt.Errorf("secret.Encrypt: read nonce: %w", err)
	}
	sealed := secretbox.Seal(nil, []byte(trimmed), &nonce, key)
	// Output is nonce || ciphertext, base64-encoded.
	buf := make([]byte, 0, len(nonce)+len(sealed))
	buf = append(buf, nonce[:]...)
	buf = append(buf, sealed...)
	return base64.StdEncoding.EncodeToString(buf), Fingerprint(trimmed), nil
}

// Decrypt reverses Encrypt. Returns an error if the master key is
// missing, the ciphertext is malformed, or authentication fails
// (indicating a rotated key or tampered storage).
func Decrypt(ciphertextB64 string) (string, error) {
	trimmed := strings.TrimSpace(ciphertextB64)
	if trimmed == "" {
		return "", fmt.Errorf("secret.Decrypt: ciphertext is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return "", fmt.Errorf("secret.Decrypt: base64 decode: %w", err)
	}
	if len(raw) < nonceLen {
		return "", fmt.Errorf("secret.Decrypt: ciphertext too short (%d bytes)", len(raw))
	}
	key, err := masterKey()
	if err != nil {
		return "", err
	}
	var nonce [nonceLen]byte
	copy(nonce[:], raw[:nonceLen])
	opened, ok := secretbox.Open(nil, raw[nonceLen:], &nonce, key)
	if !ok {
		return "", fmt.Errorf("secret.Decrypt: authenticated decryption failed (key rotated or ciphertext tampered)")
	}
	return string(opened), nil
}

// Fingerprint returns the last four characters of the input, prefixed
// with an ellipsis, so callers can render "...fG3q" for a secret
// without leaking the rest. Short inputs (<= 4 chars) are returned
// verbatim -- they shouldn't occur in practice for real secrets.
func Fingerprint(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return s
	}
	return "..." + s[len(s)-4:]
}
