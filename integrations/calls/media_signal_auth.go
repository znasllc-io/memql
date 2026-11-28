package calls

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

const mediaAdmissionAttribute = "memql.calls.admission"

// The native proof is signed independently of the provider JWT. A provider
// refresh can extend a JWT, but cannot change this session/admission binding.
// Resource ingress has its own native lease + DeleteIngress lifecycle and uses
// the private trusted ingress network. It cannot invent a browser admission.
type mediaSignalBinding struct {
	Version                                int `json:"v"`
	RoomID, Identity, Kind                 string
	SeatID, SessionID, KeyID, CredentialID string
	JoinedAt                               time.Time
	GrantID, ApprovalDigest                string
	LeaseID                                string
}

func (b mediaSignalBinding) valid() bool {
	if b.Version != 1 || b.RoomID == "" || b.Identity == "" || len(b.Identity) > 256 {
		return false
	}
	if b.Kind == string(callrules.KindSampler) {
		return b.LeaseID != "" && b.SeatID == "" && b.SessionID == "" && b.GrantID == ""
	}
	return (b.Kind == string(callrules.KindPerson) || b.Kind == string(callrules.KindAgent) || b.Kind == string(callrules.KindAvatar)) && b.SeatID != "" && b.SessionID != "" && !b.JoinedAt.IsZero() && b.LeaseID == "" && (b.GrantID == "") == (b.ApprovalDigest == "")
}
func signMediaBinding(b mediaSignalBinding, pepper []byte) (string, error) {
	if !b.valid() || len(pepper) < 16 {
		return "", accountRefusal("media_admission_invalid")
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return "", e
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	h := hmac.New(sha256.New, pepper)
	_, _ = h.Write([]byte("callkey-media-admission-v1:" + body))
	return body + "." + hex.EncodeToString(h.Sum(nil)), nil
}
func verifyMediaBinding(raw string, pepper []byte) (mediaSignalBinding, error) {
	var b mediaSignalBinding
	if len(raw) > 4096 || len(pepper) < 16 {
		return b, accountRefusal("media_admission_invalid")
	}
	p := strings.Split(raw, ".")
	if len(p) != 2 {
		return b, accountRefusal("media_admission_invalid")
	}
	h := hmac.New(sha256.New, pepper)
	_, _ = h.Write([]byte("callkey-media-admission-v1:" + p[0]))
	sig, e := hex.DecodeString(p[1])
	if e != nil || !hmac.Equal(sig, h.Sum(nil)) {
		return b, accountRefusal("media_admission_invalid")
	}
	data, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil || json.Unmarshal(data, &b) != nil || !b.valid() {
		return b, accountRefusal("media_admission_invalid")
	}
	return b, nil
}
func (s *Service) mediaSeatBinding(ctx context.Context, r roomRow, seat seatRow) (mediaSignalBinding, error) {
	sess, e := s.store.mediaSeatSession(ctx, r, seat, s.now())
	if e != nil {
		return mediaSignalBinding{}, e
	}
	// Bind the participant's admission, never the operator currently inspecting
	// it. An agent controller selecting a human presenter cannot lend its own
	// delegation to that human's media session.
	b := mediaSignalBinding{Version: 1, RoomID: r.ID, Identity: seat.Identity, Kind: seat.Kind, SeatID: seat.ID, SessionID: sess.ID, KeyID: seat.KeyID, CredentialID: seat.CredentialID, JoinedAt: seat.JoinedAt}
	// A native reconnect need not carry the original Agent request context.
	var agent struct{ GrantID, ApprovalDigest string }
	if seat.AgentData != "" {
		if json.Unmarshal([]byte(seat.AgentData), &agent) != nil {
			return mediaSignalBinding{}, accountRefusal("media_admission_invalid")
		}
		b.GrantID, b.ApprovalDigest = agent.GrantID, agent.ApprovalDigest
	}
	return b, nil
}
func (s *Service) mediaSeatProof(ctx context.Context, r roomRow, seat seatRow) (string, error) {
	b, e := s.mediaSeatBinding(ctx, r, seat)
	if e != nil {
		return "", e
	}
	pepper, e := s.pepperOr(ctx, callrules.ErrForbidden)
	if e != nil {
		return "", e
	}
	return signMediaBinding(b, pepper)
}
func (s *Service) mediaSamplerProof(ctx context.Context, r roomRow, j *processingJournal) (string, error) {
	pepper, e := s.pepperOr(ctx, callrules.ErrForbidden)
	if e != nil {
		return "", e
	}
	return signMediaBinding(mediaSignalBinding{Version: 1, RoomID: r.ID, Identity: j.Lease.Identity, Kind: string(callrules.KindSampler), LeaseID: j.Lease.ID}, pepper)
}

func (s *Service) authenticateMediaSignal(ctx context.Context, token string) (*roomRow, MediaServer, MediaCredential, mediaSignalBinding, string, error) {
	var blank mediaSignalBinding
	fail := func(e error) (*roomRow, MediaServer, MediaCredential, mediaSignalBinding, string, error) {
		return nil, MediaServer{}, MediaCredential{}, blank, "", e
	}
	if len(token) > 16384 || len(token) < 32 {
		return fail(accountRefusal("media_admission_invalid"))
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fail(accountRefusal("media_admission_invalid"))
	}
	// Unverified room is only a bounded lookup hint. No claim is used as authority
	// before the issuer and signature verify against that room's registered server.
	var hint struct {
		Video struct {
			Room string `json:"room"`
		} `json:"video"`
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || json.Unmarshal(raw, &hint) != nil || len(hint.Video.Room) > 128 || hint.Video.Room == "" {
		return fail(accountRefusal("media_admission_invalid"))
	}
	r, e := s.store.roomForMediaRoom(ctx, hint.Video.Room)
	if e != nil {
		return fail(e)
	}
	if r == nil {
		return fail(accountRefusal("media_admission_invalid"))
	}
	server, e := s.store.MediaServerByID(ctx, r.MediaServerID)
	if e != nil {
		return fail(e)
	}
	if server == nil || server.State == "removed" {
		return fail(accountRefusal("media_server_unavailable"))
	}
	credential, e := s.credential(ctx, *server)
	if e != nil {
		return fail(e)
	}
	v, e := lkauth.ParseAPIToken(token)
	if e != nil {
		return fail(accountRefusal("media_admission_invalid"))
	}
	signing, e := s.mediaSignalSigningCredential(ctx, *server, credential, v.APIKey())
	if e != nil {
		return fail(e)
	}
	claims, grants, e := v.Verify(signing.Secret)
	if e != nil || claims.ExpiresAt == nil || !s.now().Before(claims.ExpiresAt.Time) || grants.Video == nil || !grants.Video.RoomJoin || grants.Video.Room != r.MediaRoom {
		return fail(accountRefusal("media_admission_invalid"))
	}
	pepper, e := s.pepperOr(ctx, callrules.ErrForbidden)
	if e != nil {
		return fail(e)
	}
	proof := grants.Attributes[mediaAdmissionAttribute]
	b, e := verifyMediaBinding(proof, pepper)
	if e != nil {
		return fail(e)
	}
	if b.RoomID != r.ID || b.Identity != grants.Identity || b.Kind != grants.Attributes[KindAttribute] {
		return fail(accountRefusal("media_admission_invalid"))
	}
	return r, *server, credential, b, proof, nil
}

// LiveKit v1.13.7 refreshToken selects the first entry while ranging its key
// map. The bootstrap provider also has its separately sealed webhook key in
// that map, so a genuine refreshed participant JWT may have either issuer.
// The global webhook credential belongs only to this exact bootstrap registry
// binding; it grants no trust to another server or a replacement credential.
// This selects only JWT signature verification. The independent native proof
// and current admission checks below remain mandatory, and authenticate returns
// the API credential for all upstream remints and control operations.
func (s *Service) mediaSignalSigningCredential(ctx context.Context, server MediaServer, api MediaCredential, issuer string) (MediaCredential, error) {
	if issuer == api.Key {
		return api, nil
	}
	if server.ID != BootstrapMediaServerID || server.CredentialSecret != SecretMediaBootstrap {
		return MediaCredential{}, accountRefusal("media_admission_invalid")
	}
	raw, err := s.store.resolveSecret(ctx, SecretMediaWebhook)
	if err != nil {
		return MediaCredential{}, accountRefusal("media_admission_invalid")
	}
	webhook, err := ParseMediaCredential(raw)
	if err != nil || issuer != webhook.Key {
		return MediaCredential{}, accountRefusal("media_admission_invalid")
	}
	return webhook, nil
}

// Called with the room plus all relevant account/credential/grant gates held.
// Native session and admission authority remains required even for refreshed JWTs.
func (s *Service) checkMediaBinding(ctx context.Context, r roomRow, b mediaSignalBinding) error {
	return s.checkMediaBindingCurrent(ctx, r, b, true)
}
func (s *Service) checkMediaBindingCurrent(ctx context.Context, r roomRow, b mediaSignalBinding, checkFamily bool) error {
	return s.checkMediaBindingSeats(ctx, r, b, checkFamily, nil)
}

// seats is usable only within the gated callback that just read it. It is input
// to current predicates, never a cached admission verdict. Ordinary callers
// retain their fresh reads. Forward and reconciliation supply the post-acquisition
// membership input while holding every room/member gate and never writing seats.
// Even after provider I/O, time/session/credential predicates run again; only the
// unchanged full seat rows are reused for departure and seat identity.
func (s *Service) checkMediaBindingSeats(ctx context.Context, r roomRow, b mediaSignalBinding, checkFamily bool, seats *[]seatRow) error {
	if !b.valid() || b.RoomID != r.ID || !r.open() {
		return accountRefusal("media_admission_closed")
	}
	if e := fundingRoomAllowed(r, s.now()); e != nil {
		return e
	}
	var deadline time.Time
	if seats == nil {
		var err error
		deadline, err = s.departureDeadline(ctx, r, s.now())
		if err != nil {
			return err
		}
	} else {
		_, deadline = departurePolicy(r, *seats)
		if !deadline.IsZero() && !s.now().Before(deadline) {
			return callrules.Refuse(callrules.ErrEnded, "wrap_up", "the host's firm wrap-up deadline has arrived")
		}
	}
	if !deadline.IsZero() && !s.now().Before(deadline) {
		return accountRefusal("media_admission_closed")
	}
	if b.Kind == string(callrules.KindSampler) {
		j, e := s.store.processing(ctx, r.OwnerUserID, r.ID)
		if e != nil {
			return e
		}
		if j == nil || j.Lease.ID != b.LeaseID {
			return accountRefusal("media_processing_lease_closed")
		}
		ok, e := s.processingSampler(ctx, r, b.Identity)
		if e != nil {
			return e
		}
		if !ok {
			return accountRefusal("media_processing_lease_closed")
		}
		return nil
	}
	var seat *seatRow
	if seats == nil {
		var err error
		seat, err = s.store.seatByID(ctx, r.OwnerUserID, b.SeatID)
		if err != nil {
			return err
		}
	} else {
		for _, current := range *seats {
			if current.ID == b.SeatID {
				seat = &current
				break
			}
		}
	}
	if seat == nil || !seat.occupying() || seat.RoomID != r.ID || seat.Identity != b.Identity || seat.Kind != b.Kind || seat.KeyID != b.KeyID || seat.CredentialID != b.CredentialID || !seat.JoinedAt.Equal(b.JoinedAt) {
		return accountRefusal("media_admission_closed")
	}
	sess, e := s.store.sessionByID(ctx, r.OwnerUserID, b.SessionID)
	if e != nil {
		return e
	}
	if sess == nil || !sess.usable(s.now()) || sess.SiteID != r.SiteID || sess.RoomID != r.ID || sess.SeatID != seat.ID || sess.KeyID != b.KeyID || sess.CredentialID != b.CredentialID {
		return accountRefusal("media_session_closed")
	}
	if b.KeyID != "" {
		k, e := s.store.keyByID(ctx, r.OwnerUserID, b.KeyID)
		if e != nil {
			return e
		}
		if k == nil || !keyActive(k.Status) {
			return accountRefusal("media_account_closed")
		}
	}
	if checkFamily {
		if e := s.familySeatAllowedLocked(ctx, r, *seat); e != nil {
			return e
		}
	}
	if b.GrantID != "" {
		site := callrules.SiteRef{SiteID: r.SiteID, OwnerUserID: r.OwnerUserID}
		ctx = context.WithValue(ctx, agentCallContext{}, b.GrantID)
		ctx = context.WithValue(ctx, agentApprovalContext{}, b.ApprovalDigest)
		if e := s.delegatedEffectAllowed(ctx, site, b.KeyID); e != nil {
			return e
		}
		d, e := s.store.delegation(ctx, r.OwnerUserID, b.GrantID)
		if e != nil {
			return e
		}
		if d == nil {
			return accountRefusal("delegation_inactive")
		}
		accepted := false
		for _, a := range d.State.Acceptances {
			if a.SeatID == seat.ID && a.RoomID == r.ID && a.JoinedAt.Equal(seat.JoinedAt) && checkAgentAcceptance(d, &a.Acceptance) == nil {
				accepted = true
				break
			}
		}
		if !accepted {
			return accountRefusal("agent_acceptance_required")
		}
		request := seatAgentAdmissionID(seat.AgentData)
		scope := "host"
		if request != "" {
			scope = "join"
		}
		if !slices.Contains(d.State.Request.Scopes, scope) {
			return accountRefusal("delegation_scope")
		}
		if request != "" {
			j, e := s.store.agentAdmissions(ctx, r.OwnerUserID, r.ID)
			if e != nil {
				return e
			}
			a := j.find(request)
			if a == nil {
				return accountRefusal("agent_review_required")
			}
			ctx = context.WithValue(ctx, guestAgentContext{}, guestAgentContext{RoomID: r.ID, RequestID: request, GrantID: b.GrantID, ApprovalDigest: b.ApprovalDigest, AdmissionKey: a.AdmissionKey})
			return s.guestAgentAdmissionAllowed(ctx, site, r, sess, false)
		}
	} else if seat.Kind == string(callrules.KindAgent) {
		return accountRefusal("agent_approval_required")
	}
	return nil
}

// Include every current room participant's authority: sampler leases depend on
// their consent and credentials, and a guest agent also depends on its reviewer.
// Re-read this set after acquiring it to avoid locking a stale room membership.
func (s *Service) mediaSignalGateKeys(ctx context.Context, r roomRow, b mediaSignalBinding) ([]string, error) {
	keys, _, err := s.mediaSignalMembership(ctx, r, b)
	return keys, err
}

func (s *Service) mediaSignalMembership(ctx context.Context, r roomRow, b mediaSignalBinding) ([]string, []seatRow, error) {
	keys := []string{roomGate(r.ID), accountGate(r.KeyID), keyGate(r.KeyID)}
	// Independent fresh reads on request connections, never the gate's
	// transaction. Join both before returning, including on cancellation; no
	// goroutine can outlive the gate. The two-query bound does not grow with
	// participants, and pre-acquisition results never enter the callback.
	var seats []seatRow
	var journal *agentAdmissionJournal
	var seatErr, journalErr error
	mediaSignalReadPair(func() {
		seats, seatErr = s.store.seatsForRoom(ctx, r.OwnerUserID, r.ID)
	}, func() {
		journal, journalErr = s.store.agentAdmissions(ctx, r.OwnerUserID, r.ID)
	})
	if seatErr != nil {
		return nil, nil, seatErr
	}
	if journalErr != nil {
		return nil, nil, journalErr
	}
	for _, seat := range seats {
		if seat.KeyID != "" {
			keys = append(keys, accountGate(seat.KeyID), keyGate(seat.KeyID))
		}
		var a struct{ GrantID string }
		if seat.AgentData != "" && json.Unmarshal([]byte(seat.AgentData), &a) == nil && a.GrantID != "" {
			keys = append(keys, delegationGate(a.GrantID))
		}
	}
	if b.KeyID != "" {
		keys = append(keys, accountGate(b.KeyID), keyGate(b.KeyID))
	}
	if b.GrantID != "" {
		keys = append(keys, delegationGate(b.GrantID))
	}
	if journal != nil {
		for _, a := range journal.Requests {
			if a.ReviewerKeyID != "" {
				keys = append(keys, accountGate(a.ReviewerKeyID), keyGate(a.ReviewerKeyID))
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), seats, nil
}

// Both reads finish before the caller can release its gates, even on panic.
// Re-panic on the calling goroutine to preserve its existing recovery boundary;
// a child read must not turn a recoverable request panic into a process crash.
func mediaSignalReadPair(first, second func()) {
	var reads sync.WaitGroup
	var childPanic any
	reads.Add(1)
	go func() {
		defer reads.Done()
		defer func() { childPanic = recover() }()
		second()
	}()
	defer reads.Wait()
	first()
	reads.Wait()
	if childPanic != nil {
		panic(childPanic)
	}
}

func (s *Service) mediaSignalGated(ctx context.Context, hint roomRow, b mediaSignalBinding, fn func(context.Context, roomRow) error) error {
	return s.mediaSignalGatedSeats(ctx, hint, b, func(ctx context.Context, r roomRow, _ []seatRow) error {
		return fn(ctx, r)
	})
}

func (s *Service) mediaSignalGatedSeats(ctx context.Context, hint roomRow, b mediaSignalBinding, fn func(context.Context, roomRow, []seatRow) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		fresh := memql.ContextWithFreshRead(ctx)
		trace := mediaSignalTraceFrom(ctx)
		phaseAt := time.Now()
		keys, e := s.mediaSignalGateKeys(fresh, hint, b)
		trace.record("gate_keys_before", time.Since(phaseAt), e)
		if e != nil {
			return e
		}
		phaseAt = time.Now()
		release, e := s.gates(ctx, keys...)
		trace.record("gate_acquire", time.Since(phaseAt), e)
		if e != nil {
			return e
		}
		retry, e := func() (bool, error) {
			// Provider/transport callbacks can panic. The HTTP recovery boundary
			// must never retain account/room write gates after such a failure.
			defer release()
			phaseAt = time.Now()
			r, e := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
			trace.record("gate_room_read", time.Since(phaseAt), e)
			if e != nil {
				return false, e
			}
			if r == nil {
				return false, accountRefusal("media_admission_closed")
			}
			phaseAt = time.Now()
			current, seats, e := s.mediaSignalMembership(fresh, *r, b)
			trace.record("gate_keys_after", time.Since(phaseAt), e)
			if e != nil {
				return false, e
			}
			if !slices.Equal(keys, current) {
				hint = *r
				return true, nil
			}
			return false, fn(fresh, *r, seats)
		}()
		if retry {
			continue
		}
		return e
	}
	return accountRefusal("media_authority_changed")
}
