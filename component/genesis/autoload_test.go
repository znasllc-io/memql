package genesis

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql/component/secret"
)

// freshMasterKey returns a valid 64-char hex master key.
func freshMasterKey(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

// sealEnvelope seals entries under masterKeyHex and returns the
// encrypted envelope bytes. Mirrors what the cockpit Seal path
// produces, but minimal -- just the secret-layer round-trip.
func sealEnvelope(t *testing.T, masterKeyHex string, entries []EnvEntry) []byte {
	t.Helper()
	prev := os.Getenv(secret.EnvMasterKey)
	if err := os.Setenv(secret.EnvMasterKey, masterKeyHex); err != nil {
		t.Fatalf("set master key: %v", err)
	}
	defer func() { _ = os.Setenv(secret.EnvMasterKey, prev) }()
	envelope, err := secret.SealBlob(SerializeEntries(entries))
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	return envelope
}

func sampleEntries() []EnvEntry {
	return []EnvEntry{
		{Name: "OPENAI_API_KEY", Value: "sk-from-envelope"},
		{Name: "MEMORY_NODES_DATABASE_DSN", Value: "postgres://local-dev"},
		{Name: "MEMQL_NODE_TYPE", Value: "bff"},
	}
}

func TestAutoloadFromEnv_DisabledIsNoop(t *testing.T) {
	// Flag unset -> no-op, no error, nothing applied.
	t.Setenv(EnvAutoload, "")
	t.Setenv(EnvGenesisB64, "should-be-ignored")
	t.Setenv("OPENAI_API_KEY", "")
	_ = os.Unsetenv("OPENAI_API_KEY")

	res, err := AutoloadFromEnv()
	if err != nil {
		t.Fatalf("AutoloadFromEnv: %v", err)
	}
	if res.Enabled {
		t.Fatal("expected Enabled=false when flag unset")
	}
	if _, present := os.LookupEnv("OPENAI_API_KEY"); present {
		t.Fatal("disabled autoload must not set any env var")
	}
}

func TestAutoloadFromEnv_FromB64(t *testing.T) {
	key := freshMasterKey(t)
	envelope := sealEnvelope(t, key, sampleEntries())
	b64 := base64.StdEncoding.EncodeToString(envelope)

	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, b64)
	// Ensure target vars are absent so they get applied.
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("MEMORY_NODES_DATABASE_DSN")
	_ = os.Unsetenv("MEMQL_NODE_TYPE")
	t.Cleanup(func() {
		_ = os.Unsetenv("OPENAI_API_KEY")
		_ = os.Unsetenv("MEMORY_NODES_DATABASE_DSN")
		_ = os.Unsetenv("MEMQL_NODE_TYPE")
	})

	res, err := AutoloadFromEnv()
	if err != nil {
		t.Fatalf("AutoloadFromEnv: %v", err)
	}
	if !res.Enabled || res.Source != "b64" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "sk-from-envelope" {
		t.Fatalf("OPENAI_API_KEY = %q, want sk-from-envelope", got)
	}
	if len(res.Applied) != 3 {
		t.Fatalf("expected 3 applied, got %d (%v)", len(res.Applied), res.Applied)
	}
}

func TestAutoloadFromEnv_FromFile(t *testing.T) {
	key := freshMasterKey(t)
	envelope := sealEnvelope(t, key, sampleEntries())
	dir := t.TempDir()
	path := filepath.Join(dir, "genesis.znas")
	if err := os.WriteFile(path, envelope, 0o600); err != nil {
		t.Fatalf("write envelope: %v", err)
	}

	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, "") // force the file path
	t.Setenv(EnvGenesisPath, path)
	_ = os.Unsetenv("OPENAI_API_KEY")
	t.Cleanup(func() { _ = os.Unsetenv("OPENAI_API_KEY") })

	res, err := AutoloadFromEnv()
	if err != nil {
		t.Fatalf("AutoloadFromEnv: %v", err)
	}
	if !res.Enabled || res.Source != path {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "sk-from-envelope" {
		t.Fatalf("OPENAI_API_KEY = %q, want sk-from-envelope", got)
	}
}

func TestAutoloadFromEnv_SetIfAbsentPrecedence(t *testing.T) {
	// A pre-set var (simulating a Container App override) must survive.
	key := freshMasterKey(t)
	envelope := sealEnvelope(t, key, sampleEntries())
	b64 := base64.StdEncoding.EncodeToString(envelope)

	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, b64)
	// Container App override already in the environment.
	t.Setenv("MEMORY_NODES_DATABASE_DSN", "postgres://tiger-cloud-prod")
	t.Setenv("MEMQL_NODE_TYPE", "cognition")
	_ = os.Unsetenv("OPENAI_API_KEY")
	t.Cleanup(func() { _ = os.Unsetenv("OPENAI_API_KEY") })

	res, err := AutoloadFromEnv()
	if err != nil {
		t.Fatalf("AutoloadFromEnv: %v", err)
	}
	if got := os.Getenv("MEMORY_NODES_DATABASE_DSN"); got != "postgres://tiger-cloud-prod" {
		t.Fatalf("pre-set DSN clobbered: got %q", got)
	}
	if got := os.Getenv("MEMQL_NODE_TYPE"); got != "cognition" {
		t.Fatalf("pre-set node type clobbered: got %q", got)
	}
	// The absent one still gets applied.
	if got := os.Getenv("OPENAI_API_KEY"); got != "sk-from-envelope" {
		t.Fatalf("OPENAI_API_KEY = %q, want sk-from-envelope", got)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("expected 2 skipped, got %d (%v)", len(res.Skipped), res.Skipped)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("expected 1 applied, got %d (%v)", len(res.Applied), res.Applied)
	}
}

func TestAutoloadFromEnv_MissingMasterKeyErrors(t *testing.T) {
	key := freshMasterKey(t)
	envelope := sealEnvelope(t, key, sampleEntries())
	b64 := base64.StdEncoding.EncodeToString(envelope)

	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, "") // missing
	t.Setenv(EnvGenesisB64, b64)

	if _, err := AutoloadFromEnv(); err == nil {
		t.Fatal("expected error when master key is missing")
	}
}

func TestAutoloadFromEnv_NoSourceErrors(t *testing.T) {
	key := freshMasterKey(t)
	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, "")
	// Point at a path that does not exist.
	t.Setenv(EnvGenesisPath, filepath.Join(t.TempDir(), "missing.znas"))

	if _, err := AutoloadFromEnv(); err == nil {
		t.Fatal("expected error when no envelope source is available")
	}
}

func TestAutoloadFromEnv_BadCiphertextErrors(t *testing.T) {
	key := freshMasterKey(t)
	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, base64.StdEncoding.EncodeToString([]byte("not-a-sealed-envelope")))

	if _, err := AutoloadFromEnv(); err == nil {
		t.Fatal("expected error for bad ciphertext")
	}
}

func TestAutoloadFromEnv_BadBase64Errors(t *testing.T) {
	key := freshMasterKey(t)
	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, key)
	t.Setenv(EnvGenesisB64, "!!!not base64!!!")

	if _, err := AutoloadFromEnv(); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestAutoloadFromEnv_WrongKeyErrors(t *testing.T) {
	sealKey := freshMasterKey(t)
	otherKey := freshMasterKey(t)
	envelope := sealEnvelope(t, sealKey, sampleEntries())
	b64 := base64.StdEncoding.EncodeToString(envelope)

	t.Setenv(EnvAutoload, "true")
	t.Setenv(secret.EnvMasterKey, otherKey) // valid hex, wrong key
	t.Setenv(EnvGenesisB64, b64)

	if _, err := AutoloadFromEnv(); err == nil {
		t.Fatal("expected decrypt failure with wrong master key")
	}
}

func TestOpenBytesRoundTrip(t *testing.T) {
	key := freshMasterKey(t)
	entries := sampleEntries()
	envelope := sealEnvelope(t, key, entries)

	t.Setenv(secret.EnvMasterKey, key)
	got, err := OpenBytes(envelope)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("entry count: got %d want %d", len(got), len(entries))
	}
	for i := range entries {
		if got[i].Name != entries[i].Name || got[i].Value != entries[i].Value {
			t.Fatalf("entry %d mismatch: got %+v want %+v", i, got[i], entries[i])
		}
	}
}
