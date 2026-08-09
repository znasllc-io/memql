package http

// The RFC 8628 device_code grant at POST /oauth/token (memql#3410).
//
// The device polls here with the opaque device_code it received from
// POST /device/code. Every poll answers one of six ways, and the
// distinction between them is the entire protocol -- a client cannot
// tell "keep waiting" from "the human said no" from "you are hammering
// me" out of a single invalid_grant, so RFC 8628 §3.5 names four
// errors and this handler is careful to return the right one:
//
//	authorization_pending  the human has not answered yet
//	slow_down              you polled sooner than `interval`; it just went up
//	access_denied          the human said no. Terminal.
//	expired_token          the 10-minute window closed. Terminal.
//	invalid_grant          the device_code does not exist, belongs to a
//	                       different client, or has already been redeemed
//	<200 OK>               tokens
//
// The success path is deliberately the same code as the
// authorization_code grant's: consume-then-mint ordering, the same
// session row, the same live TTL settings, the same refresh cookie.
// A device sign-in is not a different KIND of session, and building it
// out of different parts is how the two drift.

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
)

// DeviceGrantType is the RFC 8628 grant_type URN.
const DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// deviceGrantErrorResponse is the RFC 6749 §5.2 / RFC 8628 §3.5 error
// body. `interval` is present only on slow_down, where RFC 8628 allows
// the server to tell the client its new floor rather than leaving it
// to guess the increment.
type deviceGrantErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
	Interval         int    `json:"interval,omitempty"`
	ErrorId          string `json:"errorId,omitempty"`
}

func writeDeviceGrantError(w http.ResponseWriter, status int, code, description string, interval int) {
	writeJSON(w, status, deviceGrantErrorResponse{
		Error:            code,
		ErrorDescription: description,
		Interval:         interval,
		ErrorId:          generateErrorId(),
	})
}

// handleDeviceCodeGrant redeems (or refuses) one poll.
func (s *Server) handleDeviceCodeGrant(w http.ResponseWriter, r *http.Request, body *tokenRequest) {
	if s.DeviceCodes == nil {
		writeDeviceGrantError(w, http.StatusServiceUnavailable, "temporarily_unavailable",
			"the device authorization grant is not wired on this node", 0)
		return
	}
	ctx := r.Context()
	presented := trim(body.DeviceCode)
	if presented == "" {
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_request", "device_code is required", 0)
		return
	}
	if body.ClientId == "" {
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_request", "client_id is required", 0)
		return
	}

	row, err := s.DeviceCodes.LookupByDeviceCodeHash(ctx, devicecode.HashDeviceCode(presented))
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("device_grant_lookup_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "lookup failed; reference "+eid)
		return
	}
	if row == nil {
		s.auditDevice(r, identity.AuditEvent{
			Action:        "device_grant_blocked",
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "device_code_not_found",
		})
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant", "device_code is invalid", 0)
		return
	}

	// Bind the poll to the client the authorization was minted for.
	// Checked BEFORE the poll clock is touched: a caller presenting
	// somebody else's device_code must not get to move the victim's
	// interval, which would let it grief a legitimate flow into
	// permanent slow_down.
	if row.ClientId != body.ClientId {
		s.auditDevice(r, identity.AuditEvent{
			Action:        "device_grant_blocked",
			TargetType:    "deviceCode",
			TargetId:      row.ID,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "client_mismatch",
			Detail:        map[string]any{"presentedClientId": body.ClientId},
		})
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant", "device_code does not match client_id", 0)
		return
	}

	now := time.Now().UTC()

	// The poll floor, enforced against a PERSISTED clock so the budget
	// is per-authorization rather than per-replica. RFC 8628 §3.5: too
	// fast means slow_down AND a raised interval, permanently.
	interval := row.Interval()
	if !row.LastPolledAt.IsZero() && now.Sub(row.LastPolledAt) < time.Duration(interval)*time.Second {
		raised := interval + devicecode.SlowDownIncrementSeconds
		if raised > devicecode.MaxIntervalSeconds {
			raised = devicecode.MaxIntervalSeconds
		}
		s.touchDevicePoll(ctx, row.ID, now, raised)
		writeDeviceGrantError(w, http.StatusBadRequest, "slow_down",
			"polling faster than the interval; the interval has been increased", raised)
		return
	}
	s.touchDevicePoll(ctx, row.ID, now, interval)

	// Expiry is checked after the clock touch and before status, so a
	// device that comes back late learns the window closed rather than
	// being told to keep waiting on a row that can never be approved.
	if row.IsExpired(now) {
		writeDeviceGrantError(w, http.StatusBadRequest, "expired_token",
			"the device authorization has expired; request a new one", 0)
		return
	}

	switch row.Status {
	case devicecode.StatusPending:
		writeDeviceGrantError(w, http.StatusBadRequest, "authorization_pending",
			"the user has not yet approved this request", 0)
		return
	case devicecode.StatusDenied:
		writeDeviceGrantError(w, http.StatusBadRequest, "access_denied",
			"the user denied this request", 0)
		return
	case devicecode.StatusRedeemed:
		// Replay. Not access_denied -- the human said yes -- and not
		// expired_token; the credential is simply spent, which is
		// invalid_grant, exactly as a replayed authorization_code is.
		s.auditDevice(r, identity.AuditEvent{
			Action:        "device_grant_replay",
			TargetType:    "deviceCode",
			TargetId:      row.ID,
			ActorUserId:   row.ApprovedByUserId,
			Outcome:       identity.AuditOutcomeBlocked,
			FailureReason: "already_redeemed",
		})
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant",
			"this device authorization has already been redeemed", 0)
		return
	case devicecode.StatusApproved:
		// fall through
	default:
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant", "device authorization is in an unusable state", 0)
		return
	}

	if row.ApprovedByUserId == "" {
		// An approved row with no approver is a write that half
		// landed. Refuse rather than mint a session for nobody.
		if s.Logger != nil {
			s.Logger.Error("device_grant_approved_without_user", slog.String("device_code_id", row.ID))
		}
		writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant", "device authorization has no approver recorded", 0)
		return
	}

	// PKCE (RFC 7636), carried through the device flow exactly as the
	// authorization_code grant carries it: when a challenge was bound
	// at request time, a matching verifier is REQUIRED, and it is
	// checked before the row is spent.
	if row.CodeChallenge != "" {
		if err := verifyPKCE(row.CodeChallenge, row.CodeChallengeMethod, body.CodeVerifier); err != nil {
			s.auditDevice(r, identity.AuditEvent{
				Action:        "device_grant_blocked",
				TargetType:    "deviceCode",
				TargetId:      row.ID,
				ActorUserId:   row.ApprovedByUserId,
				Outcome:       identity.AuditOutcomeBlocked,
				FailureReason: "pkce_failed",
			})
			writeDeviceGrantError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed", 0)
			return
		}
	}

	// Spend the authorization BEFORE minting, so a concurrent second
	// poll reads status=redeemed and is refused.
	if err := s.DeviceCodes.Redeem(ctx, row.ID, now); err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("device_grant_redeem_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "redeem failed; reference "+eid)
		return
	}

	resp, err := s.issueSessionForUser(w, r, sessionMintInput{
		UserId:   row.ApprovedByUserId,
		ClientId: row.ClientId,
		Source:   "device_code",
		Now:      now,
	})
	if err != nil {
		eid := generateErrorId()
		if s.Logger != nil {
			s.Logger.Error("device_grant_issue_failed", slog.String("error_id", eid), slog.String("error", err.Error()))
		}
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "issue failed; reference "+eid)
		return
	}

	s.auditDevice(r, identity.AuditEvent{
		Action:      "device_grant_redeemed",
		TargetType:  "deviceCode",
		TargetId:    row.ID,
		ActorUserId: row.ApprovedByUserId,
		Outcome:     identity.AuditOutcomeSuccess,
		Detail:      map[string]any{"clientId": row.ClientId},
	})

	writeJSON(w, http.StatusOK, resp)
}

// touchDevicePoll stamps the poll clock, logging rather than failing
// the request when the write does not land. A dropped clock tick means
// the NEXT poll is judged against a stale timestamp -- the client gets
// a slightly more generous floor than it earned. Failing the poll
// instead would turn a transient engine blip into a dead sign-in.
func (s *Server) touchDevicePoll(ctx context.Context, id string, at time.Time, interval int) {
	if err := s.DeviceCodes.TouchPoll(ctx, id, at, interval); err != nil && s.Logger != nil {
		s.Logger.Warn("device_grant_poll_touch_failed",
			slog.String("device_code_id", id),
			slog.String("error", err.Error()))
	}
}
