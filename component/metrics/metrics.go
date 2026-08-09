// Package metrics exposes the process-wide Prometheus metrics surface
// for every memQL binary (bff / voice / cognition / agent / planner /
// workbench / identity).
//
// Historically memQL emitted no Prometheus metrics at all -- auth
// rejects and JWKS incoherence were WARN-level slog lines only, so a
// ~50% auth-failure incident (2026-06-16) stayed silent until a user
// noticed the product was broken (memql#1523). This package is the
// minimal counter/gauge surface that lets a Prometheus alert page on
// those conditions within minutes:
//
//   - memql_auth_rejects_total  -- counter, labelled by surface /
//     reason / code. The auth-reject-rate alert sums the unknown_kid
//     and invalid_token reasons.
//   - memql_jwks_keyset_keys        -- gauge, key count this process
//     serves (identity) or trusts (verifier).
//   - memql_jwks_keyset_fingerprint -- gauge, a stable numeric
//     fingerprint over the sorted kid set. Two identity replicas that
//     serve DIFFERENT key sets report different fingerprints, so a
//     cross-replica `max != min` is the JWKS-incoherence signal.
//   - memql_identity_signing_key_created_timestamp_seconds,
//     memql_identity_signing_key_age_known,
//     memql_identity_signing_key_rotation_supported -- the signing-key
//     age surface (memql#3381). See SetIdentitySigningKey.
//
// Everything registers on a package-local registry (not the global
// default) so tests stay isolated and the /metrics endpoint is fully
// explicit. The standard Go + process collectors are registered too so
// the endpoint is a complete scrape target.
package metrics

import (
	"crypto/sha256"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

const namespace = "memql"

// Auth-reject surface labels. The surface identifies WHICH interceptor
// rejected the call, so an operator can tell a mesh-token storm
// (surface="node") from a browser/user storm (surface="grpc").
const (
	SurfaceGRPC = "grpc" // user-facing MemqlService.Stream interceptor
	SurfaceHTTP = "http" // verifier HTTP middleware
	SurfaceNode = "node" // NodeService.Stream mesh interceptor
	// SurfaceNodeForward is the APPLICATION-layer forwarded-auth gate on an
	// AiForwardRequest (memql#3205), deliberately distinct from SurfaceNode.
	// That one means the mesh TRANSPORT interceptor rejected a node token, and
	// an alert watches it for a mesh-token storm. A forwarded-authority refusal
	// is a different event with a different remedy -- usually a version skew
	// mid-rollout -- and folding the two would fire that alert on it.
	SurfaceNodeForward = "node_forward"
)

// Auth-reject reason labels. Keep these low-cardinality + stable so the
// alert query can name them directly.
const (
	ReasonUnknownKID           = "unknown_kid"            // JWKS has no key for the token's kid (the #1523 symptom)
	ReasonInvalidToken         = "invalid_token"          // signature / issuer / audience / expiry / parse failure
	ReasonMissingToken         = "missing_token"          // no / malformed Authorization metadata
	ReasonTokenRevoked         = "token_revoked"          // revocation-epoch advanced past the token claim
	ReasonRevocationCheckError = "revocation_check_error" // epoch resolver itself failed
	ReasonWrongClass           = "wrong_class"            // valid token, wrong JWT class for this surface
	ReasonMissingBinding       = "missing_binding"        // node-class token missing node_id / node_type

	// Mesh forwarded-auth refusals (memql#3205), on SurfaceNodeForward.
	ReasonForwardAuthorityMissing    = "forward_authority_missing"    // absent / unclassed / unknown-class assertion
	ReasonForwardCeilingNotApplied   = "forward_ceiling_not_applied"  // badge role exceeds its ceiling, or the ceiling is missing/stray
	ReasonForwardExpired             = "forward_expired"              // the asserted grant has expired, or carries no expiry to check
	ReasonForwardPrincipalMismatch   = "forward_principal_mismatch"   // kind/class/subject disagree, or claims drifted from the assertion
	ReasonForwardUnsupportedContract = "forward_unsupported_contract" // producer speaks a contract version this receiver does not implement
)

// gRPC status code labels (string form of the codes returned to the
// caller). Kept as a label so the alert can separate authn
// (Unauthenticated) from authz (PermissionDenied) storms.
const (
	CodeUnauthenticated  = "Unauthenticated"
	CodePermissionDenied = "PermissionDenied"
)

var (
	registry = prometheus.NewRegistry()

	authRejectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "auth",
			Name:      "rejects_total",
			Help:      "Total authentication/authorization rejects, labelled by surface (grpc/http/node), reason, and gRPC status code.",
		},
		[]string{"surface", "reason", "code"},
	)

	jwksKeysetKeys = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "jwks",
		Name:      "keyset_keys",
		Help:      "Number of signing keys in the JWKS this process currently serves (identity) or trusts (verifier).",
	})

	jwksKeysetFingerprint = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "jwks",
		Name:      "keyset_fingerprint",
		Help:      "Stable numeric fingerprint (top 52 bits of sha256 over the sorted kid set) of the JWKS this process serves/trusts. Cross-replica disagreement (max != min over identity replicas) signals JWKS incoherence.",
	})

	authEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "auth",
		Name:      "enabled",
		Help:      "1 when authentication is enforced on this node, 0 when disabled via MEMQL_IDENTITY_ENABLED=false (troubleshooting only -- never staging/prod). Alert on any 0.",
	})

	identitySigningKeyCreatedTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "identity",
		Name:      "signing_key_created_timestamp_seconds",
		Help:      "Unix timestamp at which the ACTIVE Ed25519 signing key was created. Key age is time() minus this. Meaningful only while memql_identity_signing_key_age_known is 1; it is 0 otherwise.",
	})

	identitySigningKeyAgeKnown = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "identity",
		Name:      "signing_key_age_known",
		Help:      "1 when this replica knows when its active signing key was really created, 0 when it does not. An env-provided seed (MEMQL_IDENTITY_SIGNING_KEY_B64) carries no creation date, so without MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT the only date available is process start -- which measures pod uptime, not key age. Alert on 0: key age is UNOBSERVABLE, not zero.",
	})

	identitySigningKeyRotationSupported = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "identity",
		Name:      "signing_key_rotation_supported",
		Help:      "1 when this replica can rotate its own signing key (on-disk MEMQL_IDENTITY_KEY_DIR mode, dev), 0 when it cannot (env-seed mode -- staging/prod). At 0 the in-process 90-day rotation scheduler is INERT and rotation is the manual re-seal-and-roll runbook (memql#3381).",
	})
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		authRejectsTotal,
		jwksKeysetKeys,
		jwksKeysetFingerprint,
		authEnabled,
		identitySigningKeyCreatedTimestamp,
		identitySigningKeyAgeKnown,
		identitySigningKeyRotationSupported,
	)
	// Explicit zero so the series exists before the first keyset is
	// observed; an alert on a missing series is harder to reason about
	// than one on a stable 0.
	jwksKeysetFingerprint.Set(0)
	jwksKeysetKeys.Set(0)
	// Auth is enforced by default; app/configAndAuth pins this to 0 when
	// the master toggle disables auth for troubleshooting.
	authEnabled.Set(1)
	// The signing-key series are written ONLY by the identity binary
	// (identity.EmitKeysetMetric). Every other node exports them at these
	// zeros, which is honest -- a bff holds no signing key -- so every
	// alert over them must select app="identity".
	identitySigningKeyCreatedTimestamp.Set(0)
	identitySigningKeyAgeKnown.Set(0)
	identitySigningKeyRotationSupported.Set(0)
}

// SetIdentitySigningKey publishes the age surface for the ACTIVE Ed25519
// signing key (memql#3381). Called by the identity service at startup, on
// every JWKS serve, and after a rotation.
//
// createdAtKnown is the load-bearing argument. In env-seed mode
// (MEMQL_IDENTITY_SIGNING_KEY_B64) the seed is 32 bytes and nothing else --
// it carries no creation date, and the KeyManager fills CreatedAt with
// time.Now() at construction. Publishing THAT as the key's creation date
// would make a five-year-old key report an age of "since the last pod
// restart": a metric that always looks healthy in exactly the environments
// where rotation never happens, which is the same false-signal shape as the
// dormant scheduler this metric exists to compensate for. So when the
// operator has not stamped MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT, age is
// reported as UNKNOWN (age_known=0, timestamp=0) rather than as zero.
func SetIdentitySigningKey(createdAt time.Time, createdAtKnown, rotationSupported bool) {
	if createdAtKnown && !createdAt.IsZero() {
		identitySigningKeyCreatedTimestamp.Set(float64(createdAt.Unix()))
		identitySigningKeyAgeKnown.Set(1)
	} else {
		identitySigningKeyCreatedTimestamp.Set(0)
		identitySigningKeyAgeKnown.Set(0)
	}
	if rotationSupported {
		identitySigningKeyRotationSupported.Set(1)
	} else {
		identitySigningKeyRotationSupported.Set(0)
	}
}

// IdentitySigningKeyValues returns the current
// (createdTimestampSeconds, ageKnown, rotationSupported) gauge values.
// An introspection/testing aid; production code only ever calls
// SetIdentitySigningKey.
func IdentitySigningKeyValues() (createdTimestamp, ageKnown, rotationSupported float64) {
	return gaugeValue(identitySigningKeyCreatedTimestamp),
		gaugeValue(identitySigningKeyAgeKnown),
		gaugeValue(identitySigningKeyRotationSupported)
}

func gaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

// SetAuthEnabled records whether authentication is enforced on this node.
// Called once at boot: 1 in the normal (verifier) path, 0 when
// MEMQL_IDENTITY_ENABLED=false selects the local-dev no-auth path.
func SetAuthEnabled(enabled bool) {
	if enabled {
		authEnabled.Set(1)
		return
	}
	authEnabled.Set(0)
}

// Registry returns the process-wide Prometheus registry. Exposed for
// tests and for callers that want to register additional collectors.
func Registry() *prometheus.Registry { return registry }

// Handler returns the http.Handler serving the Prometheus text exposition
// format. Mounted at GET /metrics by the app bootstrap.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// AuthReject records one authentication/authorization reject. Safe to
// call on every reject path; the label set is fixed and low-cardinality.
// Use the Surface*, Reason*, and Code* constants for the arguments.
func AuthReject(surface, reason, code string) {
	authRejectsTotal.WithLabelValues(surface, reason, code).Inc()
}

// AuthRejectValue returns the current value of the auth-reject counter
// for the given label set, or 0 when the series has no observations yet.
// An introspection/testing aid -- prod code only ever calls AuthReject.
func AuthRejectValue(surface, reason, code string) float64 {
	c, err := authRejectsTotal.GetMetricWithLabelValues(surface, reason, code)
	if err != nil {
		return 0
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// SetJWKSKeyset records the JWKS this process currently serves (identity
// service) or trusts (per-node verifier): the key count plus a stable
// numeric fingerprint over the sorted set of key ids. Idempotent -- call
// it whenever the keyset is (re)loaded, rotated, or refreshed.
func SetJWKSKeyset(kids []string) {
	jwksKeysetKeys.Set(float64(len(kids)))
	jwksKeysetFingerprint.Set(KeysetFingerprint(kids))
}

// KeysetFingerprint computes the stable numeric fingerprint used by the
// jwks keyset_fingerprint gauge. Exported so tests (and any future
// cross-replica coherence check) can compute the same value. The result
// is the top 52 bits of sha256 over the NUL-delimited sorted kid set,
// which is exactly representable as a float64 (Prometheus gauges carry
// float64). The empty set maps to 0.
func KeysetFingerprint(kids []string) float64 {
	if len(kids) == 0 {
		return 0
	}
	sorted := make([]string, len(kids))
	copy(sorted, kids)
	sort.Strings(sorted)

	h := sha256.New()
	for _, k := range sorted {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0}) // delimiter so {"ab","c"} != {"a","bc"}
	}
	sum := h.Sum(nil)

	var v uint64
	for i := 0; i < 8; i++ {
		v = (v << 8) | uint64(sum[i])
	}
	v &= (uint64(1) << 52) - 1 // keep float64-exact
	return float64(v)
}
