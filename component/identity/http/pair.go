package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/workerpairing"
	"github.com/znasllc-io/memql/component/identity/workertoken"
)

// defaultPairingCodeTTL mirrors the previous gRPC default. Operators
// can supply a smaller / larger TTL via the request body.
const defaultPairingCodeTTL = 10 * time.Minute

// envAllowInsecurePair is the dev-only escape hatch that bypasses the
// HTTPS-required check on /pair/codes and /pair/redeem. The pair code
// is a bearer credential, so production deployments must NEVER set
// this. The runtime emits a WARN log every time a request slips
// through under the escape hatch so accidental production toggles
// surface in the operator's log shipper.
const envAllowInsecurePair = "MEMQL_IDENTITY_ALLOW_INSECURE_PAIR"

// requireSecureRequest rejects HTTP-only requests on endpoints that
// must run over TLS. The pair-code endpoints carry plaintext
// credentials inside the Authorization header (Pair <code>) and
// inside the JSON response (plainCode), so a plaintext hop here is a
// credential-leak surface. A reverse-proxy fronting plaintext to the
// binary surfaces the deployment posture via X-Forwarded-Proto.
//
// The transport predicate itself lives in identity.RequestIsSecure so
// the enrolment surface (memql#3408) enforces the same rule from the
// same code rather than from a second copy of it; the dev escape hatch
// stays here because it is named per-surface.
//
// Returns true when the request is admissible; writes a 403 and
// returns false otherwise.
func (s *Server) requireSecureRequest(w http.ResponseWriter, r *http.Request) bool {
	if r == nil {
		http.Error(w, "no request", http.StatusBadRequest)
		return false
	}
	if identity.RequestIsSecure(r) {
		return true
	}
	if identity.InsecureTransportEscapeEnabled(envAllowInsecurePair) {
		if s != nil && s.Logger != nil {
			s.Logger.Warn("pair endpoint admitting plaintext request via "+envAllowInsecurePair+"=1; production must leave this unset",
				"path", r.URL.Path,
				"remote", clientIP(r),
			)
		}
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":     "https required",
		"errorCode": "insecure_transport",
	})
	return false
}

// PairCreateRequest is the JSON body for POST /pair/codes. Authenticated
// callers (the product's Connect Computer modal) supply their cluster
// URL + an optional owner override (admin-only). The server stamps
// expiresAt and the owner user id.
type PairCreateRequest struct {
	ClusterURL  string `json:"clusterUrl"`
	OwnerUserId string `json:"ownerUserId,omitempty"`
	TTLSeconds  int64  `json:"ttlSeconds,omitempty"`
}

// PairCreateResponse mirrors the previous gRPC reply shape so the
// product UI can show the same plain-code + expiry surface.
type PairCreateResponse struct {
	Success     bool   `json:"success"`
	PlainCode   string `json:"plainCode,omitempty"`
	PairingId   string `json:"pairingId,omitempty"`
	OwnerUserId string `json:"ownerUserId,omitempty"`
	ClusterURL  string `json:"clusterUrl,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
}

// PairRedeemRequest is the JSON body for POST /pair/redeem. Auth lives
// on the request via `Authorization: Pair <code>`; the body just
// carries the worker name suggestion.
type PairRedeemRequest struct {
	WorkerName string `json:"workerName,omitempty"`
}

// PairRedeemResponse mirrors the previous gRPC reply shape.
//
// ClusterURL is the gRPC dial target for WorkerService.Stream, and it STATES
// its transport: `https://host:443` or `http://host:50050` (memql#3437). The
// consumer parses it with sdk/go/worker.ParseClusterURL, which reads a scheme
// as authoritative and a BARE `host:port` as plaintext -- so a bare value here
// told a worker to dial a TLS port in the clear. The only value that is still
// bare is an operator endpoint that named a port but no scheme; the handler
// logs a WARN when it emits one.
type PairRedeemResponse struct {
	Success     bool   `json:"success"`
	PlainToken  string `json:"plainToken,omitempty"`
	IdentityId  string `json:"identityId,omitempty"`
	OwnerUserId string `json:"ownerUserId,omitempty"`
	ClusterURL  string `json:"clusterUrl,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
}

// handlePairCreate mints a worker-pairing code for an authenticated
// caller. The caller is identified by the bearer JWT; the SystemActor
// middleware does not attach a user identity, so we pull the subject
// off the verifier-emitted claims.
//
// Identity binaries do NOT run the per-node verifier middleware (they
// own the signing keys themselves), so we verify the bearer token
// directly via the local issuer.
func (s *Server) handlePairCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil || s.Issuer == nil {
		s.writePairError(w, http.StatusInternalServerError, "server", "identity engine not wired")
		return
	}

	caller, ok := s.requireBearer(w, r)
	if !ok {
		return
	}
	callerUserId := strings.TrimSpace(caller.Subject)
	if callerUserId == "" {
		s.writePairError(w, http.StatusUnauthorized, "unauthenticated", "subject claim missing")
		return
	}

	var body PairCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writePairError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}

	clusterURL := strings.TrimSpace(body.ClusterURL)
	if clusterURL == "" {
		s.writePairError(w, http.StatusBadRequest, "bad_request", "clusterUrl required")
		return
	}

	ownerUserId := strings.TrimSpace(body.OwnerUserId)
	if ownerUserId == "" {
		ownerUserId = callerUserId
	} else if ownerUserId != callerUserId && !isAdminRole(caller.Role) {
		s.writePairError(w, http.StatusForbidden, "permission_denied", "only admins may mint pairing codes for other users")
		return
	}

	ttl := defaultPairingCodeTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	plain, hash, err := workerpairing.Mint()
	if err != nil {
		s.logErr("pair: code mint failed", err)
		s.writePairError(w, http.StatusInternalServerError, "internal", "code mint failed")
		return
	}
	pairingId, err := workerpairing.NewId()
	if err != nil {
		s.logErr("pair: id mint failed", err)
		s.writePairError(w, http.StatusInternalServerError, "internal", "id mint failed")
		return
	}

	store := &workerpairing.Store{Engine: s.Store.Engine, Logger: s.Logger}
	// SystemActorMiddleware ran before us via Mount(), so the context
	// already carries the synthetic actor downstream mutations need.
	ctx := r.Context()
	if err := store.Create(ctx, pairingId, ownerUserId, hash, clusterURL, expiresAt, clientIP(r)); err != nil {
		s.logErr("pair: persist failed", err)
		s.writePairError(w, http.StatusInternalServerError, "persist_failed", err.Error())
		return
	}

	if s.Audit != nil {
		s.Audit.Log(ctx, identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      "worker_pairing_code_created",
			ActorUserId: callerUserId,
			TargetType:  "workerPairingCode",
			TargetId:    pairingId,
			Outcome:     identity.AuditOutcomeSuccess,
			Detail: map[string]any{
				"ownerUserId": ownerUserId,
				"clusterUrl":  clusterURL,
			},
		})
	}

	writeJSON(w, http.StatusOK, PairCreateResponse{
		Success:     true,
		PlainCode:   plain,
		PairingId:   workerpairing.CanonicalId(pairingId),
		OwnerUserId: ownerUserId,
		ClusterURL:  clusterURL,
		ExpiresAt:   expiresAt.Format(time.RFC3339Nano),
	})
}

// handlePairRedeem swaps a fresh `Authorization: Pair <code>` for a
// `mql_wkr_<...>` worker token + the canonical gRPC dial address the
// cockpit-worker should use.
//
// The redeem endpoint is unauthenticated by Bearer-token standards --
// the pair code IS the credential. We re-validate the row here (the
// interceptor on the gRPC stream did the same in the previous
// implementation; on HTTP it's a single hop, so the same logic lives
// inline).
func (s *Server) handlePairRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.Store == nil {
		s.writeRedeemError(w, http.StatusInternalServerError, "server", "identity engine not wired")
		return
	}

	plainCode := strings.TrimSpace(extractAuthScheme(r, "Pair"))
	if plainCode == "" {
		s.writeRedeemError(w, http.StatusUnauthorized, "unauthenticated", "Authorization: Pair <code> required")
		return
	}

	canonical := workerpairing.Canonicalize(plainCode)
	if canonical == "" {
		s.writeRedeemError(w, http.StatusUnauthorized, "bad_code", "code is malformed")
		return
	}
	hash := workerpairing.Hash(canonical)
	if hash == "" {
		s.writeRedeemError(w, http.StatusUnauthorized, "bad_code", "code hash failed")
		return
	}

	pairStore := &workerpairing.Store{Engine: s.Store.Engine, Logger: s.Logger}
	tokenStore := &workertoken.Store{Engine: s.Store.Engine, Logger: s.Logger}
	// SystemActorMiddleware ran before us via Mount(), so the context
	// already carries the synthetic actor the workerPairingCode mutations
	// need.
	ctx := r.Context()
	// The worker_token row is a machine credential, so its write is gated
	// by the memql#2513 credential-actor guard: only a system actor may
	// write it. The middleware's role="owner" session actor is rejected,
	// so the tokenStore create/revoke below run under the dedicated
	// system credential actor. The pair code redeemed here is the
	// caller's authorization. memql#2549.
	credCtx := identity.ContextWithSystemCredentialActor(ctx)

	row, err := pairStore.LookupByHash(ctx, hash)
	if err != nil {
		s.logErr("pair: lookup failed", err)
		s.writeRedeemError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	if row == nil {
		s.writeRedeemError(w, http.StatusNotFound, "not_found", "pairing code not found")
		return
	}

	now := time.Now().UTC()
	if row.IsRedeemed() {
		s.writeRedeemError(w, http.StatusConflict, "already_redeemed", "pairing code already redeemed")
		return
	}
	if row.IsExpired(now) {
		s.writeRedeemError(w, http.StatusGone, "expired", "pairing code expired")
		return
	}

	var body PairRedeemRequest
	if r.Body != nil {
		// Body is optional. Decoding-on-empty returns io.EOF which we
		// silently swallow.
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	plainToken, tokenHash, err := workertoken.Mint()
	if err != nil {
		s.logErr("pair: token mint failed", err)
		s.writeRedeemError(w, http.StatusInternalServerError, "internal", "token mint failed")
		return
	}
	identityId, err := workertoken.NewId()
	if err != nil {
		s.logErr("pair: identity id failed", err)
		s.writeRedeemError(w, http.StatusInternalServerError, "internal", "identity id failed")
		return
	}

	workerName := strings.TrimSpace(body.WorkerName)
	if workerName == "" {
		// Fallback: tag with "paired-<pairingId-suffix>" so audit
		// rows are distinguishable. The cockpit overwrites this with
		// its hostname when it Registers on WorkerService.
		workerName = "paired-" + suffix(row.ID, 6)
	}

	if err := tokenStore.Create(credCtx, identityId, row.OwnerUserId, workerName, tokenHash, row.OwnerUserId, time.Time{}); err != nil {
		s.logErr("pair: token row create failed", err)
		s.writeRedeemError(w, http.StatusInternalServerError, "persist_failed", "token row: "+err.Error())
		return
	}

	canonicalIdentityId := workertoken.CanonicalId(identityId)
	if err := pairStore.Redeem(ctx, row.ID, canonicalIdentityId, clientIP(r), now); err != nil {
		// Token row already landed; redeem-stamp failure means the
		// code can be redeemed twice if we're unlucky. Best-effort
		// soft-revoke the freshly-minted token to fail closed.
		_ = tokenStore.Revoke(credCtx, canonicalIdentityId)
		s.logErr("pair: redeem stamp failed", err)
		s.writeRedeemError(w, http.StatusInternalServerError, "persist_failed", "redeem stamp: "+err.Error())
		return
	}

	dialURL := resolveWorkerDialEndpoint(row.ClusterURL)
	s.warnOnTransportSilentDialURL(dialURL)

	if s.Audit != nil {
		s.Audit.Log(ctx, identity.AuditEvent{
			Category:    identity.AuditCategoryAuth,
			Action:      "worker_pairing_code_redeemed",
			ActorUserId: row.OwnerUserId,
			TargetType:  "identity",
			TargetId:    canonicalIdentityId,
			SourceIP:    clientIP(r),
			Outcome:     identity.AuditOutcomeSuccess,
			Detail: map[string]any{
				"pairingId": row.ID,
			},
		})
	}

	writeJSON(w, http.StatusOK, PairRedeemResponse{
		Success:     true,
		PlainToken:  plainToken,
		IdentityId:  canonicalIdentityId,
		OwnerUserId: row.OwnerUserId,
		ClusterURL:  dialURL,
	})
}

// requireBearer extracts and verifies the bearer JWT against the
// local issuer. Returns the verified claims on success; writes 401
// and returns ok=false on failure.
func (s *Server) requireBearer(w http.ResponseWriter, r *http.Request) (*identity.AccessTokenClaims, bool) {
	bearer := strings.TrimSpace(extractAuthScheme(r, "Bearer"))
	if bearer == "" {
		s.writePairError(w, http.StatusUnauthorized, "unauthenticated", "Authorization: Bearer <token> required")
		return nil, false
	}
	claims, err := s.Issuer.VerifyAccessToken(bearer, time.Now().UTC())
	if err != nil {
		s.writePairError(w, http.StatusUnauthorized, "unauthenticated", "token rejected: "+err.Error())
		return nil, false
	}
	return claims, true
}

// resolveWorkerDialEndpoint picks the gRPC URL the cockpit-worker
// should dial for WorkerService.Stream. WorkerService is registered
// only on the `agent` build tag, so the URL must point at the agent
// node, not the identity service or BFF.
//
// Resolution order:
//
//  1. MEMQL_WORKER_DIAL_ENDPOINT -- explicit operator override.
//     Production deployments set this to the agent's public dial
//     address, WITH A SCHEME: https://agent.acme.com (see below).
//  2. identity.DialURLFromOrigin(storedURL) -- maps the origin
//     stamped on the pairing row (the product SPA's
//     `window.location.origin`) to a dial address, using the same
//     origin-to-dial mapping the discovery document derives with.
//  3. The cluster-wide advertised discovery endpoint, when the stored
//     origin is unreadable.
//  4. Fall back to the stored URL verbatim.
//
// Tier 2 used to call identity.DeriveGRPCEndpoint, whose OWN first tier is
// MEMQL_DISCOVERY_GRPC_ENDPOINT -- so wherever that is set, which in a
// deployed cluster is everywhere, the stored origin was never read and every
// worker was handed the one advertised endpoint regardless of what it paired
// against. The mapping is now reached without the lookup (memql#3434); the
// advertised endpoint survives only as tier 3, where it belongs.
//
// THE VALUE CARRIES ITS TRANSPORT (memql#3437). Each tier resolves through a
// scheme-STATING mapping rather than the bare `host:port` one the discovery
// document publishes. The consumer is sdk/go/worker.ParseClusterURL, which
// reads a bare address as useTLS=false -- so the old bare "cockpit.<domain>:443"
// told a worker to dial a TLS port in plaintext, with its `mql_wkr_` bearer
// token in the clear. The dial TARGET is unchanged; strip the scheme from any
// value below and the pre-fix string comes back exactly.
//
// The one form that stays bare is an operator endpoint that names a PORT but
// no scheme (tiers 1 and 3), because nothing in `host:443` says whether that
// listener speaks TLS and the port is not evidence -- see
// identity.DialURLFromEndpoint. The caller logs a WARN when that happens; the
// remedy is to write the scheme.
func resolveWorkerDialEndpoint(storedURL string) string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_WORKER_DIAL_ENDPOINT")); v != "" {
		if u := identity.DialURLFromEndpoint(v); u != "" {
			return u
		}
		// Unreadable, but the operator wrote it deliberately -- hand it over
		// as-is rather than silently substituting a derived address.
		return v
	}
	if v := identity.DialURLFromOrigin(storedURL); v != "" {
		return v
	}
	// Unreadable origin. The advertised endpoint answers a different
	// question -- it names the cluster front door, not this pairing's
	// origin -- but it is at least a dialable address.
	if v := identity.DialURLFromEndpoint(os.Getenv("MEMQL_DISCOVERY_GRPC_ENDPOINT")); v != "" {
		return v
	}
	return storedURL
}

// warnOnTransportSilentDialURL logs when the resolved dial address names a
// port but no transport (memql#3437).
//
// The receiving worker parses it with sdk/go/worker.ParseClusterURL, which
// documents a bare `host:port` as PLAINTEXT. That default is reasonable for an
// operator hand-editing worker.yaml against a local cluster, and the server
// deliberately does NOT override it by guessing from the port number -- a TLS
// listener on a non-standard port would be guessed wrong, and `:443` is a
// convention, not a fact. What the server can do is refuse to be quiet about
// it: a worker that dials a TLS port in the clear writes its `mql_wkr_` bearer
// token onto the wire before the handshake fails.
//
// Only an operator-configured endpoint can reach here (tiers 1 and 3); a
// pairing origin always carries its scheme.
func (s *Server) warnOnTransportSilentDialURL(dialURL string) {
	if s == nil || s.Logger == nil {
		return
	}
	if dialURL == "" || strings.Contains(dialURL, "://") {
		return
	}
	s.Logger.Warn("worker pairing reply carries a dial address with no transport; the worker will dial it in PLAINTEXT",
		"dialEndpoint", dialURL,
		"remedy", "write the scheme on the configured endpoint -- https://host for TLS, http://host for plaintext",
		"source", "MEMQL_WORKER_DIAL_ENDPOINT or MEMQL_DISCOVERY_GRPC_ENDPOINT",
	)
}

// extractAuthScheme returns the token portion of an `Authorization:
// <scheme> <token>` header when the scheme matches case-insensitively;
// returns "" otherwise. Never panics on missing/malformed headers.
func extractAuthScheme(r *http.Request, scheme string) string {
	if r == nil {
		return ""
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], scheme) {
		return ""
	}
	return parts[1]
}

// suffix returns the trailing n chars of s. Fallback worker-name
// builder when the cockpit doesn't supply one.
func suffix(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func isAdminRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	return role == "owner" || role == "admin"
}

func (s *Server) logErr(msg string, err error) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn(msg, slog.String("error", err.Error()))
}

func (s *Server) writePairError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, PairCreateResponse{
		Success:   false,
		ErrorCode: code,
		Error:     message,
	})
}

func (s *Server) writeRedeemError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, PairRedeemResponse{
		Success:   false,
		ErrorCode: code,
		Error:     message,
	})
}
