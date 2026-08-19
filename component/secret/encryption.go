// Package secret provides authenticated symmetric encryption helpers
// for memql. The live surface is per-value Encrypt / Decrypt: seals one
// secret string at a time for storage in memQL concept rows
// (v1:platform:partitionSecret, v1:platform:globalSecret, historical
// v1:router:apikey).
//
// (A whole-blob SealBlob / OpenBlob pair used to live here too, for the
// sealed genesis envelope's on-disk artifact. It is gone -- see "THE
// WHOLE-BLOB SEAL/OPEN PAIR IS GONE" further down this file, epic memql#3958.)
//
// Encrypt/Decrypt use NaCl secretbox (XSalsa20-Poly1305) -- the Go port of
// libsodium's secretbox primitive -- and a single 32-byte master key
// from the MEMQL_MASTER_KEY env var (64 hex characters). NaCl secretbox
// was chosen per the Phase 1 decision: simplest authenticated symmetric
// encryption that works everywhere the platform runs, no KMS dependency.
//
// Per-value ciphertext format (concept rows):
//
//	base64( nonce(24B) || secretbox_seal(plaintext) )
//
// Helpers are pure -- they don't mutate shared state and are safe to
// call concurrently.
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
//
// This key DECRYPTS. It is not an authentication credential and must not
// be used as one again -- see EnvOperatorKey.
const EnvMasterKey = "MEMQL_MASTER_KEY"

// The OPERATOR credential -- the bearer token that authenticates
// `Authorization: Operator <key>` as a synthetic cluster owner -- is a
// DIFFERENT secret, and its name lives in component/auth as
// auth.EnvOperatorKey (memql#3519). Deliberately not here: this package is
// about encryption, an auth credential is not, and keeping the two names in
// one file is a short step from keeping the two VALUES in one variable, which
// is the defect that issue exists to undo. See
// docs/public/operate/auth/operator-credential.md.

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

// THE WHOLE-BLOB SEAL/OPEN PAIR IS GONE (epic memql#3958).
//
// SealBlob / OpenBlob, the "ZNAS" envelope format, BlobMagic, BlobVersion and
// the header-length constant existed for exactly one caller: the sealed genesis
// envelope, which sealed a developer's .env under MEMQL_MASTER_KEY so a node
// could decrypt it back into its own environment at boot. Config has ONE
// delivery path now -- the k8s Secret every node envFroms -- so nothing seals a
// blob and nothing opens one.
//
// Encrypt / Decrypt STAY, and the distinction is worth stating plainly because
// deleting the wrong pair would take the secrets store with it: those are the
// per-VALUE functions behind v1:platform:globalSecret, a live feature with live
// rows. What went is the whole-FILE pair.
//
// MEMQL_MASTER_KEY itself also stays, and stays DECRYPT-ONLY. memql#3519 split
// decrypting from authenticating; the operator bearer is MEMQL_OPERATOR_KEY.
// Nothing in this epic re-merges them.
