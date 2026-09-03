// Package metrics exposes the process-wide Prometheus metrics surface
// for every MemQL binary (bff / voice / cognition / agent / planner /
// workbench / identity).
//
// Historically MemQL emitted no Prometheus metrics at all -- auth
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
		Help:      "IDENTITY NODES ONLY -- select app=\"identity\"; every other node type exports this series at a constant 0 because it holds no signing key. Unix timestamp at which the ACTIVE Ed25519 signing key was created. Key age is time() minus this. Meaningful only while memql_identity_signing_key_age_known is 1; it is 0 otherwise.",
	})

	identitySigningKeyAgeKnown = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "identity",
		Name:      "signing_key_age_known",
		Help:      "IDENTITY NODES ONLY -- select app=\"identity\"; every other node type exports this series at a constant 0 because it holds no signing key, so an unqualified alert fires forever on most of the mesh and buries the one case this measures. 1 when this replica knows when its active signing key was really created, 0 when it does not. An env-provided seed (MEMQL_IDENTITY_SIGNING_KEY_B64) carries no creation date, so without MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT the only date available is process start -- which measures pod uptime, not key age. Alert on 0 WHERE app=\"identity\": key age is UNOBSERVABLE, not zero.",
	})

	subscriptionRowsDenied = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "subscription",
			Name:      "rows_denied_total",
			Help:      "Graph-subscription events dropped at fan-out because the subscribing stream's actor may not read the row, labelled by concept. Row admission on subscriptions landed in memql#4309; before it, a signed-in stream received every user's rows for any concept it subscribed to. A steady non-zero rate is NORMAL and means the gate is working -- it counts rows a stream asked for and was not entitled to. Alert on a SUSTAINED SPIKE against its own baseline, which is a client subscribing far wider than it can read, not on any non-zero value.",
		},
		[]string{"concept"},
	)

	authActivityPruned = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "auth",
		Name:      "activity_pruned_total",
		Help:      "IDENTITY NODES ONLY -- select app=\"identity\"; every other node type exports this at a constant 0 because only the identity node runs the sweep. Rows hard-deleted from v1:identity:authActivity by the daily retention job, past MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS (default 30). Unlike v1:identity:auditEvent's observe-only sweep, this one really deletes, so the counter measures work done rather than work identified. A steady non-zero rate is NORMAL and is what the job existing looks like. Alert on a FLAT ZERO over more than a day on a cluster that authenticates anyone -- that means the sweep is not running, and the first thing to break is refresh-token REUSE DETECTION, which reaches back exactly as far as this window and degrades silently to \"stale cookie\" when the rows it keys on are neither pruned nor present.",
	})
	aiFederationExchanges = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "ai",
			Name:      "federation_exchanges_total",
			Help:      "Anthropic workload identity federation token exchanges (POST /v1/oauth/token), labelled by outcome: ok, denied (Anthropic refused the assertion -- 4xx; the reason is in the warn log and in the Console's authentication-events tab), error (the exchange never got an answer, or got a 5xx). This is NOT an LLM call: it is deliberately outside the guard's fingerprint, so it counts toward no rate ceiling and no cost budget (memql#4335). A steady low rate of `ok` is the healthy shape -- the SDK re-exchanges as a one-hour token nears expiry, so roughly one per token lifetime per client. ANY sustained `denied` means the cluster is running on a credential Anthropic will not renew: alert on it, because the last good token keeps working until it expires and the outage arrives up to an hour after the cause.",
		},
		[]string{"outcome"},
	)

	identitySigningKeyRotationSupported = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "identity",
		Name:      "signing_key_rotation_supported",
		Help:      "IDENTITY NODES ONLY -- select app=\"identity\"; every other node type exports this series at a constant 0 because it holds no signing key. 1 when this replica can rotate its own signing key (on-disk MEMQL_IDENTITY_KEY_DIR mode, dev), 0 when it cannot (env-seed mode -- staging/prod). At 0 the in-process 90-day rotation scheduler is INERT and rotation is the manual re-seal-and-roll runbook (memql#3381).",
	})

	// The log store (epic memql#4893): every node's log lines, persisted in
	// the log_line hypertable by component/logstore.Sink. Four series, all
	// written on EVERY NODE TYPE, so no app= selector is needed to read them.
	logsWrittenTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "logs",
		Name:      "written_total",
		Help:      "EVERY NODE TYPE writes this series: log lines this node persisted into the log_line hypertable (v1:observability:logLine) since it started. A steady non-zero rate is what a healthy store looks like on any node that logs at or above MEMQL_LOGS_LEVEL. Alert on a FLAT ZERO across the whole mesh for more than a few minutes: nothing is being kept, and the Logs app is showing history rather than the present.",
	})

	logsDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "logs",
			Name:      "dropped_total",
			Help:      "EVERY NODE TYPE writes this series: log lines this node did NOT persist, by reason. queue: the 4096-line queue was full (the database is slower than the node logs). rate: the per-node bucket (MEMQL_LOGS_MAX_LINES_PER_SECOND, default 2000) was empty. level: the line was below MEMQL_LOGS_LEVEL on a path the handler does not pre-filter (the OS write). db: a batch insert failed and the whole batch was lost. Every drop is a gap in the Logs app that the app itself reports once a minute. Alert on ANY sustained rate of reason=\"db\" -- the store is not reaching its table -- and on reason=\"queue\" above a few per second, which is the database falling behind.",
		},
		[]string{"reason"},
	)

	logsArchivedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "logs",
		Name:      "archived_total",
		Help:      "EVERY NODE TYPE exports this series, but only the node that ran the nightly retention sweep (the cron leader, or an owner calling logsSweep by hand) ever moves it; the rest export a constant 0. Log lines the sweep wrote into the archive container as logs/<day>/<nodeType>.ndjson.gz before deleting them. On a cluster with an archive container a non-zero daily step is the sweep working; a FLAT ZERO on every node for more than a day means the sweep is not running or is refusing, and the store grows past its retention. On a cluster with NO archive container the sweep refuses by design and this stays at 0.",
	})

	logsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "logs",
		Name:      "deleted_total",
		Help:      "EVERY NODE TYPE exports this series, moved only by the node that ran the retention sweep. Log lines deleted from log_line AFTER their day was archived -- never before, so this can never exceed memql_logs_archived_total over the same run. Alert on it moving while memql_logs_archived_total does not: that is a delete without an archive, which the sweep is built never to do.",
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
		subscriptionRowsDenied,
		authActivityPruned,
		aiFederationExchanges,
		logsWrittenTotal,
		logsDroppedTotal,
		logsArchivedTotal,
		logsDeletedTotal,
		resultCacheInvalidationEvictions,
		resultCacheInvalidationEvents,
		resultCacheQueryReads,
		cacheCollector{},
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
	//
	// THAT QUALIFICATION NOW LIVES IN THE Help STRINGS TOO, and it has to
	// (memql#3804). This comment is the right note for someone editing this
	// file and reaches nobody else: an operator writing an alerting rule reads
	// the scrape, where Help is the only prose there is. So for as long as the
	// qualifier was here and not there, the audience that needed it was the one
	// audience structurally unable to see it -- while age_known's own Help said
	// "Alert on 0" flat, on a series that is a constant 0 across seven of eight
	// node types. Keep both in step; TestSigningKeyHelpNamesTheIdentityScope
	// fails if a Help string loses the scope.
	identitySigningKeyCreatedTimestamp.Set(0)
	identitySigningKeyAgeKnown.Set(0)
	identitySigningKeyRotationSupported.Set(0)
	// All three federation outcomes exist at 0 from boot, for the reason the
	// keyset gauges above do -- and here it is load-bearing rather than tidy.
	// The runbook tells operators to ALERT ON `denied`, and a Prometheus rule
	// over a series that does not exist yet evaluates to no data, not to zero:
	// on a cluster where the exchange has never once been refused, the alert
	// is silently unarmed, which is precisely the state it is meant to watch
	// for. A CounterVec creates a child only on first Inc, so without these
	// three lines `denied` first appears at the moment it is too late.
	aiFederationExchanges.WithLabelValues(FederationExchangeOK).Add(0)
	aiFederationExchanges.WithLabelValues(FederationExchangeDenied).Add(0)
	aiFederationExchanges.WithLabelValues(FederationExchangeError).Add(0)
	// The log store's four series exist at 0 from boot, for the reason the
	// federation outcomes do: the Help strings tell an operator to alert on a
	// flat zero and on reason="db", and a rule over a series that does not
	// exist yet evaluates to no data rather than to zero. A CounterVec child
	// appears on its first Inc, so each drop reason is created here.
	logsWrittenTotal.Add(0)
	logsArchivedTotal.Add(0)
	logsDeletedTotal.Add(0)
	for _, reason := range []string{LogsDropQueue, LogsDropRate, LogsDropLevel, LogsDropDB} {
		logsDroppedTotal.WithLabelValues(reason).Add(0)
	}
}

// SubscriptionRowDenied records one graph-subscription event dropped at
// fan-out because the subscribing stream may not read the row
// (memql#4309).
//
// Labelled by CONCEPT and by nothing else. Concept ids are a closed,
// low-cardinality set, so they are safe to label on; the row id and the
// actor are not, and labelling by either would put the identifier of a row
// somebody could not read into a metrics store that is usually read more
// widely than the data is.
func SubscriptionRowDenied(concept string) {
	subscriptionRowsDenied.WithLabelValues(concept).Inc()
}

// SubscriptionRowsDeniedValue returns the current count for one concept,
// for tests.
func SubscriptionRowsDeniedValue(concept string) float64 {
	var m dto.Metric
	c, err := subscriptionRowsDenied.GetMetricWithLabelValues(concept)
	if err != nil {
		return 0
	}
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
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

// AuthActivityPruned records rows hard-deleted from v1:identity:authActivity
// by the retention job (memql#4330).
//
// Unlabelled, deliberately. The only dimension worth having would be the
// concept, and there is exactly one; a per-run or per-batch label would be
// unbounded cardinality bought for nothing.
func AuthActivityPruned(n int64) {
	if n <= 0 {
		return
	}
	authActivityPruned.Add(float64(n))
}

// AuthActivityPrunedValue returns the current count, for tests.
func AuthActivityPrunedValue() float64 {
	var m dto.Metric
	if err := authActivityPruned.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// Federation-exchange outcomes (memql#4335). Closed set: an outcome label is
// a dimension of an alert, so it is these three and never a free string.
const (
	FederationExchangeOK     = "ok"
	FederationExchangeDenied = "denied"
	FederationExchangeError  = "error"
)

// AIFederationExchange records one Anthropic federation token exchange.
//
// Labelled by OUTCOME and by nothing else. The tempting extra labels -- the
// federation rule id, the service account, the HTTP status -- are all either
// unbounded or an identifier of a credential, and this series is scraped into
// a store that is usually read more widely than the config is. The reason a
// denial happened belongs in the warn log beside it, which carries Anthropic's
// own error body.
func AIFederationExchange(outcome string) {
	aiFederationExchanges.WithLabelValues(outcome).Inc()
}

// AIFederationExchangesValue returns the current count for one outcome, for
// tests.
func AIFederationExchangesValue(outcome string) float64 {
	var m dto.Metric
	c, err := aiFederationExchanges.GetMetricWithLabelValues(outcome)
	if err != nil {
		return 0
	}
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// Log-store drop reasons (epic memql#4893). Closed set: a reason is a
// dimension of an alert, so it is these four and never a free string.
const (
	LogsDropQueue = "queue" // the 4096-line queue was full
	LogsDropRate  = "rate"  // the per-node MEMQL_LOGS_MAX_LINES_PER_SECOND bucket was empty
	LogsDropLevel = "level" // below MEMQL_LOGS_LEVEL on an un-prefiltered path (the OS write)
	LogsDropDB    = "db"    // a batch insert failed; the whole batch was lost
)

// LogsWritten records n log lines persisted by this node's store sink.
func LogsWritten(n int) {
	if n <= 0 {
		return
	}
	logsWrittenTotal.Add(float64(n))
}

// LogsDropped records n log lines this node's store did not persist, under
// one of the LogsDrop* reasons.
func LogsDropped(reason string, n uint64) {
	if n == 0 {
		return
	}
	logsDroppedTotal.WithLabelValues(reason).Add(float64(n))
}

// LogsArchived records n log lines the retention sweep wrote to the archive.
func LogsArchived(n int64) {
	if n <= 0 {
		return
	}
	logsArchivedTotal.Add(float64(n))
}

// LogsDeleted records n log lines the retention sweep deleted after archiving.
func LogsDeleted(n int64) {
	if n <= 0 {
		return
	}
	logsDeletedTotal.Add(float64(n))
}

// LogsWrittenValue returns the current count, for tests.
func LogsWrittenValue() float64 { return counterValue(logsWrittenTotal) }

// LogsDroppedValue returns the current count for one reason, for tests.
func LogsDroppedValue(reason string) float64 {
	c, err := logsDroppedTotal.GetMetricWithLabelValues(reason)
	if err != nil {
		return 0
	}
	return counterValue(c)
}

// LogsArchivedValue returns the current count, for tests.
func LogsArchivedValue() float64 { return counterValue(logsArchivedTotal) }

// LogsDeletedValue returns the current count, for tests.
func LogsDeletedValue() float64 { return counterValue(logsDeletedTotal) }
