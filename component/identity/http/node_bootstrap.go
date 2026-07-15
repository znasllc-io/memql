package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// node_bootstrap.go implements the `POST /node/bootstrap` endpoint
// that lets a cluster binary (bff / agent / cognition / planner /
// voice) mint itself a class="node" JWT at startup when its
// MEMQL_NODE_TOKEN env var is empty. Addresses memql#338: every
// peer-to-peer NodeService.Stream call was failing with
// "authorization header missing" in a local multi-node cluster that
// never wired per-node tokens.
//
// Security model:
//
//   - The endpoint is DISABLED by default. It only accepts requests
//     when `MEMQL_NODE_BOOTSTRAP_TOKEN` is set on the identity
//     service (loaded into `Config.NodeBootstrapToken`); when empty,
//     every request returns 503. Production deploys that mint
//     per-node tokens out-of-band via the operator CLI leave this
//     env var unset and the endpoint stays dark.
//
//   - When enabled, the caller must present the same bootstrap
//     secret as `Authorization: Bootstrap <token>`. The compare is
//     constant-time. A mismatched or missing secret returns 401
//     with a structured error code so node-startup code can
//     distinguish "wrong secret" from "endpoint disabled" / "wrong
//     route" / "identity unreachable".
//
//   - The endpoint runs over the same require-secure transport gate
//     as `/pair/redeem` -- the bootstrap secret is a long-lived
//     plaintext credential. The dev escape hatch
//     `MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP=1` lets a local dev
//     cluster (where identity is reachable plain-HTTP in-network)
//     self-bootstrap without an HTTPS terminator.
//     Production deploys must leave this unset.
//
// The minted JWT is identical to what an out-of-band mint would
// produce -- same class, same NodeId / NodeType claims, same
// signing key, same TTL (DefaultNodeTokenTTLSeconds = 30 days).
// Bootstrap and out-of-band are two paths to the same token; rolling
// the bootstrap secret does NOT invalidate live tokens.
//
// On-disk audit + revoke: the bootstrap path uses a synthetic
// `IdentityId` of the form `v1:identity:identity:node:<nodeType>:<nodeId>`
// rather than writing a v1:identity:identity row. The JWT carries
// the synthetic id as `sub`; the verifier doesn't look the row up
// (it trusts the signed claim). A future enhancement can persist
// rows for /admin/tokens to display + revoke -- see #338's
// follow-up notes. For now, revoke is "rotate the bootstrap secret
// + wait for outstanding tokens to expire (30d max)".

// envAllowInsecureBootstrap is the dev-only escape hatch that bypasses
// the HTTPS-required check on /node/bootstrap. The bootstrap secret
// is a long-lived bearer credential, so production deploys must NEVER
// set this. The runtime emits a WARN log every time a request slips
// through under the escape hatch so accidental production toggles
// surface in the operator's log shipper.
const envAllowInsecureBootstrap = "MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP"

// NodeBootstrapRequest is the JSON body for POST /node/bootstrap.
// NodeId + NodeType are required for the default class="node" path;
// the minted JWT binds to them via the standard NodeIssueInput.
//
// memql#342 extends the endpoint with optional TokenClass +
// InstanceId so the Go voice-agent can self-bootstrap its
// class="voice_agent" JWT through the same surface + bootstrap
// secret. Backward-compatible: an empty TokenClass falls back to
// the original class="node" mint path.
type NodeBootstrapRequest struct {
	// NodeId is required on the class="node" path. Ignored on the
	// class="voice_agent" path (the instance id takes its place in
	// the JWT claims).
	NodeId string `json:"nodeId"`
	// NodeType is required on the class="node" path. Ignored on the
	// class="voice_agent" path.
	NodeType string `json:"nodeType"`
	// TokenClass selects the JWT class to mint. Empty or "node"
	// preserves the pre-#342 behavior; "voice_agent" mints a
	// class="voice_agent" JWT.
	TokenClass string `json:"tokenClass,omitempty"`
	// InstanceId is the operator-chosen voice-agent instance label
	// (e.g. "voice-agent-prod-us-east-1"). Required when
	// TokenClass="voice_agent"; ignored otherwise.
	InstanceId string `json:"instanceId,omitempty"`
}

// NodeBootstrapResponse carries the minted JWT (or a structured
// error). Mirrors the shape of PairRedeemResponse so node-startup
// code can use a single decode for either success or failure.
type NodeBootstrapResponse struct {
	Success    bool   `json:"success"`
	PlainToken string `json:"plainToken,omitempty"`
	IdentityId string `json:"identityId,omitempty"`
	NodeType   string `json:"nodeType,omitempty"`
	NodeId     string `json:"nodeId,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// handleNodeBootstrap mints a class="node" JWT for a presenting
// caller that knows the configured bootstrap secret. Returns:
//
//   - 503 service_unavailable -- endpoint disabled (no bootstrap secret configured)
//   - 401 unauthorized        -- missing or wrong bootstrap secret
//   - 400 bad_request         -- malformed body / missing required fields
//   - 403 insecure_transport  -- plaintext HTTP without the dev escape hatch
//   - 500 mint_failed         -- JWT issuance failed (key material missing etc.)
//   - 200 success             -- {success: true, plainToken: <JWT>, ...}
func (s *Server) handleNodeBootstrap(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureBootstrapRequest(w, r) {
		return
	}

	configured := strings.TrimSpace(s.Cfg.NodeBootstrapToken)
	if configured == "" {
		writeJSON(w, http.StatusServiceUnavailable, NodeBootstrapResponse{
			Success:   false,
			Error:     "node bootstrap is disabled (MEMQL_NODE_BOOTSTRAP_TOKEN not set on identity service)",
			ErrorCode: "bootstrap_disabled",
		})
		return
	}

	presented := extractAuthScheme(r, "Bootstrap")
	if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) != 1 {
		writeJSON(w, http.StatusUnauthorized, NodeBootstrapResponse{
			Success:   false,
			Error:     "invalid bootstrap secret",
			ErrorCode: "invalid_bootstrap_secret",
		})
		return
	}

	var body NodeBootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, NodeBootstrapResponse{
			Success:   false,
			Error:     "malformed JSON body: " + err.Error(),
			ErrorCode: "bad_request",
		})
		return
	}

	tokenClass := strings.TrimSpace(body.TokenClass)
	if tokenClass == "" {
		tokenClass = "node"
	}

	switch tokenClass {
	case "node":
		s.mintNodeBootstrapToken(w, r, body)
	case "voice_agent":
		s.mintVoiceAgentBootstrapToken(w, r, body)
	default:
		writeJSON(w, http.StatusBadRequest, NodeBootstrapResponse{
			Success:   false,
			Error:     "tokenClass must be \"node\" or \"voice_agent\", got " + tokenClass,
			ErrorCode: "bad_request",
		})
	}
}

// mintNodeBootstrapToken is the original class="node" mint path,
// extracted from handleNodeBootstrap so the multi-class dispatcher
// reads cleanly. memql#338 contract preserved verbatim.
func (s *Server) mintNodeBootstrapToken(w http.ResponseWriter, r *http.Request, body NodeBootstrapRequest) {
	nodeType := strings.TrimSpace(body.NodeType)
	nodeId := strings.TrimSpace(body.NodeId)
	if nodeType == "" || nodeId == "" {
		writeJSON(w, http.StatusBadRequest, NodeBootstrapResponse{
			Success:   false,
			Error:     "nodeId and nodeType are required",
			ErrorCode: "bad_request",
		})
		return
	}

	// Lookup-or-create the v1:identity:identity row for this
	// (nodeType, nodeId) binding (memql#343). Persisting the row
	// gives operators audit + listing surface on bootstrap-minted
	// credentials -- the synthetic-subject shape that #338 originally
	// shipped left bootstrap-minted tokens invisible to
	// /admin/tokens, which precluded both revocation and audit.
	//
	// The verifier still doesn't dereference IdentityId during JWT
	// validation -- it trusts the signed `node_id` / `node_type`
	// claims directly. The row exists for operational visibility,
	// not for runtime auth. Hot-path revocation enforcement is the
	// follow-up to this issue (see #343's step 5).
	//
	// When Store is nil (unit tests + ad-hoc handler drivers), the
	// handler falls back to the synthetic-subject behaviour from
	// before #343. Production deploys always wire Store -- the
	// fallback exists purely to keep the handler's tests independent
	// of the full bootstrap stack.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		existing *identity.NodeTokenLookup
		keyHash  string
	)
	now := time.Now().UTC()

	if s.Store != nil {
		binding := identity.NodeTokenBinding{NodeType: nodeType, NodeId: nodeId}
		lookup, lookupErr := s.Store.LookupNodeTokenIdentityByBinding(ctx, binding)
		if lookupErr != nil {
			errorId := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("node_bootstrap_lookup_failed",
					"error_id", errorId,
					"error", lookupErr.Error(),
					"node_type", nodeType,
					"node_id", nodeId,
				)
			}
			writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
				Success:   false,
				Error:     "node-token lookup failed; see identity logs (errorId=" + errorId + ")",
				ErrorCode: "mint_failed",
			})
			return
		}
		existing = lookup
		if existing != nil && !existing.Active {
			// The operator revoked this row out-of-band (e.g. via
			// /admin/tokens). Refuse to silently re-mint a fresh
			// token for the same (nodeType, nodeId) -- restoring the
			// row to active status is the explicit, audit-emitting
			// path.
			writeJSON(w, http.StatusForbidden, NodeBootstrapResponse{
				Success:   false,
				Error:     "node_token identity for this (nodeType, nodeId) is revoked; re-activate via /admin/tokens before bootstrapping again",
				ErrorCode: "credential_revoked",
			})
			return
		}

		// Mint a fresh auxiliary bearer + hash on every bootstrap
		// call so the keyHash audit fingerprint stays correlated
		// with the freshly-signed JWT (rotating the JWT but keeping
		// the keyHash would split the audit trail). The plaintext
		// bearer is never returned -- only the JWT is the
		// operational credential.
		_, hash, hashErr := newNodeTokenAuxBearer()
		if hashErr != nil {
			errorId := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("node_bootstrap_aux_bearer_failed",
					"error_id", errorId,
					"error", hashErr.Error(),
				)
			}
			writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
				Success:   false,
				Error:     "node-token mint failed; see identity logs (errorId=" + errorId + ")",
				ErrorCode: "mint_failed",
			})
			return
		}
		keyHash = hash
	}

	var identityId string
	if existing != nil {
		identityId = existing.IdentityId
	} else if s.Store != nil {
		newId, idErr := newNodeTokenIdentityId(nodeType)
		if idErr != nil {
			errorId := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("node_bootstrap_id_gen_failed",
					"error_id", errorId,
					"error", idErr.Error(),
				)
			}
			writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
				Success:   false,
				Error:     "node-token mint failed; see identity logs (errorId=" + errorId + ")",
				ErrorCode: "mint_failed",
			})
			return
		}
		identityId = newId
	} else {
		// Test-mode fallback: synthetic subject, no persistence.
		identityId = "v1:identity:identity:node:" + nodeType + ":" + nodeId
	}

	token, expiresAt, err := s.Issuer.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: identityId,
		NodeId:     nodeId,
		NodeType:   nodeType,
	}, now)
	if err != nil {
		errorId := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("node_bootstrap_mint_failed",
				"error_id", errorId,
				"error", err.Error(),
				"token_class", "node",
				"node_type", nodeType,
				"node_id", nodeId,
			)
		}
		writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
			Success:   false,
			Error:     "node-token mint failed; see identity logs (errorId=" + errorId + ")",
			ErrorCode: "mint_failed",
		})
		return
	}

	// Persist the audit fields. First-time bindings get insert();
	// repeat callers get update(). Either way we own the row before
	// returning -- a row that fails to write would leak a JWT no
	// admin can later see in /admin/tokens, which is exactly the gap
	// #343 exists to close. Skipped entirely when Store is nil (the
	// test-mode fallback above).
	bootstrappedFrom := clientIP(r)
	if s.Store != nil {
		expiresAtStr := expiresAt.Format(time.RFC3339Nano)
		bootstrappedAt := now.Format(time.RFC3339Nano)
		const bootstrapMintedBy = "system:node-bootstrap"
		const bootstrapUserId = "system:node-bootstrap"

		// The node_token row is a machine credential, so its write is
		// gated by the memql#2513 credential-actor guard: only a system
		// actor may write it. The bootstrap request authenticates with
		// the shared Bootstrap secret and carries no JWT, so
		// SystemActorMiddleware stamps the role="owner" session actor --
		// which the guard rejects. Stamp the dedicated system credential
		// actor here (overriding that owner actor) so both the first-time
		// create and the repeat-bootstrap stamp legs pass. memql#2549.
		persistCtx := identity.ContextWithSystemCredentialActor(ctx)

		var persistErr error
		if existing == nil {
			persistErr = s.Store.CreateNodeTokenIdentity(persistCtx,
				identityId, bootstrapUserId, nodeId, nodeType, keyHash,
				bootstrapMintedBy, expiresAtStr, bootstrappedAt, bootstrappedFrom)
		} else {
			persistErr = s.Store.StampNodeTokenBootstrap(persistCtx,
				identityId, bootstrapUserId, nodeId, nodeType, keyHash,
				bootstrapMintedBy, expiresAtStr, bootstrappedAt, bootstrappedFrom)
		}
		if persistErr != nil {
			errorId := generateErrorId()
			if s.Logger != nil {
				s.Logger.Error("node_bootstrap_persist_failed",
					"error_id", errorId,
					"error", persistErr.Error(),
					"node_type", nodeType,
					"node_id", nodeId,
					"identity_id", identityId,
				)
			}
			writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
				Success:   false,
				Error:     "node-token audit persist failed; see identity logs (errorId=" + errorId + ")",
				ErrorCode: "mint_failed",
			})
			return
		}
	}

	if s.Logger != nil {
		reuse := existing != nil
		s.Logger.Info("node_bootstrap_issued",
			"token_class", "node",
			"node_type", nodeType,
			"node_id", nodeId,
			"identity_id", identityId,
			"expires_at", expiresAt.Format(time.RFC3339),
			"remote", bootstrappedFrom,
			"reused_row", reuse,
			"persisted", s.Store != nil,
		)
	}

	writeJSON(w, http.StatusOK, NodeBootstrapResponse{
		Success:    true,
		PlainToken: token,
		IdentityId: identityId,
		NodeType:   nodeType,
		NodeId:     nodeId,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	})
}

// mintVoiceAgentBootstrapToken is the class="voice_agent" mint path
// added by memql#342. Lets the Go voice-agent process self-
// bootstrap its JWT through the same endpoint + bootstrap secret
// the node binaries use, eliminating the voice-agent crash-loop
// when the out-of-band mint step was skipped.
//
// Verifier contract: the voice-agent gRPC interceptor reads the
// JWT's class + identity_id + node_id (voice-agent reuses the
// NodeId claim slot for InstanceId per JWTIssuer's design) without
// per-class cross-checks against persisted identity rows. The same
// synthetic-IdentityId pattern the node path uses works here too.
func (s *Server) mintVoiceAgentBootstrapToken(w http.ResponseWriter, r *http.Request, body NodeBootstrapRequest) {
	instanceId := strings.TrimSpace(body.InstanceId)
	if instanceId == "" {
		writeJSON(w, http.StatusBadRequest, NodeBootstrapResponse{
			Success:   false,
			Error:     "instanceId is required for tokenClass=voice_agent",
			ErrorCode: "bad_request",
		})
		return
	}

	syntheticIdentityId := "v1:identity:identity:voice_agent:" + instanceId

	token, expiresAt, err := s.Issuer.IssueVoiceAgentAccessToken(identity.VoiceAgentIssueInput{
		IdentityId: syntheticIdentityId,
		InstanceId: instanceId,
	}, time.Now().UTC())
	if err != nil {
		errorId := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("node_bootstrap_mint_failed",
				"error_id", errorId,
				"error", err.Error(),
				"token_class", "voice_agent",
				"instance_id", instanceId,
			)
		}
		writeJSON(w, http.StatusInternalServerError, NodeBootstrapResponse{
			Success:   false,
			Error:     "voice-agent token mint failed; see identity logs (errorId=" + errorId + ")",
			ErrorCode: "mint_failed",
		})
		return
	}

	if s.Logger != nil {
		s.Logger.Info("node_bootstrap_issued",
			"token_class", "voice_agent",
			"instance_id", instanceId,
			"identity_id", syntheticIdentityId,
			"expires_at", expiresAt.Format(time.RFC3339),
			"remote", clientIP(r),
		)
	}

	// NodeId echoes the instance id on the voice-agent path so a
	// single response struct serves both classes; NodeType stays
	// empty (the voice-agent JWT doesn't carry a node-type claim).
	writeJSON(w, http.StatusOK, NodeBootstrapResponse{
		Success:    true,
		PlainToken: token,
		IdentityId: syntheticIdentityId,
		NodeId:     instanceId,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	})
}

// newNodeTokenIdentityId mints the v1:identity:identity row id for a
// bootstrap-created node_token credential. The id encodes the
// node-type so operators reading admin / audit output can recognise
// at a glance which binary class a row belongs to; the random
// suffix is the actual uniqueness driver. Mirrors the voice-agent
// mint subcommand's pattern (subcommand_voice_agent_token.go).
// memql#343.
func newNodeTokenIdentityId(nodeType string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "v1:identity:identity:node_" + nodeType + "_" + hex.EncodeToString(b), nil
}

// newNodeTokenAuxBearer generates the auxiliary `mql_node_<base64>`
// bearer + its SHA-256 hex hash. The plaintext is NEVER returned to
// the caller (the JWT is the operational credential); the hash
// satisfies the schema's @required contract on `credentials.keyHash`
// and gives operators a stable fingerprint to correlate the
// identity row with audit lines. memql#343.
func newNodeTokenAuxBearer() (bearer, keyHash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	bearer = "mql_node_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(sum[:]), nil
}

// requireSecureBootstrapRequest mirrors requireSecureRequest's logic
// but reads the bootstrap-specific dev escape hatch
// (MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP=1). Kept distinct from the
// pair endpoints so an operator can selectively allow one and not
// the other.
func (s *Server) requireSecureBootstrapRequest(w http.ResponseWriter, r *http.Request) bool {
	if r == nil {
		http.Error(w, "no request", http.StatusBadRequest)
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	if strings.TrimSpace(os.Getenv(envAllowInsecureBootstrap)) == "1" {
		if s != nil && s.Logger != nil {
			s.Logger.Warn("node bootstrap endpoint admitting plaintext request via "+envAllowInsecureBootstrap+"=1; production must leave this unset",
				"path", r.URL.Path,
				"remote", clientIP(r),
			)
		}
		return true
	}
	writeJSON(w, http.StatusForbidden, NodeBootstrapResponse{
		Success:   false,
		Error:     "https required for node bootstrap",
		ErrorCode: "insecure_transport",
	})
	return false
}
