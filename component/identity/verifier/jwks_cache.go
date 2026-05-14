package verifier

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// JWK is the on-the-wire representation of a single key in the JWKS
// document. The identity service publishes Ed25519 keys only, so this
// is the OKP/Ed25519 binding from RFC 8037.
type JWK struct {
	Kty string `json:"kty"`           // "OKP"
	Alg string `json:"alg,omitempty"` // "EdDSA"
	Use string `json:"use,omitempty"` // "sig"
	Crv string `json:"crv"`           // "Ed25519"
	Kid string `json:"kid"`
	X   string `json:"x"` // base64url(public)
}

// JWKS is a JSON Web Key Set as served at /.well-known/jwks.json.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKSCache fetches and caches the identity service's public keyset.
// Concurrent-safe. Background goroutine refreshes on the configured
// cadence; ForceRefresh runs an additional refresh on demand (used
// when a token's kid isn't in the current cache — the key may have
// been published since the last poll).
type JWKSCache struct {
	url        string
	httpClient *http.Client
	logger     *slog.Logger
	refreshIv  time.Duration

	mu        sync.RWMutex
	keys      map[string]ed25519.PublicKey
	lastFetch time.Time

	// inflight serializes ForceRefresh calls so a burst of unknown-kid
	// tokens doesn't fan out into a burst of HTTP requests.
	inflight sync.Mutex
}

// NewJWKSCache builds a cache for the given URL. Performs an initial
// fetch synchronously so the verifier can fail-fast on a misconfig
// (wrong URL, identity service unreachable) at startup. Returns the
// initial-fetch error wrapped if it fails.
//
// Callers must call Start to begin background refreshes.
func NewJWKSCache(cfg Config, logger *slog.Logger) (*JWKSCache, error) {
	url := cfg.EffectiveJWKSURL()
	if url == "" {
		return nil, errors.New("verifier.NewJWKSCache: empty JWKS URL (IDENTITY_VERIFIER_BASE_URL not set?)")
	}
	c := &JWKSCache{
		url:        url,
		httpClient: &http.Client{Timeout: cfg.JWKSFetchTimeout},
		logger:     logger,
		refreshIv:  cfg.JWKSRefreshInterval,
		keys:       make(map[string]ed25519.PublicKey),
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.JWKSFetchTimeout)
	defer cancel()
	if err := c.refresh(ctx); err != nil {
		return nil, fmt.Errorf("verifier.NewJWKSCache: initial fetch from %s: %w", url, err)
	}
	return c, nil
}

// Start launches the background refresh goroutine. Calling Start
// multiple times is safe — the goroutine returns when ctx is done.
func (c *JWKSCache) Start(ctx context.Context) {
	if c == nil || c.refreshIv <= 0 {
		return
	}
	go c.runBackground(ctx)
}

// PublicKey returns the cached Ed25519 public key for the given kid.
// Returns nil + false when the kid is not in the cache. Callers can
// then trigger ForceRefresh to handle the rotation-overlap case
// (a token signed under a freshly published key that hasn't been
// polled into this cache yet).
func (c *JWKSCache) PublicKey(kid string) (ed25519.PublicKey, bool) {
	if c == nil || kid == "" {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.keys[kid]
	return k, ok
}

// ForceRefresh triggers an immediate JWKS fetch outside the background
// cadence. Serialized via inflight so concurrent callers coalesce into
// a single network request. Errors are logged and returned; callers
// typically log and continue (the original "unknown kid" error is the
// authoritative failure surface for the request).
func (c *JWKSCache) ForceRefresh(ctx context.Context) error {
	if c == nil {
		return errors.New("verifier.JWKSCache: nil receiver")
	}
	c.inflight.Lock()
	defer c.inflight.Unlock()
	return c.refresh(ctx)
}

// LastFetch reports the timestamp of the most recent successful
// refresh. Useful for health checks and debugging.
func (c *JWKSCache) LastFetch() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFetch
}

// KeyCount returns the number of keys currently in the cache. Useful
// for the same diagnostic surface as LastFetch.
func (c *JWKSCache) KeyCount() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.keys)
}

func (c *JWKSCache) runBackground(ctx context.Context) {
	t := time.NewTicker(c.refreshIv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
			if err := c.refresh(rctx); err != nil {
				c.warn("jwks_cache: background refresh failed", "error", err, "url", c.url)
			}
			cancel()
		}
	}
}

func (c *JWKSCache) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	var doc JWKS
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			c.warn("jwks_cache: skipping malformed JWK", "kid", k.Kid, "error", err)
			continue
		}
		if len(raw) != ed25519.PublicKeySize {
			c.warn("jwks_cache: skipping JWK with wrong key length", "kid", k.Kid, "len", len(raw))
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}

	c.mu.Lock()
	c.keys = keys
	c.lastFetch = time.Now().UTC()
	c.mu.Unlock()

	c.debug("jwks_cache: refreshed", "key_count", len(keys), "url", c.url)
	return nil
}

func (c *JWKSCache) warn(msg string, args ...any) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Warn(msg, args...)
}

func (c *JWKSCache) debug(msg string, args ...any) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Debug(msg, args...)
}
