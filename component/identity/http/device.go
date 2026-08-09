package http

// POST /device/code -- the RFC 8628 device authorization request
// (memql#3410).
//
// This is the FALLBACK sign-in path, for the case the loopback
// redirect cannot serve: a genuinely headless box, or a network
// hardened enough that a browser on one machine cannot reach a
// listener on another. It costs the user a second device and a
// transcribed code, so it is built to be correct and safe rather than
// elaborate.
//
// The endpoint is HTTP because RFC 8628 is defined over HTTP: the
// device polls a token endpoint and the human visits a URL. Both live
// under the "Auth (identity service)" exception in CLAUDE.md's
// endpoint-protocol policy, alongside /oauth/token itself, which this
// grant slots into.
//
// Conventions are pair.go's: HTTPS required (both credentials are
// bearer secrets in a JSON body), hash-at-rest with the plaintext
// returned exactly once, an explicit TTL, a per-IP limiter, and an
// audit event carrying SourceIP on every outcome.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	"github.com/znasllc-io/memql/component/identity/devicecode"
)

// envDeviceCodePerHour tunes the per-IP device-authorization-request
// limit. Each accepted request writes a row and burns a user_code out
// of a 40-bit space, so the limiter is what stops an attacker from
// carpeting the space with pending codes hoping a human approves one.
// The honest user makes one request per sign-in.
const envDeviceCodePerHour = "MEMQL_IDENTITY_DEVICE_CODE_PER_HOUR"

const defaultDeviceCodePerHour = 60

// envDeviceCodeTTLSeconds overrides the 10-minute authorization
// window cluster-wide.
const envDeviceCodeTTLSeconds = "MEMQL_IDENTITY_DEVICE_CODE_TTL_SECONDS"

// deviceCodeMaxTTL caps the window. A device authorization is an
// approvable credential sitting in the database with a code short
// enough to type; a long window is exactly the wrong trade.
const deviceCodeMaxTTL = 30 * time.Minute

// deviceCodeLimiter returns this Server's per-IP device-authorization
// limiter, built once on first use. Server-scoped for the same reason
// grantLimiter is: each Server (and each test) gets its own bucket map
// and captures its own logger. Same in-memory caveat as every other
// limiter here -- a multi-replica deployment gets per-replica budgets.
func deviceCodeLimiter(s *Server) *abuse.IPRateLimiter {
	s.deviceCodeLimiterOnce.Do(func() {
		perHour := defaultDeviceCodePerHour
		if v := strings.TrimSpace(os.Getenv(envDeviceCodePerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.deviceCodeLimiterVal = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.deviceCodeLimiterVal
}

// DeviceAuthorizationResponse is the RFC 8628 §3.2 success body. Field
// names are the RFC's snake_case wire names, NOT this tree's usual
// camelCase JSON -- a stock OAuth device-flow client reads these keys
// and there is no version of "our house style" that it will accept.
type DeviceAuthorizationResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// handleDeviceCode mints a device authorization: one row, two hashed
// credentials, and the URL the human should visit.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	if s.DeviceCodes == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the device authorization grant is not wired on this node")
		return
	}
	ctx := r.Context()
	sourceIP := clientIP(r)

	if allowed, retryAfter := deviceCodeLimiter(s).Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.writeJSONError(w, http.StatusTooManyRequests, "slow_down", "too many device authorization requests from this address")
		return
	}

	req, err := readDeviceAuthorizationRequest(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ClientId == "" {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	// Same client resolution the authorization_code grant uses, so a
	// dynamically-registered (RFC 7591) client works here exactly as a
	// statically-configured one does. There is no redirect_uri to check:
	// the whole point of this grant is that the device has no redirect.
	if identity.ResolveClient(ctx, s.Cfg, s.Store, req.ClientId) == nil {
		s.auditDevice(r, identity.AuditEvent{
			Action:        "device_authorization_blocked",
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "unregistered_client",
			Detail:        map[string]any{"clientId": req.ClientId},
		})
		s.writeJSONError(w, http.StatusBadRequest, "invalid_client", "client_id is not registered")
		return
	}
	if err := validateDevicePKCE(req.CodeChallenge, req.CodeChallengeMethod); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	deviceCodePlain, deviceCodeHash, err := devicecode.MintDeviceCode()
	if err != nil {
		s.logErr("device: device_code mint failed", err)
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "code mint failed")
		return
	}
	userCodeDisplay, userCodeHash, err := devicecode.MintUserCode()
	if err != nil {
		s.logErr("device: user_code mint failed", err)
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "code mint failed")
		return
	}
	rowId, err := devicecode.NewId()
	if err != nil {
		s.logErr("device: id mint failed", err)
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "id mint failed")
		return
	}

	ttl := deviceCodeTTL()
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	if err := s.DeviceCodes.Create(ctx, devicecode.CreateInput{
		Id:                  rowId,
		ClientId:            req.ClientId,
		DeviceCodeHash:      deviceCodeHash,
		UserCodeHash:        userCodeHash,
		ExpiresAt:           expiresAt,
		IntervalSeconds:     devicecode.DefaultIntervalSeconds,
		Scope:               req.Scope,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: req.CodeChallengeMethod,
		SourceIP:            sourceIP,
		UserAgent:           r.Header.Get("User-Agent"),
	}); err != nil {
		s.logErr("device: persist failed", err)
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "could not record the device authorization")
		return
	}

	s.auditDevice(r, identity.AuditEvent{
		Action:     "device_authorization_requested",
		TargetType: "deviceCode",
		TargetId:   devicecode.CanonicalId(rowId),
		Outcome:    identity.AuditOutcomeSuccess,
		Detail: map[string]any{
			"clientId":  req.ClientId,
			"expiresAt": expiresAt.Format(time.RFC3339Nano),
			"pkce":      req.CodeChallenge != "",
		},
	})

	verificationURI := s.DeviceVerificationURI()
	writeJSON(w, http.StatusOK, DeviceAuthorizationResponse{
		DeviceCode:              deviceCodePlain,
		UserCode:                userCodeDisplay,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURI + "?user_code=" + url.QueryEscape(userCodeDisplay),
		ExpiresIn:               int(ttl / time.Second),
		Interval:                devicecode.DefaultIntervalSeconds,
	})
}

// DeviceVerificationURI is the absolute URL of the human-facing
// verification page. Exported because the wiring layer and tests both
// need to agree with what the response advertises.
func (s *Server) DeviceVerificationURI() string {
	base := strings.TrimRight(strings.TrimSpace(s.Cfg.BaseURL), "/")
	return base + DeviceVerificationPath
}

// DeviceVerificationPath is the path component of the verification
// page, mounted by component/identity/web.
const DeviceVerificationPath = "/device"

// deviceAuthorizationRequest is the parsed POST /device/code body.
type deviceAuthorizationRequest struct {
	ClientId            string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// readDeviceAuthorizationRequest accepts the OAuth-canonical
// x-www-form-urlencoded body AND application/json, mirroring
// readTokenRequest -- a device-flow client library will post the
// former; a hand-rolled editor extension usually posts the latter.
func readDeviceAuthorizationRequest(r *http.Request) (*deviceAuthorizationRequest, error) {
	// Bounded: this body carries four short strings and nothing else.
	body := http.MaxBytesReader(nil, r.Body, 16*1024)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			ClientId            string `json:"client_id"`
			Scope               string `json:"scope"`
			CodeChallenge       string `json:"code_challenge"`
			CodeChallengeMethod string `json:"code_challenge_method"`
		}
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			return nil, err
		}
		return &deviceAuthorizationRequest{
			ClientId:            strings.TrimSpace(in.ClientId),
			Scope:               strings.TrimSpace(in.Scope),
			CodeChallenge:       strings.TrimSpace(in.CodeChallenge),
			CodeChallengeMethod: strings.TrimSpace(in.CodeChallengeMethod),
		}, nil
	}
	r.Body = body
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return &deviceAuthorizationRequest{
		ClientId:            strings.TrimSpace(r.Form.Get("client_id")),
		Scope:               strings.TrimSpace(r.Form.Get("scope")),
		CodeChallenge:       strings.TrimSpace(r.Form.Get("code_challenge")),
		CodeChallengeMethod: strings.TrimSpace(r.Form.Get("code_challenge_method")),
	}, nil
}

// validateDevicePKCE rejects a malformed challenge at REQUEST time
// rather than at redemption. A device that mistypes its method would
// otherwise complete the whole human round trip before discovering the
// grant can never be redeemed.
func validateDevicePKCE(challenge, method string) error {
	if challenge == "" {
		if method != "" {
			return errDevicePKCEMethodWithoutChallenge
		}
		return nil
	}
	switch method {
	case "", "S256", "plain":
		return nil
	default:
		return errDevicePKCEUnsupportedMethod
	}
}

var (
	errDevicePKCEMethodWithoutChallenge = deviceError("code_challenge_method supplied without a code_challenge")
	errDevicePKCEUnsupportedMethod      = deviceError("unsupported code_challenge_method; use S256 or plain")
)

type deviceError string

func (e deviceError) Error() string { return string(e) }

// deviceCodeTTL resolves the authorization window: env override >
// package default, capped at deviceCodeMaxTTL.
func deviceCodeTTL() time.Duration {
	ttl := devicecode.DefaultTTL
	if v := strings.TrimSpace(os.Getenv(envDeviceCodeTTLSeconds)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	if ttl > deviceCodeMaxTTL {
		ttl = deviceCodeMaxTTL
	}
	return ttl
}

// auditDevice stamps the shared category on a device-grant event and
// forwards it through the request-aware audit helper (which fills in
// SourceIP + UserAgent).
func (s *Server) auditDevice(r *http.Request, ev identity.AuditEvent) {
	ev.Category = identity.AuditCategoryAuth
	s.audit(r, ev)
}
