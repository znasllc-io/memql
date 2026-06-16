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
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		authRejectsTotal,
		jwksKeysetKeys,
		jwksKeysetFingerprint,
	)
	// Explicit zero so the series exists before the first keyset is
	// observed; an alert on a missing series is harder to reason about
	// than one on a stable 0.
	jwksKeysetFingerprint.Set(0)
	jwksKeysetKeys.Set(0)
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
