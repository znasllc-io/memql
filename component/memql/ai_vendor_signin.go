package memql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The one sign-in a process keeps per vendor (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions A1, A2, A4 and A5).
//
// THE INVARIANT: a process presents an identity token to a vendor at most once,
// however many clients it holds.
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
// WHAT IT WILL NOT DO, and each refusal is deliberate:
//
//   - Present an ACCEPTED token again to a replay-protected vendor. It cannot
//     succeed, and every attempt is a refusal in the vendor's console. The
//     engine waits for kubelet to rotate the file and says so.
//   - Retry a REFUSED token on its own. A refusal is a configuration answer
//     (the rule, the subject, the audience) that only a person changes. One
//     exception: AllowRetry, which Verify and Apply call, lets a refused token
//     be presented once more, because the operator may have just fixed the
//     vendor's side and should not wait out a rotation to find out.
//   - Drop a bearer that still works because a newer sign-in failed. The last
//     good bearer is handed out until it expires.
//
// WHAT SURVIVES WHAT. The sign-in is per PROCESS. It survives a provider
// reload while the federation ids are unchanged (processVendorSignIn returns
// the same object for the same fingerprint). It does not survive a container
// restart, and a restarted container reads the same token file: on a
// replay-protected vendor its first sign-in is then refused until the token
// rotates. That refusal is recorded with MayBeReplay, so it does not read as a
// broken configuration (A4).
//
// THE RECORD is what `memql provider-auth check` reports instead of signing
// in itself (A5): two signers in one pod cannot both succeed under a replay
// rule. It is written to a small file in the pod's temporary directory and
// carries no credential: not the bearer, not the identity token.
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
	// expiresAt means the vendor named no lifetime. A vendor's 4xx is returned
	// as *vendorExchangeRefused; anything else means no answer.
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
	// MayBeReplay marks a refusal of a token issued before this process
	// started: an earlier process in this pod may already have used it.
	MayBeReplay bool `json:"mayBeReplay,omitempty"`
	// NextTokenDue is when the identity token is expected to be replaced.
	NextTokenDue  time.Time         `json:"nextTokenDue,omitempty"`
	Fingerprint   string            `json:"fingerprint"`
	Ids           map[string]string `json:"ids,omitempty"`
	TokenSubject  string            `json:"tokenSubject,omitempty"`
	TokenAudience []string          `json:"tokenAudience,omitempty"`
	// ModelsListed is how many models the vendor listed with the new bearer,
	// the proof that it is accepted for API calls and not only well-formed.
	ModelsListed int `json:"modelsListed,omitempty"`
	Pid          int `json:"pid"`
}

// vendorSignIn is one vendor's sign-in for this process.
type vendorSignIn struct {
	cfg vendorSignInConfig
	now func() time.Time
	// started is when the PROCESS started, which is what decides whether a
	// refused token may have been used before it (MayBeReplay).
	started time.Time

	mu        sync.Mutex
	bearer    string
	expiresAt time.Time

	// The last identity token presented, by hash, and how the vendor answered.
	hasPresented     bool
	presented        [sha256.Size]byte
	presentedOutcome string
	presentedAt      time.Time
	// retryAllowed is set by AllowRetry and spent by the next presentation.
	retryAllowed bool
	// lastErr is what a caller gets while nothing may be presented.
	lastErr error

	hasRecord bool
	record    vendorSignInRecord
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
// THE LOCK IS HELD ACROSS THE EXCHANGE, ON PURPOSE. Many callers arrive
// together when a bearer nears expiry; releasing the lock would let each
// present the same token, which is the failure this type exists to prevent.
// The first caller pays and the rest wake to the result.
func (s *vendorSignIn) Bearer(ctx context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.bearer != "" && s.expiresAt.Sub(now) > vendorSignInRefreshWindow {
		return s.bearer, s.expiresAt, nil
	}

	// Re-read on every sign-in: kubelet rewrites a projected token in place,
	// and a copy taken earlier stops being valid on a schedule nothing here
	// controls. A failed read presents nothing and marks nothing as presented.
	token, err := readFileTrimmed(s.cfg.TokenFile)
	if err != nil {
		if s.bearerValid(now) {
			return s.bearer, s.expiresAt, nil
		}
		return "", time.Time{}, fmt.Errorf("cannot read the identity token at %s: %w", s.cfg.TokenFile, err)
	}
	hash := sha256.Sum256([]byte(token))

	if s.hasPresented && hash == s.presented {
		if hold, heldErr := s.holdSameToken(token, now); hold {
			if s.bearerValid(now) {
				return s.bearer, s.expiresAt, nil
			}
			return "", time.Time{}, heldErr
		}
	}
	return s.present(ctx, token, hash)
}

// holdSameToken decides whether the token on disk, which is the one last
// presented, must NOT be presented again right now. hold is false when it may.
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

// present makes one sign-in with the token and records what the vendor said.
// The caller holds s.mu.
func (s *vendorSignIn) present(ctx context.Context, token string, hash [sha256.Size]byte) (string, time.Time, error) {
	started := s.now()
	exchangeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vendorSignInExchangeTimeout)
	bearer, expiresAt, err := s.cfg.Exchange(exchangeCtx, token)
	cancel()

	s.hasPresented, s.presented, s.presentedAt, s.retryAllowed = true, hash, started, false

	var refused *vendorExchangeRefused
	switch {
	case err == nil:
		if expiresAt.IsZero() {
			expiresAt = started.Add(vendorSignInUnknownLifetime)
		}
		s.bearer, s.expiresAt = bearer, expiresAt
		s.presentedOutcome, s.lastErr = signInOutcomeOK, nil
		s.setRecord(vendorSignInRecord{Outcome: signInOutcomeOK, At: started, ExpiresAt: expiresAt}, token)
		return bearer, expiresAt, nil

	case errors.As(err, &refused):
		s.presentedOutcome = signInOutcomeDenied
		// The runbook is named because a refusal is a configuration answer, and
		// this error is what a failed model call carries to whoever reads it.
		s.lastErr = fmt.Errorf("%s sign-in: %w. Runbook: %s", s.cfg.Vendor, err, federationRunbookFor(s.cfg.Vendor))
		rec := vendorSignInRecord{
			Outcome: signInOutcomeDenied,
			Status:  refused.Status,
			At:      started,
			Detail:  refused.Body,
		}
		// A token issued before this process started may already have been
		// used by the process this one replaced. Say so, and say when the
		// next token is due, so a restart does not read as a broken rule.
		if identityTokenIssuedBefore(token, s.started) {
			rec.MayBeReplay = true
			rec.NextTokenDue = nextIdentityTokenDue(token)
		}
		s.setRecord(rec, token)

	default:
		s.presentedOutcome = signInOutcomeError
		s.lastErr = fmt.Errorf("%s sign-in did not complete: %w", s.cfg.Vendor, err)
		s.setRecord(vendorSignInRecord{
			Outcome: signInOutcomeError,
			At:      started,
			Detail:  truncateForLog(err.Error(), maxFederationBodyNote),
		}, token)
	}

	// THE LAST GOOD BEARER KEEPS WORKING. A refusal is an answer that arrives
	// while a valid bearer may still be in hand, and dropping it would turn a
	// misconfiguration an operator has time to fix into an immediate outage.
	// The clock is read again: the exchange may have spent seconds failing.
	if s.bearerValid(s.now()) {
		return s.bearer, s.expiresAt, nil
	}
	return "", time.Time{}, s.lastErr
}

func (s *vendorSignIn) bearerValid(now time.Time) bool {
	return s.bearer != "" && s.expiresAt.After(now)
}

// Invalidate drops a bearer the vendor answered an API call with 401 for. The
// next Bearer signs in if a token may be presented.
//
// It names the bearer that failed, and drops the one in hand only if it is that
// one: a call that started with the previous bearer must not throw away the
// one a newer sign-in has just bought.
func (s *vendorSignIn) Invalidate(bearer string) {
	s.mu.Lock()
	if bearer != "" && s.bearer == bearer {
		s.bearer, s.expiresAt = "", time.Time{}
	}
	s.mu.Unlock()
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

// NoteModelsListed records how many models the vendor listed with the bearer
// of an accepted sign-in.
func (s *vendorSignIn) NoteModelsListed(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasRecord || s.record.Outcome != signInOutcomeOK {
		return
	}
	s.record.ModelsListed = n
	writeVendorSignInStatus(s.record)
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
// accepted, if that is how its last presentation went.
func (s *vendorSignIn) acceptedPresentation() ([sha256.Size]byte, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasPresented || s.presentedOutcome != signInOutcomeOK {
		return [sha256.Size]byte{}, time.Time{}, false
	}
	return s.presented, s.presentedAt, true
}

// identityTokenTimes reads iat and exp out of a token, zero when absent.
func identityTokenTimes(token string) (iat, exp time.Time) {
	claims, err := parseJWTClaims(token)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	read := func(key string) time.Time {
		switch v := claims[key].(type) {
		case float64:
			return time.Unix(int64(v), 0).UTC()
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return time.Unix(n, 0).UTC()
			}
		}
		return time.Time{}
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
