package secret

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

// testKey generates a valid 32-byte hex-encoded master key for unit
// tests, avoiding hard-coded test keys that would leak into the
// production build.
func testKey(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))

	plaintext := "sk-proj-abcdef0123456789"

	ct, fp, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == plaintext {
		t.Fatalf("ciphertext must not equal plaintext")
	}
	if !strings.HasPrefix(fp, "...") || !strings.HasSuffix(fp, "6789") {
		t.Fatalf("unexpected fingerprint %q", fp)
	}

	got, err := Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestEncrypt_EmptyPlaintextFails(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	if _, _, err := Encrypt("   "); err == nil {
		t.Fatalf("expected error for empty plaintext")
	}
}

func TestEncrypt_MissingKeyFails(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	_, _, err := Encrypt("anything")
	if err == nil {
		t.Fatalf("expected error when master key missing")
	}
	if !strings.Contains(err.Error(), EnvMasterKey) {
		t.Fatalf("error should name the env var, got %v", err)
	}
}

func TestDecrypt_KeyMismatchFails(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	ct, _, err := Encrypt("some-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Rotate to a fresh key and ensure Decrypt reports authentication
	// failure rather than returning garbage bytes.
	t.Setenv(EnvMasterKey, testKey(t))
	if _, err := Decrypt(ct); err == nil {
		t.Fatalf("expected authenticated-decryption failure after key rotation")
	}
}

func TestDecrypt_MalformedCiphertextFails(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	if _, err := Decrypt("not-base64!!"); err == nil {
		t.Fatalf("expected error for malformed base64")
	}
	if _, err := Decrypt(""); err == nil {
		t.Fatalf("expected error for empty ciphertext")
	}
}

func TestFingerprint_Formats(t *testing.T) {
	if got := Fingerprint("abcd"); got != "abcd" {
		t.Fatalf("short input: got %q want %q", got, "abcd")
	}
	if got := Fingerprint("abcdef"); got != "...cdef" {
		t.Fatalf("long input: got %q want %q", got, "...cdef")
	}
	if got := Fingerprint("  sk-prefix-6789  "); got != "...6789" {
		t.Fatalf("trimmed input: got %q want %q", got, "...6789")
	}
}

func TestEncrypt_NonceIsRandom(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	ct1, _, err := Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, _, err := Encrypt("same-input")
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if ct1 == ct2 {
		t.Fatalf("two encryptions of the same plaintext must produce different ciphertexts (random nonce)")
	}
}

// ---------------------------------------------------------------------
// SealBlob / OpenBlob tests
// ---------------------------------------------------------------------

func TestSealOpenBlob_RoundTrip(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	plaintext := []byte("OPENAI_API_KEY=sk-test\nANTHROPIC_API_KEY=sk-ant\n")

	env, err := SealBlob(plaintext)
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	if string(env[0:4]) != BlobMagic {
		t.Fatalf("magic: got %q want %q", env[0:4], BlobMagic)
	}
	if env[4] != BlobVersion {
		t.Fatalf("version byte: got %#x want %#x", env[4], BlobVersion)
	}
	if env[5] != BlobAlgoSecretbox {
		t.Fatalf("algo byte: got %#x want %#x", env[5], BlobAlgoSecretbox)
	}
	if env[6] != 0 || env[7] != 0 {
		t.Fatalf("reserved bytes must be zero, got %#x %#x", env[6], env[7])
	}

	opened, err := OpenBlob(env)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	if string(opened) != string(plaintext) {
		t.Fatalf("round-trip mismatch: got %q want %q", opened, plaintext)
	}
}

func TestOpenBlob_WrongKeyFails(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	env, err := SealBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	t.Setenv(EnvMasterKey, testKey(t))
	if _, err := OpenBlob(env); err == nil {
		t.Fatalf("expected auth failure with wrong key")
	}
}

func TestOpenBlob_TamperFails(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	env, err := SealBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	// Flip the last byte (inside the ciphertext / auth-tag region).
	env[len(env)-1] ^= 0xFF
	if _, err := OpenBlob(env); err == nil {
		t.Fatalf("expected auth failure for tampered envelope")
	}
}

func TestOpenBlob_BadMagic(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	env, err := SealBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	env[0] = 'X'
	_, err = OpenBlob(env)
	if err == nil {
		t.Fatalf("expected magic error")
	}
	if !strings.Contains(err.Error(), "bad magic") {
		t.Fatalf("error should mention bad magic, got %v", err)
	}
}

func TestOpenBlob_UnsupportedVersion(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	env, err := SealBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	env[4] = 0xFF
	_, err = OpenBlob(env)
	if err == nil {
		t.Fatalf("expected version error")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error should mention version, got %v", err)
	}
}

func TestOpenBlob_UnknownAlgorithm(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	env, err := SealBlob([]byte("payload"))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	env[5] = 0xFF
	_, err = OpenBlob(env)
	if err == nil {
		t.Fatalf("expected algorithm error")
	}
	if !strings.Contains(err.Error(), "algorithm") {
		t.Fatalf("error should mention algorithm, got %v", err)
	}
}

func TestOpenBlob_TooShort(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	if _, err := OpenBlob(nil); err == nil {
		t.Fatalf("expected error for nil envelope")
	}
	if _, err := OpenBlob([]byte("short")); err == nil {
		t.Fatalf("expected error for short envelope")
	}
}

func TestSealBlob_NonceIsRandom(t *testing.T) {
	t.Setenv(EnvMasterKey, testKey(t))
	a, err := SealBlob([]byte("same"))
	if err != nil {
		t.Fatalf("SealBlob 1: %v", err)
	}
	b, err := SealBlob([]byte("same"))
	if err != nil {
		t.Fatalf("SealBlob 2: %v", err)
	}
	if string(a) == string(b) {
		t.Fatalf("two seals of the same plaintext must differ (random nonce)")
	}
}

func TestSealBlob_MissingKeyFails(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	if _, err := SealBlob([]byte("payload")); err == nil {
		t.Fatalf("expected error when master key missing")
	}
}
