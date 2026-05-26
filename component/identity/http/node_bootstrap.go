package http

import (
	"crypto/subtle"
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
// "authorization header missing" in the docker-compose dev stack
// because the compose file never wired per-node tokens.
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
//     `MEMQL_IDENTITY_ALLOW_INSECURE_BOOTSTRAP=1` lets the
//     docker-compose dev stack (where identity:8081 is plain HTTP
//     in-network) self-bootstrap without an HTTPS terminator.
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
// InstanceId so the Python voice-agent can self-bootstrap its
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

	// Synthetic IdentityId. The verifier doesn't look up identity rows
	// during JWT validation -- it trusts the signed `node_id` /
	// `node_type` claims directly -- so a synthetic subject works
	// fine for getting peer auth functional. A follow-up can persist
	// real identity rows so /admin/tokens can display + revoke per-
	// node tokens (see memql#338's notes).
	syntheticIdentityId := "v1:identity:identity:node:" + nodeType + ":" + nodeId

	token, expiresAt, err := s.Issuer.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: syntheticIdentityId,
		NodeId:     nodeId,
		NodeType:   nodeType,
	}, time.Now().UTC())
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

	if s.Logger != nil {
		s.Logger.Info("node_bootstrap_issued",
			"token_class", "node",
			"node_type", nodeType,
			"node_id", nodeId,
			"identity_id", syntheticIdentityId,
			"expires_at", expiresAt.Format(time.RFC3339),
			"remote", clientIP(r),
		)
	}

	writeJSON(w, http.StatusOK, NodeBootstrapResponse{
		Success:    true,
		PlainToken: token,
		IdentityId: syntheticIdentityId,
		NodeType:   nodeType,
		NodeId:     nodeId,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	})
}

// mintVoiceAgentBootstrapToken is the class="voice_agent" mint path
// added by memql#342. Lets the Python voice-agent process self-
// bootstrap its JWT through the same endpoint + bootstrap secret
// the node binaries use, eliminating the docker-compose crash-loop
// when `make dev-refresh`'s out-of-band mint step was skipped.
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
