package memql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"

	"github.com/znasllc-io/memql/core/num"
)

// The one sign-in a process keeps per vendor (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions A1, A2, A4 and A5).
//
// THE INVARIANT: a process presents an identity token to a vendor at most once,
// however many clients it holds and however many callers arrive together.
//
// WHY IT EXISTS. The cluster's account at a vendor is workload identity
// federation: the pod's projected Kubernetes token is exchanged for a
// short-lived bearer. Anthropic accepts a token carrying a `jti` ONCE. The
// engine used to build one credential per provider record -- four for Claude --
// and rebuilt all of them on every provider reload, so one identity token was
// presented four times and only the first could be accepted. That was never
// observed, because federation had never been run against a vendor; it was
// read out of the code against the vendor's documentation, and this file is
// the repair. Every client of a vendor now takes its bearer from the one
// vendorSignIn processVendorSignIn returns, so the number of clients no longer
// decides the number of sign-ins.
//
// ONE EXCHANGE AT A TIME, AS A FLIGHT. The lock is never held across the
// exchange. The first caller that needs a sign-in starts it on a goroutine of
// its own; callers with a bearer that still works are served at once while it
// runs, callers with none wait for it and leave when their own context ends,
// and the pause before another attempt is counted from when the attempt ENDED.
// A vendor that hangs therefore costs one attempt, not one per waiting caller.
//
// THREE ANSWERS A SIGN-IN CAN GET, and what each allows:
//
//   - ACCEPTED. The token is never presented again to a replay-protected
//     vendor: it cannot succeed, and every attempt is a refusal in the vendor's
//     console. The engine waits for kubelet to rotate the file and says so.
//   - REFUSED (a 4xx other than 408, 425 and 429; signInStatusRefuses). A
//     configuration answer -- the rule, the subject, the audience -- that only
//     a person changes, so it is never retried on a timer. One exception:
//     AllowRetry, which Verify and Apply call, lets a refused token be
//     presented once more, because the operator may have just fixed the
//     vendor's side and should not wait out a rotation to find out.
//   - NO ANSWER (a transport fault, a 5xx, or 408/425/429: "come back
//     later"). The vendor did not judge the token, so it is presented again,
//     no more often than vendorSignInRetryBackoff.
//
// In every case the last good bearer keeps being handed out until it expires:
// a newer sign-in that failed is not a reason to drop one that works.
//
// WHAT SURVIVES WHAT. The sign-in is per PROCESS. It survives a provider
// reload while the federation ids are unchanged (processVendorSignIn returns
// the same object for the same fingerprint). It does not survive a container
// restart, and a restarted container reads the same token file: on a
// replay-protected vendor its first sign-in is then refused until the token
// rotates. That refusal is recorded with MayBeReplay, so it does not read as a
// broken configuration (A4), and the engine presents the next token when it
// arrives (ai_vendor_signin_boot.go).
//
// THE RECORD is what `memql provider-auth check` reports instead of signing
// in itself (A5): two signers in one pod cannot both succeed under a replay
// rule. It is written to a small file in the pod's temporary directory and
// carries no credential: not the bearer, not the identity token. It says two
// things about an accepted sign-in, because they are two answers: that the
// vendor accepted the identity token (Outcome), and whether the vendor's API
// then took the bearer (Proof). Only both make a vendor this node can use.
//
// Regression tests: ai_vendor_signin_test.go. Never run against a vendor;
// the design record's A9 is that run.

const (
	// vendorSignInRefreshWindow is how long before expiry a new sign-in is
	// attempted. The Anthropic SDK's own advisory threshold, adopted so both
	// vendors behave alike.
	vendorSignInRefreshWindow = 120 * time.Second

	// vendorSignInRetryBackoff bounds how often the SAME identity token is
	// presented again where presenting it again is allowed at all: after a
	// sign-in the vendor never answered, and on a vendor with no replay rule
	// once its bearer has expired.
	vendorSignInRetryBackoff = 5 * time.Second

	// vendorSignInUnknownLifetime is the lifetime of a bearer whose vendor
	// named none. Short rather than forever: an early sign-in costs one
	// request, and a bearer cached forever stops working silently.
	vendorSignInUnknownLifetime = 60 * time.Second

	// vendorSignInExchangeTimeout bounds one exchange. The exchange does not
	// follow the calling request's cancellation: a sign-in serves every caller
	// in the process, and abandoning one mid-flight can leave a token the
	// vendor has accepted and the engine has no bearer for.
	vendorSignInExchangeTimeout = 20 * time.Second
)

// The outcomes a record carries. `waiting` is the engine's own: it has a token
// it must not present and no bearer to hand out.
const (
	signInOutcomeOK      = "ok"
	signInOutcomeDenied  = "denied"
	signInOutcomeError   = "error"
	signInOutcomeWaiting = "waiting"
	// signInOutcomeNotConfigured: the engine found no federation ids for this
	// vendor, in the environment or in the cluster's storage. Nobody set it up.
	signInOutcomeNotConfigured = "notConfigured"
	// signInOutcomeNotUsed: this node type calls no models and signs in to no
	// vendor, whatever is configured.
	signInOutcomeNotUsed = "notUsed"
	// signInOutcomeMisconfigured: ids are set and the engine itself refuses
	// them before any vendor is asked: a partial set, or an identity token it
	// cannot use. Detail carries the engine's own reason.
	signInOutcomeMisconfigured = "misconfigured"
)

// vendorSignInConfig is everything one vendor's sign-in is built from.
type vendorSignInConfig struct {
	// Vendor is metrics.FederationVendorAnthropic or FederationVendorOpenAI.
	Vendor string
	// Fingerprint identifies this configuration: a hash over the vendor's ids,
	// the token file and the endpoint. Two configs with one fingerprint are
	// the same sign-in.
	Fingerprint string
	TokenFile   string
	// ReplayProtected says the vendor accepts an identity token once.
	ReplayProtected bool
	// Ids are the vendor's ids, for the record. Never a credential.
	Ids map[string]string
	// Exchange presents one identity token and returns the bearer. A zero
	// expiresAt means the vendor named no lifetime. A status that refuses the
	// token (signInStatusRefuses) is returned as *vendorExchangeRefused;
	// anything else means no answer.
	Exchange func(ctx context.Context, identityToken string) (bearer string, expiresAt time.Time, err error)
}

// vendorExchangeRefused is a sign-in the vendor understood and refused.
type vendorExchangeRefused struct {
	Status int
	// Body is the vendor's own reason, already truncated. It is what the
	// vendor's console shows, so an operator holding it can act.
	Body string
}

func (e *vendorExchangeRefused) Error() string {
	return fmt.Sprintf("the vendor refused the sign-in (status %d): %s", e.Status, e.Body)
}

// signInStatusRefuses reports whether a sign-in's HTTP status is the vendor
// REFUSING the identity token: an answer about the configuration, which only a
// person changes and the engine therefore never retries by itself.
//
// NOT EVERY 4xx IS THAT. 408, 425 and 429 ask the engine to come back: the
// vendor did not judge the token, and by its own account did not consume it.
// Read as refusals they left a node without the vendor until its identity
// token was replaced (most of an hour), for one rate-limited request, and
// ticked the denial counter operators are told to alert on. They are "no
// answer", retried on the sign-in's back-off like a transport fault.
//
// One function, three callers that must agree: both vendors' exchanges and the
// exchange observer (ai_federation_observer.go).
func signInStatusRefuses(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return true
}

// vendorSignInWaiting is returned when the only identity token on disk has
// already been presented and there is no bearer left to hand out.
type vendorSignInWaiting struct {
	Vendor string
	// UsedAt is when the token on disk was presented.
	UsedAt time.Time
	// NextTokenDue is when kubelet is expected to replace it; zero when the
	// token does not say when it was issued.
	NextTokenDue time.Time
}

func (e *vendorSignInWaiting) Error() string {
	due := "soon"
	if !e.NextTokenDue.IsZero() {
		due = "about " + e.NextTokenDue.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"%s sign-in is waiting for the next identity token: the one on disk was already presented at %s "+
			"and the vendor accepts a token once. The next is due %s",
		e.Vendor, e.UsedAt.UTC().Format(time.RFC3339), due)
}

// The proof a record carries for an accepted sign-in: whether the vendor's API
// has taken the bearer. An accepted sign-in is a bearer; it is not yet a
// vendor this node can use.
const (
	// signInProofOK: the vendor listed its models with the bearer, or served a
	// call with it.
	signInProofOK = "ok"
	// signInProofRefused: the vendor's API answered the bearer with 401 or 403.
	signInProofRefused = "refused"
	// signInProofError: the first API call with the bearer got no answer.
	signInProofError = "error"
)

// vendorSignInRecord is the last thing that happened to a vendor's sign-in in
// this process. Every field is safe to show an operator.
type vendorSignInRecord struct {
	Vendor  string `json:"vendor"`
	Outcome string `json:"outcome"`
	// Status is the vendor's HTTP status on a refusal.
	Status int       `json:"status,omitempty"`
	At     time.Time `json:"at"`
	// ExpiresAt is when the bearer from an accepted sign-in stops working.
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	// Detail is the vendor's reason on a refusal, or the fault on an error.
	Detail string `json:"detail,omitempty"`
	// MayBeReplay marks a refusal, by a vendor that accepts a token once, of a
	// token issued before this process started: an earlier process in this pod
	// may already have used it. It is also true of every NEW pod's first
	// token, which kubelet mints before the container starts, so it says only
	// that the next token will settle the question (the engine tries again
	// when that token is due).
	MayBeReplay bool `json:"mayBeReplay,omitempty"`
	// NextTokenDue is when the identity token is expected to be replaced.
	NextTokenDue  time.Time         `json:"nextTokenDue,omitempty"`
	Fingerprint   string            `json:"fingerprint"`
	Ids           map[string]string `json:"ids,omitempty"`
	TokenSubject  string            `json:"tokenSubject,omitempty"`
	TokenAudience []string          `json:"tokenAudience,omitempty"`
	// Proof says whether the vendor's API has taken a bearer of this sign-in:
	// "" while nothing has used one, then one of the signInProof values.
	// ProofStatus and ProofDetail are the API's answer when it did not, and
	// ProofAt is when it was last shown. A renewal made while the last bearer
	// still worked keeps the proof (see fly); ProofAt says how old it is.
	Proof       string    `json:"proof,omitempty"`
	ProofStatus int       `json:"proofStatus,omitempty"`
	ProofDetail string    `json:"proofDetail,omitempty"`
	ProofAt     time.Time `json:"proofAt,omitempty"`
	// ModelsListed is how many models the vendor listed with the bearer, when
	// a model list is what proved it.
	ModelsListed int `json:"modelsListed,omitempty"`
	// BearerDroppedAt is when the vendor's API answered the bearer of this
	// sign-in with 401 and the engine dropped it. Zero while it is in use.
	BearerDroppedAt time.Time `json:"bearerDroppedAt,omitempty"`
	Pid             int       `json:"pid"`
}

// signInFlight is one exchange in progress. done closes once the vendor has
// answered, or the attempt gave up, and the result is on the sign-in.
type signInFlight struct {
	done chan struct{}
}

// vendorSignIn is one vendor's sign-in for this process.
type vendorSignIn struct {
	cfg vendorSignInConfig
	now func() time.Time
	// started is when the PROCESS started, which is what decides whether a
	// refused token may have been used before it (MayBeReplay).
	started time.Time

	// mu guards everything below. It is NEVER held across an exchange: the
	// exchange runs as a flight, and callers wait on the flight, not the lock.
	mu        sync.Mutex
	bearer    string
	expiresAt time.Time

	// The last identity token presented, by hash, and how the vendor answered.
	// presentedAt is when the attempt ENDED: the pause before the same token
	// may be presented again is counted from the answer, or from giving up.
	hasPresented     bool
	presented        [sha256.Size]byte
	presentedOutcome string
	presentedAt      time.Time
	// retryAllowed is set by AllowRetry and spent by the next presentation.
	retryAllowed bool
	// lastErr is what a caller gets while nothing may be presented.
	lastErr error
	// flight is the exchange in progress, nil when there is none.
	flight *signInFlight

	hasRecord bool
	record    vendorSignInRecord
	// bearerProven says the vendor's API has taken the bearer IN HAND. The
	// record's Proof can be older than that: a renewal keeps it (see fly).
	bearerProven bool
}

func newVendorSignIn(cfg vendorSignInConfig, now func() time.Time, started time.Time) *vendorSignIn {
	if now == nil {
		now = time.Now
	}
	return &vendorSignIn{cfg: cfg, now: now, started: started}
}

// Bearer returns a bearer the vendor will accept now, signing in when one is
// due and a token may be presented.
//
// ONE EXCHANGE AT A TIME, AND NOBODY WAITS WHO DOES NOT HAVE TO. Many callers
// arrive together when a bearer nears expiry or a process starts. Exactly one
// exchange is made for them (a second would present the same identity token,
// which is the failure this type exists to prevent), and:
//
//   - a caller with a bearer that still works gets it at once, while the
//     renewal goes on behind it;
//   - a caller with none waits for the exchange, and stops waiting when its
//     own context ends. The exchange itself is not stopped: it serves every
//     other caller, and abandoning it can leave a token the vendor accepted
//     and the engine has no bearer for.
//
// This used to hold the lock across the exchange. A vendor that hung then made
// every caller wait in turn and make an attempt of its own, context or not,
// including callers that already held a working bearer.
func (s *vendorSignIn) Bearer(ctx context.Context) (string, time.Time, error) {
	for {
		bearer, expiresAt, wait, err := s.bearerOrFlight(ctx)
		if wait == nil {
			return bearer, expiresAt, err
		}
		select {
		case <-wait:
			// The exchange ended. Ask again: there is a bearer now, or there is
			// the reason there is not.
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		}
	}
}

// bearerOrFlight answers from what is in hand. When the caller has to wait for
// an exchange it returns that exchange's done channel, starting the exchange
// if none is running.
func (s *vendorSignIn) bearerOrFlight(ctx context.Context) (string, time.Time, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.bearer != "" && s.expiresAt.Sub(now) > vendorSignInRefreshWindow {
		return s.bearer, s.expiresAt, nil, nil
	}
	valid := s.bearerValid(now)

	if s.flight == nil {
		// Re-read on every sign-in: kubelet rewrites a projected token in
		// place, and a copy taken earlier stops being valid on a schedule
		// nothing here controls. A failed read presents nothing and marks
		// nothing as presented.
		token, err := readFileTrimmed(s.cfg.TokenFile)
		if err != nil {
			if valid {
				return s.bearer, s.expiresAt, nil, nil
			}
			return "", time.Time{}, nil, fmt.Errorf("cannot read the identity token at %s: %w", s.cfg.TokenFile, err)
		}
		hash := sha256.Sum256([]byte(token))
		if s.hasPresented && hash == s.presented {
			if hold, heldErr := s.holdSameToken(token, now); hold {
				if valid {
					return s.bearer, s.expiresAt, nil, nil
				}
				return "", time.Time{}, nil, heldErr
			}
		}
		s.flight = &signInFlight{done: make(chan struct{})}
		// The exchange keeps the caller's values (a client's request says
		// which address its vendor is reached at) and not its cancellation.
		go s.fly(context.WithoutCancel(ctx), s.flight, token, hash)
	}
	if valid {
		return s.bearer, s.expiresAt, nil, nil
	}
	return "", time.Time{}, s.flight.done, nil
}

// holdSameToken decides whether the token on disk, which is the one last
// presented, must NOT be presented again right now. hold is false when it may.
// The caller holds s.mu.
func (s *vendorSignIn) holdSameToken(token string, now time.Time) (hold bool, err error) {
	switch s.presentedOutcome {
	case signInOutcomeOK:
		if s.bearerValid(now) {
			// The bearer this token bought still works. Nothing is due.
			return true, nil
		}
		if s.cfg.ReplayProtected {
			waiting := &vendorSignInWaiting{
				Vendor:       s.cfg.Vendor,
				UsedAt:       s.presentedAt,
				NextTokenDue: nextIdentityTokenDue(token),
			}
			if !s.hasRecord || s.record.Outcome != signInOutcomeWaiting {
				s.setRecord(vendorSignInRecord{
					Outcome:      signInOutcomeWaiting,
					At:           now,
					Detail:       "the identity token on disk was already presented; waiting for the next one",
					NextTokenDue: waiting.NextTokenDue,
				}, token)
			}
			return true, waiting
		}
		// No replay rule: the token may buy another bearer, but not on every
		// call while the first one is dead.
		if now.Sub(s.presentedAt) < vendorSignInRetryBackoff {
			return true, s.lastErrOr("the bearer has expired and the identity token was presented moments ago")
		}
		return false, nil
	case signInOutcomeDenied:
		if s.retryAllowed {
			return false, nil
		}
		return true, s.lastErrOr("the vendor refused this identity token")
	default:
		// The vendor never answered. The token may not have been consumed, so
		// it is presented again, but not on every call.
		if now.Sub(s.presentedAt) < vendorSignInRetryBackoff {
			return true, s.lastErrOr("the last sign-in did not complete")
		}
		return false, nil
	}
}

func (s *vendorSignIn) lastErrOr(fallback string) error {
	if s.lastErr != nil {
		return s.lastErr
	}
	return fmt.Errorf("%s sign-in: %s", s.cfg.Vendor, fallback)
}

// fly makes one sign-in with the token and records what the vendor said. It
// runs on a goroutine of its own and holds s.mu only to record.
func (s *vendorSignIn) fly(ctx context.Context, flight *signInFlight, token string, hash [sha256.Size]byte) {
	// Closed last, after the result is on the sign-in and the lock is released.
	defer close(flight.done)

	exchangeCtx, cancel := context.WithTimeout(ctx, vendorSignInExchangeTimeout)
	bearer, expiresAt, err := s.cfg.Exchange(exchangeCtx, token)
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.flight = nil
	// The clock is read after the exchange: it may have spent seconds failing,
	// and the pause before another attempt starts from here.
	answered := s.now()
	s.hasPresented, s.presented, s.presentedAt, s.retryAllowed = true, hash, answered, false

	var refused *vendorExchangeRefused
	switch {
	case err == nil:
		if expiresAt.IsZero() {
			expiresAt = answered.Add(vendorSignInUnknownLifetime)
		}
		rec := vendorSignInRecord{Outcome: signInOutcomeOK, At: answered, ExpiresAt: expiresAt}
		// A RENEWAL WHILE THE LAST BEARER STILL WORKS KEEPS ITS PROOF. Every
		// node renews about once an hour, and the call that starts a renewal is
		// served with the bearer already in hand, so nothing uses the new one
		// until the next call. Cleared here, the proof would make a node with
		// little traffic read as "not verified" from each renewal to its next
		// call. The new bearer comes from the same ids and the same service
		// account as the one the API took minutes ago, so the record keeps
		// saying so, with the time it was last shown (ProofAt). A sign-in after
		// the bearer expired or was dropped starts unproven: the call that
		// waited for it is about to prove it either way.
		if s.hasRecord && s.record.Outcome == signInOutcomeOK && s.record.BearerDroppedAt.IsZero() && s.bearerValid(answered) {
			rec.Proof, rec.ProofStatus, rec.ProofDetail = s.record.Proof, s.record.ProofStatus, s.record.ProofDetail
			rec.ProofAt, rec.ModelsListed = s.record.ProofAt, s.record.ModelsListed
		}
		s.bearer, s.expiresAt, s.bearerProven = bearer, expiresAt, false
		s.presentedOutcome, s.lastErr = signInOutcomeOK, nil
		s.setRecord(rec, token)

	case errors.As(err, &refused):
		// THE LAST GOOD BEARER KEEPS WORKING. A refusal is an answer that
		// arrives while a valid bearer may still be in hand, and dropping it
		// would turn a misconfiguration an operator has time to fix into an
		// immediate outage. Bearer hands it out for as long as it is valid.
		s.presentedOutcome = signInOutcomeDenied
		// The runbook is named because a refusal is a configuration answer, and
		// this error is what a failed model call carries to whoever reads it.
		s.lastErr = fmt.Errorf("%s sign-in: %w. Runbook: %s", s.cfg.Vendor, err, federationRunbookFor(s.cfg.Vendor))
		rec := vendorSignInRecord{
			Outcome: signInOutcomeDenied,
			Status:  refused.Status,
			At:      answered,
			Detail:  refused.Body,
		}
		// ONLY A VENDOR WITH A REPLAY RULE CAN MEAN THIS. A token issued before
		// this process started may already have been used by the process this
		// one replaced, so the refusal may say nothing about the configuration.
		// It is equally true of a new pod's first token, so the mark claims no
		// more than "the next token settles it", and the engine presents that
		// token when it is due (ai_vendor_signin_boot.go). A vendor that
		// accepts a token any number of times refused for a reason of its own.
		if s.cfg.ReplayProtected && identityTokenIssuedBefore(token, s.started) {
			rec.MayBeReplay = true
			rec.NextTokenDue = nextIdentityTokenDue(token)
		}
		s.setRecord(rec, token)

	default:
		s.presentedOutcome = signInOutcomeError
		s.lastErr = fmt.Errorf("%s sign-in did not complete: %w", s.cfg.Vendor, err)
		s.setRecord(vendorSignInRecord{
			Outcome: signInOutcomeError,
			At:      answered,
			Detail:  truncateForLog(err.Error(), maxFederationBodyNote),
		}, token)
	}
}

func (s *vendorSignIn) bearerValid(now time.Time) bool {
	return s.bearer != "" && s.expiresAt.After(now)
}

// waitIdle returns once no exchange is in progress.
func (s *vendorSignIn) waitIdle() {
	for {
		s.mu.Lock()
		flight := s.flight
		s.mu.Unlock()
		if flight == nil {
			return
		}
		<-flight.done
	}
}

// Invalidate drops a bearer the vendor answered an API call with 401 for, and
// says so on the record. The next Bearer signs in if a token may be presented.
//
// It names the bearer that failed, and drops the one in hand only if it is that
// one: a call that started with the previous bearer must not throw away the
// one a newer sign-in has just bought.
func (s *vendorSignIn) Invalidate(bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bearer == "" || s.bearer != bearer {
		return
	}
	s.bearer, s.expiresAt, s.bearerProven = "", time.Time{}, false
	// The record still said "accepted", which is what the check command reads
	// as a working vendor. It was accepted; it is not working.
	if s.hasRecord && s.record.Outcome == signInOutcomeOK && s.record.BearerDroppedAt.IsZero() {
		s.record.BearerDroppedAt = s.now()
		writeVendorSignInStatus(s.record)
	}
}

// newTokenOnDisk reports whether the identity token on disk is one this sign-in
// has not presented. It presents nothing. The engine asks it before spending a
// sign-in pass on a token kubelet may not have replaced yet
// (ai_vendor_signin_boot.go). False when the file cannot be read.
func (s *vendorSignIn) newTokenOnDisk() bool {
	token, err := readFileTrimmed(s.cfg.TokenFile)
	if err != nil {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.hasPresented || hash != s.presented
}

// AllowRetry lets a REFUSED identity token be presented once more. Verify and
// Apply call it: a person has asked, possibly right after fixing the vendor's
// side. It never allows an accepted token to be presented again.
func (s *vendorSignIn) AllowRetry() {
	s.mu.Lock()
	if s.presentedOutcome == signInOutcomeDenied {
		s.retryAllowed = true
	}
	s.mu.Unlock()
}

// NoteModelList records what the vendor's API answered when it was asked for
// its models with the bearer of an accepted sign-in: n models, or err.
//
// A refused sign-in is one answer and a refused API call is another, and the
// second used to be logged and left off the record, so an accepted sign-in
// whose bearer the API would not take still read as a working vendor.
func (s *vendorSignIn) NoteModelList(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasRecord || s.record.Outcome != signInOutcomeOK {
		return
	}
	s.record.ProofAt = s.now()
	switch status, detail := apiAnswerOf(err); {
	case err == nil:
		s.record.Proof, s.record.ProofStatus, s.record.ProofDetail = signInProofOK, 0, ""
		s.record.ModelsListed = n
		s.bearerProven = true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		s.record.Proof, s.record.ProofStatus, s.record.ProofDetail = signInProofRefused, status, detail
		// The next call the vendor serves with this bearer is newer evidence
		// than this list, and NoteServed must be free to record it.
		s.bearerProven = false
	default:
		s.record.Proof, s.record.ProofStatus, s.record.ProofDetail = signInProofError, status, detail
		s.bearerProven = false
	}
	writeVendorSignInStatus(s.record)
}

// NoteServed records that the vendor's API served a call made with bearer. A
// served call proves a bearer as well as a model list does, and it is the only
// proof a bearer gets when it was bought by a renewal rather than at start.
//
// Called on every served call, so it does nothing unless bearer is the one in
// hand and that bearer is not proven yet: one write of the status file per
// bearer, about once an hour.
func (s *vendorSignIn) NoteServed(bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bearer == "" || s.bearer != bearer || s.bearerProven || !s.hasRecord || s.record.Outcome != signInOutcomeOK {
		return
	}
	s.bearerProven = true
	s.record.Proof, s.record.ProofStatus, s.record.ProofDetail = signInProofOK, 0, ""
	s.record.ProofAt = s.now()
	writeVendorSignInStatus(s.record)
}

// apiAnswerOf reads the status and the reason out of an error from either
// vendor's API, or from the sign-in itself. Zero and the error's own text when
// it carries no status.
func apiAnswerOf(err error) (status int, detail string) {
	if err == nil {
		return 0, ""
	}
	var fromAnthropic *anthropic.Error
	var fromOpenAI *openai.Error
	var refused *vendorExchangeRefused
	switch {
	case errors.As(err, &fromAnthropic):
		status = fromAnthropic.StatusCode
	case errors.As(err, &fromOpenAI):
		status = fromOpenAI.StatusCode
	case errors.As(err, &refused):
		status = refused.Status
	}
	return status, truncateForLog(err.Error(), maxFederationBodyNote)
}

// Record returns the last thing that happened, or false before any attempt.
func (s *vendorSignIn) Record() (vendorSignInRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record, s.hasRecord
}

// setRecord stamps the standing fields onto rec, keeps it and writes the
// status file. The caller holds s.mu.
func (s *vendorSignIn) setRecord(rec vendorSignInRecord, token string) {
	rec.Vendor = s.cfg.Vendor
	rec.Fingerprint = s.cfg.Fingerprint
	rec.Ids = s.cfg.Ids
	rec.Pid = os.Getpid()
	if claims, err := parseJWTClaims(token); err == nil {
		rec.TokenSubject, _ = claims["sub"].(string)
		rec.TokenAudience = jwtAudienceValues(claims)
	}
	s.record, s.hasRecord = rec, true
	writeVendorSignInStatus(rec)
}

// adoptAccepted carries an accepted presentation over from the sign-in this
// one replaces, so the same token is not presented under new ids.
func (s *vendorSignIn) adoptAccepted(hash [sha256.Size]byte, at time.Time) {
	s.mu.Lock()
	s.hasPresented, s.presented, s.presentedOutcome, s.presentedAt = true, hash, signInOutcomeOK, at
	s.mu.Unlock()
}

// acceptedPresentation reports the token this sign-in presented and the vendor
// accepted, if that is how its last presentation went. It waits for an
// exchange in progress first: a token the vendor is accepting at this moment
// is one the next sign-in must not present.
func (s *vendorSignIn) acceptedPresentation() ([sha256.Size]byte, time.Time, bool) {
	s.waitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasPresented || s.presentedOutcome != signInOutcomeOK {
		return [sha256.Size]byte{}, time.Time{}, false
	}
	return s.presented, s.presentedAt, true
}

// identityTokenTimes reads iat and exp out of a token, zero when absent.
//
// A claim is whole seconds, or a fraction of one (RFC 7519's NumericDate),
// which reads to the second. A number an int64 cannot hold reads as ABSENT:
// both callers treat a zero time as "the token does not say", and a made-up
// date would call a fresh token replayed or promise a rotation that never
// comes. num.WholeInt64 is the bound (memql#4779).
func identityTokenTimes(token string) (iat, exp time.Time) {
	claims, err := parseJWTClaims(token)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	read := func(key string) time.Time {
		seconds, ok := claims[key].(float64)
		if !ok {
			return time.Time{}
		}
		whole, ok := num.WholeInt64(math.Trunc(seconds))
		if !ok {
			return time.Time{}
		}
		return time.Unix(whole, 0).UTC()
	}
	return read("iat"), read("exp")
}

func identityTokenIssuedBefore(token string, t time.Time) bool {
	iat, _ := identityTokenTimes(token)
	return !iat.IsZero() && iat.Before(t)
}

// nextIdentityTokenDue is when kubelet replaces a projected token: at 80
// percent of its lifetime. Zero when the token does not carry both times.
func nextIdentityTokenDue(token string) time.Time {
	iat, exp := identityTokenTimes(token)
	if iat.IsZero() || exp.IsZero() || !exp.After(iat) {
		return time.Time{}
	}
	return iat.Add(exp.Sub(iat) * 8 / 10)
}

// ---------------------------------------------------------------------------
// The process's sign-ins
// ---------------------------------------------------------------------------

var (
	vendorSignInsMu sync.Mutex
	vendorSignIns   = map[string]*vendorSignIn{}

	// vendorSignInProcessStarted is close enough to the process's start for
	// the one question it answers: was this identity token issued before this
	// process existed.
	vendorSignInProcessStarted = time.Now()
)

// processVendorSignIn returns the process's sign-in for cfg.Vendor.
//
// The same fingerprint returns the same object, bearer and all: that is what
// makes N clients one sign-in, and what carries a sign-in across a provider
// reload. A different fingerprint replaces it and drops the old bearer, which
// was minted for other ids. On a replay-protected vendor reading the same
// token file, the new sign-in also remembers a token the old one had accepted,
// so it is not presented again under the new ids.
func processVendorSignIn(cfg vendorSignInConfig) *vendorSignIn {
	vendorSignInsMu.Lock()
	defer vendorSignInsMu.Unlock()

	current := vendorSignIns[cfg.Vendor]
	if current != nil && current.cfg.Fingerprint == cfg.Fingerprint {
		return current
	}
	next := newVendorSignIn(cfg, time.Now, vendorSignInProcessStarted)
	if current != nil && cfg.ReplayProtected && current.cfg.TokenFile == cfg.TokenFile {
		if hash, at, ok := current.acceptedPresentation(); ok {
			next.adoptAccepted(hash, at)
		}
	}
	vendorSignIns[cfg.Vendor] = next
	return next
}

// currentVendorSignIn returns the process's sign-in for a vendor, if one has
// been configured.
func currentVendorSignIn(vendor string) (*vendorSignIn, bool) {
	vendorSignInsMu.Lock()
	defer vendorSignInsMu.Unlock()
	s, ok := vendorSignIns[vendor]
	return s, ok && s != nil
}

// ---------------------------------------------------------------------------
// The status file
// ---------------------------------------------------------------------------

// vendorSignInStatusPath is where a vendor's record is kept for the check
// command, which runs as a second process in the same container.
func vendorSignInStatusPath(vendor string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, strings.ToLower(vendor))
	return filepath.Join(os.TempDir(), "memql-vendor-signin-"+clean+".json")
}

var vendorSignInStatusWarned sync.Once

// vendorSignInStatusFile says whether this process writes status files. Off by
// default and switched on by the RUNNING ENGINE only: the check command loads
// the same providers in a second process, and so does every test, and neither
// may overwrite the record of the engine whose sign-in it is.
var vendorSignInStatusFile atomic.Bool

// enableVendorSignInStatusFile makes this process the one that records its
// sign-ins where `memql provider-auth check` reads them.
func enableVendorSignInStatusFile() { vendorSignInStatusFile.Store(true) }

// writeVendorSignInStatus replaces the status file with rec. A failure is
// logged once per process and changes nothing else: the file is how a person
// reads the sign-in, never part of signing in.
func writeVendorSignInStatus(rec vendorSignInRecord) {
	if !vendorSignInStatusFile.Load() {
		return
	}
	path := vendorSignInStatusPath(rec.Vendor)
	err := func() error {
		raw, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".memql-vendor-signin-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(raw); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		if err := os.Chmod(tmpName, 0o600); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		return nil
	}()
	if err != nil {
		vendorSignInStatusWarned.Do(func() {
			slog.Warn("vendor sign-in: the status file could not be written; signing in is unaffected, "+
				"but `memql provider-auth check` will report nothing recorded",
				"path", path, "err", err)
		})
	}
}

// recordVendorWithoutSignIn writes the record of a vendor this engine holds no
// sign-in for: nobody set it up, or this node type signs in to none. It
// replaces whatever an earlier configuration left in the file, so a vendor
// whose ids were removed no longer reads as its last accepted sign-in.
func recordVendorWithoutSignIn(vendor, outcome, detail string) {
	writeVendorSignInStatus(vendorSignInRecord{
		Vendor:  vendor,
		Outcome: outcome,
		At:      time.Now(),
		Detail:  detail,
		Pid:     os.Getpid(),
	})
}

// readVendorSignInStatus reads a vendor's record back. ok is false when no
// record has been written.
func readVendorSignInStatus(vendor string) (vendorSignInRecord, bool, error) {
	raw, err := os.ReadFile(vendorSignInStatusPath(vendor))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vendorSignInRecord{}, false, nil
		}
		return vendorSignInRecord{}, false, err
	}
	var rec vendorSignInRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return vendorSignInRecord{}, false, fmt.Errorf("the status file at %s is not a sign-in record: %w",
			vendorSignInStatusPath(vendor), err)
	}
	return rec, true, nil
}
