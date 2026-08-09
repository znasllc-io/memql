package identity

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// JWKS represents a JSON Web Key Set in the format consumers expect
// at `/.well-known/jwks.json`. We expose only Ed25519 keys.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is the per-key entry in JWKS. Fields and conventions follow
// RFC 7517 plus RFC 8037 for the Ed25519 ("OKP" / "Ed25519") binding.
type JWK struct {
	Kty string `json:"kty"`           // "OKP"
	Alg string `json:"alg,omitempty"` // "EdDSA"
	Use string `json:"use,omitempty"` // "sig"
	Crv string `json:"crv"`           // "Ed25519"
	Kid string `json:"kid"`
	X   string `json:"x"` // base64url(Public)
}

// BuildJWKS builds a JWKS document from a KeyManager's current +
// previous keys. Pure function — easy to unit-test.
func BuildJWKS(km *KeyManager, now time.Time) JWKS {
	keys := km.PublicKeySet(now)
	out := JWKS{Keys: make([]JWK, 0, len(keys))}
	for _, k := range keys {
		out.Keys = append(out.Keys, JWK{
			Kty: "OKP",
			Alg: "EdDSA",
			Use: "sig",
			Crv: "Ed25519",
			Kid: k.KID,
			X:   k.PublicB64,
		})
	}
	return out
}

// EmitKeysetMetric publishes the key set this identity replica currently
// serves (count + stable fingerprint) to the Prometheus surface. Called
// at startup and on every JWKS serve so the gauge tracks what is actually
// published. Two identity replicas that serve DIFFERENT key sets report
// different fingerprints, so a cross-replica `max != min` over
// memql_jwks_keyset_fingerprint is the JWKS-incoherence alert signal that
// the 2026-06-16 silent auth failure lacked (memql#1523).
// It also publishes the signing-key AGE surface (memql#3381): when the
// active key was created, whether that date is trustworthy, and whether
// this process can rotate at all. Rotation in a deployed cluster is a
// manual re-seal-and-roll (see KeyManager.RotationSupported), so an
// age gauge an operator can alert on is the only automated pressure
// toward ever performing it.
func EmitKeysetMetric(km *KeyManager, now time.Time) {
	doc := BuildJWKS(km, now)
	kids := make([]string, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		kids = append(kids, k.Kid)
	}
	metrics.SetJWKSKeyset(kids)

	createdAt, known := km.CurrentKeyCreatedAt()
	metrics.SetIdentitySigningKey(createdAt, known, km.RotationSupported())
}

// JWKSHandler returns an http.Handler that serves the live JWKS
// document. Cache-Control is set short — verifiers should refresh
// when they encounter an unknown kid (the recommended pattern with
// jwx / golang-jwt JWKS caches), so a low TTL keeps rotation fast.
func JWKSHandler(km *KeyManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		EmitKeysetMetric(km, now)
		body, err := json.Marshal(BuildJWKS(km, now))
		if err != nil {
			http.Error(w, "marshal jwks", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		// CORS for JWKS is normally permissive — verifiers run on
		// every node in the cluster plus, eventually, third-party
		// integrations.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(body)
	})
}
