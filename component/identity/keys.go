package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// KeyFile names. The current key signs new tokens; the previous key
// stays around in JWKS for the rotation overlap window so in-flight
// access tokens still verify.
const (
	currentKeyFilename  = "jwt-current.ed25519"
	previousKeyFilename = "jwt-previous.ed25519"
	keyFileMode         = 0o600
	keyDirMode          = 0o700
)

// argon2id parameters tuned for "wrap an env-var-derived key once at
// startup, fast enough not to delay boot, slow enough to make brute-
// force on a leaked encrypted file expensive." OWASP 2024 minimums.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32 // AES-256 key
	argonSaltLen        = 16
)

// KeyMaterial is one Ed25519 keypair plus its identifying metadata.
// kid (key id) is the SHA-256 prefix of the public key, used as the
// JWT `kid` header and the JWKS `kid` field so verifiers can pick
// the right key.
type KeyMaterial struct {
	KID        string             `json:"kid"`
	Public     ed25519.PublicKey  `json:"-"`
	Private    ed25519.PrivateKey `json:"-"`
	CreatedAt  time.Time          `json:"createdAt"`
	RetiresAt  time.Time          `json:"retiresAt,omitempty"`
	PublicB64  string             `json:"publicB64"`
	PrivateB64 string             `json:"-"`
}

// keyEnvelope is the on-disk representation: the private key bytes
// optionally wrapped in AES-256-GCM keyed off the operator's
// MEMQL_IDENTITY_KEY_ENCRYPTION_KEY. metadata fields are stored alongside
// so we don't have to re-derive them on load.
type keyEnvelope struct {
	Version    int       `json:"version"`
	KID        string    `json:"kid"`
	CreatedAt  time.Time `json:"createdAt"`
	RetiresAt  time.Time `json:"retiresAt,omitempty"`
	PublicB64  string    `json:"publicB64"`
	PrivateB64 string    `json:"privateB64,omitempty"` // plaintext (dev mode)
	// Encrypted form (production mode):
	EncSalt    string `json:"encSalt,omitempty"`
	EncNonce   string `json:"encNonce,omitempty"`
	EncPayload string `json:"encPayload,omitempty"`
}

const keyEnvelopeVersion = 1

// KeyManager owns the on-disk keypair files and the in-memory cache
// of the current + previous key material. Concurrent-safe.
//
// In envMode (NewKeyManagerFromSeed, #550) the current key is derived
// from an env-provided seed and there is no disk: Load is a no-op,
// Rotate is refused, and there is never a previous key. This is what
// lets identity run >=2 replicas -- every replica handed the same seed
// derives the same key + kid + JWKS, so there's no single-writer PVC.
type KeyManager struct {
	dir              string
	encryptionSecret string
	envMode          bool

	mu       sync.RWMutex
	current  *KeyMaterial
	previous *KeyMaterial
}

// NewKeyManagerFromSeed builds a KeyManager whose signing key is the
// Ed25519 keypair derived from a base64 (std) 32-byte seed, loaded from
// the environment rather than a PVC. Deterministic: the same seed always
// yields the same keypair + kid, so multiple identity replicas agree on
// JWKS. Rotation is disabled in this mode.
func NewKeyManagerFromSeed(seedB64 string) (*KeyManager, error) {
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("identity: MEMQL_IDENTITY_SIGNING_KEY_B64 is not valid base64: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: MEMQL_IDENTITY_SIGNING_KEY_B64 must decode to %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &KeyManager{
		envMode: true,
		current: materializeKey(pub, priv, time.Now().UTC()),
	}, nil
}

// RotationSupported reports whether automatic key rotation is available.
// False in envMode (the key is sealed in the envelope; rotate by
// re-sealing + rolling).
func (km *KeyManager) RotationSupported() bool { return !km.envMode }

// NewKeyManager prepares a KeyManager rooted at dir. dir is created
// if it doesn't exist. The encryptionSecret must be either empty
// (plaintext storage, dev only) or at least 16 bytes.
func NewKeyManager(dir, encryptionSecret string) (*KeyManager, error) {
	if dir == "" {
		return nil, errors.New("identity: KeyManager dir is required")
	}
	if encryptionSecret != "" && len(encryptionSecret) < 16 {
		return nil, errors.New("identity: encryption secret must be at least 16 bytes")
	}
	if err := os.MkdirAll(dir, keyDirMode); err != nil {
		return nil, fmt.Errorf("identity: create key dir %q: %w", dir, err)
	}
	return &KeyManager{
		dir:              dir,
		encryptionSecret: encryptionSecret,
	}, nil
}

// Load reads the current and (if present) previous keypair files into
// memory. If no current key exists, generates one and writes it. Safe
// to call repeatedly; a no-op when both keys are already loaded.
func (km *KeyManager) Load() error {
	km.mu.Lock()
	defer km.mu.Unlock()

	// envMode: the key was derived from the env seed at construction;
	// there is no disk to read and nothing to generate.
	if km.envMode {
		return nil
	}

	cur, err := km.loadKeyFile(currentKeyFilename)
	switch {
	case err == nil:
		km.current = cur
	case errors.Is(err, os.ErrNotExist):
		generated, gerr := km.generateAndWriteCurrent()
		if gerr != nil {
			return gerr
		}
		km.current = generated
	default:
		return fmt.Errorf("identity: load current key: %w", err)
	}

	prev, err := km.loadKeyFile(previousKeyFilename)
	switch {
	case err == nil:
		km.previous = prev
	case errors.Is(err, os.ErrNotExist):
		// No prior rotation has happened — nothing to load.
	default:
		return fmt.Errorf("identity: load previous key: %w", err)
	}

	return nil
}

// Current returns a snapshot of the active signing key. nil before
// Load() has been called.
func (km *KeyManager) Current() *KeyMaterial {
	km.mu.RLock()
	defer km.mu.RUnlock()
	if km.current == nil {
		return nil
	}
	cp := *km.current
	return &cp
}

// Previous returns the retiring key during the JWKS overlap window,
// or nil if no rotation is in progress.
func (km *KeyManager) Previous() *KeyMaterial {
	km.mu.RLock()
	defer km.mu.RUnlock()
	if km.previous == nil {
		return nil
	}
	cp := *km.previous
	return &cp
}

// PublicKeySet returns the keys that should appear in the JWKS
// endpoint right now. Always at minimum the current key; includes
// the previous key only while it's still inside the overlap window.
func (km *KeyManager) PublicKeySet(now time.Time) []*KeyMaterial {
	km.mu.RLock()
	defer km.mu.RUnlock()
	out := make([]*KeyMaterial, 0, 2)
	if km.current != nil {
		cur := *km.current
		out = append(out, &cur)
	}
	if km.previous != nil && (km.previous.RetiresAt.IsZero() || now.Before(km.previous.RetiresAt)) {
		prev := *km.previous
		out = append(out, &prev)
	}
	return out
}

// Rotate generates a new keypair, makes it the current key, demotes
// the previous-current to previous (with a retirement timestamp set
// overlapWindow into the future), and writes both to disk. Safe to
// call repeatedly — the rotation cron and the admin "Rotate now"
// button both end up here.
func (km *KeyManager) Rotate(overlapWindow time.Duration) (*KeyMaterial, error) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if km.envMode {
		return nil, errors.New("identity: key rotation is disabled when the signing key is env-provided (MEMQL_IDENTITY_SIGNING_KEY_B64); re-seal the envelope with a new seed and roll the deployment to rotate")
	}

	now := time.Now().UTC()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate Ed25519 keypair: %w", err)
	}
	newKey := materializeKey(pub, priv, now)

	// Move the current key into "previous" with a retirement deadline.
	if km.current != nil {
		retired := *km.current
		retired.RetiresAt = now.Add(overlapWindow)
		km.previous = &retired
		if err := km.writeKeyFile(previousKeyFilename, &retired); err != nil {
			return nil, fmt.Errorf("identity: write previous key: %w", err)
		}
	}

	km.current = newKey
	if err := km.writeKeyFile(currentKeyFilename, newKey); err != nil {
		return nil, fmt.Errorf("identity: write current key: %w", err)
	}

	cp := *newKey
	return &cp, nil
}

// SweepRetiredPrevious removes the previous key from JWKS once its
// retirement window has elapsed. Called periodically by the rotation
// goroutine. Returns true if a key was actually swept.
func (km *KeyManager) SweepRetiredPrevious(now time.Time) (bool, error) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if km.envMode || km.previous == nil {
		return false, nil
	}
	if !km.previous.RetiresAt.IsZero() && now.Before(km.previous.RetiresAt) {
		return false, nil
	}
	km.previous = nil
	path := filepath.Join(km.dir, previousKeyFilename)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("identity: remove retired previous key file: %w", err)
	}
	return true, nil
}

// generateAndWriteCurrent is called once on the very first start of
// a fresh deployment when no keypair exists yet.
func (km *KeyManager) generateAndWriteCurrent() (*KeyMaterial, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate Ed25519 keypair: %w", err)
	}
	mat := materializeKey(pub, priv, time.Now().UTC())
	if err := km.writeKeyFile(currentKeyFilename, mat); err != nil {
		return nil, fmt.Errorf("identity: write fresh key: %w", err)
	}
	return mat, nil
}

// materializeKey builds a KeyMaterial with kid + base64 fields filled
// in. Centralized so every callsite produces the same shape.
func materializeKey(pub ed25519.PublicKey, priv ed25519.PrivateKey, createdAt time.Time) *KeyMaterial {
	hash := sha256.Sum256(pub)
	return &KeyMaterial{
		KID:        base64.RawURLEncoding.EncodeToString(hash[:8]),
		Public:     pub,
		Private:    priv,
		CreatedAt:  createdAt,
		PublicB64:  base64.RawURLEncoding.EncodeToString(pub),
		PrivateB64: base64.RawURLEncoding.EncodeToString(priv),
	}
}

// loadKeyFile reads + decodes one key file. Returns os.ErrNotExist
// (wrapped) when the file isn't there so callers can distinguish
// "fresh deploy" from "real I/O error."
func (km *KeyManager) loadKeyFile(name string) (*KeyMaterial, error) {
	path := filepath.Join(km.dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env keyEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Version != keyEnvelopeVersion {
		return nil, fmt.Errorf("unsupported key envelope version %d", env.Version)
	}

	var privBytes []byte
	switch {
	case env.PrivateB64 != "":
		// Plaintext — dev path.
		privBytes, err = base64.RawURLEncoding.DecodeString(env.PrivateB64)
		if err != nil {
			return nil, fmt.Errorf("decode plaintext private: %w", err)
		}
	case env.EncPayload != "":
		if km.encryptionSecret == "" {
			return nil, errors.New("encrypted key on disk but MEMQL_IDENTITY_KEY_ENCRYPTION_KEY is empty")
		}
		privBytes, err = decryptPrivateKey(env, km.encryptionSecret)
		if err != nil {
			return nil, fmt.Errorf("decrypt private key: %w", err)
		}
	default:
		return nil, errors.New("envelope contains neither plaintext nor encrypted private key")
	}

	pubBytes, err := base64.RawURLEncoding.DecodeString(env.PublicB64)
	if err != nil {
		return nil, fmt.Errorf("decode public: %w", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key length %d", len(pubBytes))
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key length %d", len(privBytes))
	}

	return &KeyMaterial{
		KID:        env.KID,
		Public:     ed25519.PublicKey(pubBytes),
		Private:    ed25519.PrivateKey(privBytes),
		CreatedAt:  env.CreatedAt,
		RetiresAt:  env.RetiresAt,
		PublicB64:  env.PublicB64,
		PrivateB64: base64.RawURLEncoding.EncodeToString(privBytes),
	}, nil
}

func (km *KeyManager) writeKeyFile(name string, mat *KeyMaterial) error {
	env := keyEnvelope{
		Version:   keyEnvelopeVersion,
		KID:       mat.KID,
		CreatedAt: mat.CreatedAt,
		RetiresAt: mat.RetiresAt,
		PublicB64: mat.PublicB64,
	}
	if km.encryptionSecret == "" {
		env.PrivateB64 = mat.PrivateB64
	} else {
		enc, err := encryptPrivateKey(mat.Private, km.encryptionSecret)
		if err != nil {
			return fmt.Errorf("encrypt private key: %w", err)
		}
		env.EncSalt = enc.salt
		env.EncNonce = enc.nonce
		env.EncPayload = enc.payload
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	path := filepath.Join(km.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, keyFileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// encryptedPrivateKey is the AES-256-GCM-wrapped form of a private
// key for at-rest storage.
type encryptedPrivateKey struct {
	salt    string
	nonce   string
	payload string
}

func encryptPrivateKey(priv ed25519.PrivateKey, secret string) (encryptedPrivateKey, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return encryptedPrivateKey{}, err
	}
	dk := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	block, err := aes.NewCipher(dk)
	if err != nil {
		return encryptedPrivateKey{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedPrivateKey{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return encryptedPrivateKey{}, err
	}
	ct := gcm.Seal(nil, nonce, priv, nil)

	return encryptedPrivateKey{
		salt:    base64.RawStdEncoding.EncodeToString(salt),
		nonce:   base64.RawStdEncoding.EncodeToString(nonce),
		payload: base64.RawStdEncoding.EncodeToString(ct),
	}, nil
}

func decryptPrivateKey(env keyEnvelope, secret string) ([]byte, error) {
	salt, err := base64.RawStdEncoding.DecodeString(env.EncSalt)
	if err != nil {
		return nil, fmt.Errorf("decode salt: %w", err)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(env.EncNonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	ct, err := base64.RawStdEncoding.DecodeString(env.EncPayload)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	dk := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM open (likely wrong MEMQL_IDENTITY_KEY_ENCRYPTION_KEY): %w", err)
	}
	return pt, nil
}
