package vendorkeys

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// notice.go -- telling a key's owner, once, that it stopped working (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K13, mesh rule M4).
//
// # Why there is one
//
// A key that stops working does not stop anybody's work. The call moves to the
// next account that can pay: a key somebody else lent, or the cluster's own.
// That is the point of the order, and it is also why a dead key is silent. Its
// owner finds out when they next open Fleet, and until then somebody else is
// paying for calls they thought were theirs. So the owner is mailed.
//
// # A pass, and not the node that saw the fall
//
// A fall is recorded by whichever replica made the call the key was paying
// for (observe.go): an agent or a planner as often as a bff, in the middle of
// somebody's request. Mail is slow, can fail and has to be tried again, and
// none of that belongs there. So recording a fall sends nothing. The fall is
// on the key's state row, and a pass reads the rows and does the telling:
// the engine's scheduled automation `vendorKeysOwnerNotices`, every ten
// minutes on one bff replica (dsl/router/automations.memql). Ten minutes is
// the longest an owner goes without knowing.
//
// # What is owed
//
// A key owes its owner a notice when it is ACTIVE, it is NOT PAYING (refused,
// or out of credit), and nobody has been told since it last worked. The last
// part is read off the state row: notifiedAt is not later than goodAt. Nothing
// else is stored, so there is no flag to fall out of step with the state. It
// follows that:
//
//   - a key refused on every call for a week owes one notice, and a key that
//     goes from refused to out of credit owes no second one;
//   - a key that works again and then stops again owes a second;
//   - a key that works again BEFORE its owner was told owes nothing, because
//     a mail saying it stopped would be false by the time it was read;
//   - a key its owner REMOVED owes nothing: they know.
//
// # Once per fall (M4)
//
// The automation's lease places the pass on one replica. A placement is not a
// guarantee: a lease changes hands, and two replicas can each believe they
// hold it for a moment. What makes it one notice is the key's own write gate,
// the same Postgres advisory lock every verdict is written under. Under it a
// pass reads the row again, and if a notice is still owed and nobody tried in
// the last noticeRetryEvery, records that it is trying (noticeTriedAt). Then
// it lets go of the gate, sends, and takes the gate again to record that the
// owner was told (notifiedAt). A second replica that reaches the key in
// between reads the first one's stamp and leaves the key alone.
//
// The gate is NOT held while the mail is sent. Sending can take seconds and
// takes gates of its own (the email integration's send receipts); a gate held
// across it would keep a connection of the node's small gate pool and stall
// every verdict about the key.
//
// # When the mail cannot be sent
//
// Nothing is recorded as told, so the notice is still owed, and noticeTriedAt
// keeps the next half hour of passes away from it. After that it is tried
// again, for as long as the key is still not paying and still active. The
// verdict itself never depended on the mail: the key was passed over from the
// moment the vendor refused it.
//
// The one way an owner is told twice: the mail went, and the write that
// records it failed. It is logged, and the notice goes again half an hour on.
// Told twice was chosen over possibly never.
//
// The same stamp means a key that falls again within half an hour of its last
// notice waits out the rest of that half hour.
//
// # Nobody to tell
//
// A key whose owner is no longer an active user, or has no address, is
// settled: recorded as told with nothing sent, so the pass does not ask about
// it every ten minutes for ever.
//
// # What it carries
//
// The key's name as its owner gave it, its vendor, which way it stopped
// working, and the vendor's own words with anything shaped like a key removed.
// Never the value, and never the fingerprint: the fingerprint is the end of
// the key. Whoever sends the notice decides its wording
// (component/identity/emailsender), and it carries no link.
//
// # Who runs it
//
// The engine's own automation and nobody else, by the mark the engine puts on
// its own automations: not a person, a cluster owner included, and not an
// automation of the same name from a mounted bundle. The notifier itself is
// installed by app/, which is where a mail sender can be reached; this
// package imports none.
//
// Regression tests: notice_db_test.go, and
// component/automations/vendor_keys_owner_notices_test.go for the schedule.

const (
	// noticeAutomation is the one automation the pass answers to.
	noticeAutomation = "vendorKeysOwnerNotices"

	// noticeRetryEvery is how long a notice that somebody started sending is
	// left alone: by another replica, and by the next passes when it failed.
	noticeRetryEvery = 30 * time.Minute

	// noticeSendTimeout bounds handing one notice to the notifier.
	noticeSendTimeout = 20 * time.Second

	// defaultNoticeLimit is how many owners one pass tells at most.
	defaultNoticeLimit = 50

	// noticePassBudget bounds one pass, at half the time between two, so a
	// pass with a slow mail server behind it has ended before the next begins.
	noticePassBudget = 5 * time.Minute
)

// noticeRefusal is what anybody but the pass is told.
const noticeRefusal = "The owner of an API key that stops working is told by the cluster. Nobody sends that notice by hand."

// KeyNotice is what a key's owner is told when it stops working.
type KeyNotice struct {
	KeyId string
	// OwnerUserId is the bare id of the person the key belongs to, and
	// OwnerEmail and OwnerName are theirs as the cluster holds them now.
	OwnerUserId, OwnerEmail, OwnerName string
	// Label is the owner's own name for the key.
	Label string
	// Vendor is memql.VendorAnthropic or memql.VendorOpenAI.
	Vendor string
	// State is StateRefused or StateCredit.
	State string
	// Detail is the vendor's words, bounded and with key-shaped text removed.
	Detail string
	// At is the time of the evidence: when the call or check that last found
	// the key not working started.
	At time.Time
}

// Notifier delivers one notice to a key's owner. An error leaves the notice
// owed, to be tried again after noticeRetryEvery.
type Notifier func(ctx context.Context, notice KeyNotice) error

// notifierHolder is the installed notifier, behind an atomic so the pass on
// one goroutine and app/ installing it on another do not race.
type notifierHolder struct{ atomic.Pointer[Notifier] }

// SetNotifier installs what tells a key's owner. Nil removes it. With none
// installed a pass sends nothing and records nothing, so the notices are
// still owed when a node that can send runs it.
func (s *Store) SetNotifier(n Notifier) {
	if s == nil {
		return
	}
	if n == nil {
		s.notify.Store(nil)
		return
	}
	s.notify.Store(&n)
}

// SetNotifier installs what tells a key's owner (Store.SetNotifier).
func (i *Integration) SetNotifier(n Notifier) {
	if i != nil {
		i.store.SetNotifier(n)
	}
}

func (s *Store) notifier() Notifier {
	if held := s.notify.Load(); held != nil {
		return *held
	}
	return nil
}

// owesNotice reports whether a key's state owes its owner a notice: it is not
// paying, and nobody has been told since it last worked.
func owesNotice(state State) bool {
	notPaying := state.State == StateRefused || state.State == StateCredit
	return notPaying && !state.NotifiedAt.After(state.GoodAt)
}

// noticeCounts is what one pass did.
type noticeCounts struct {
	// owed is how many active keys owed a notice when the pass began.
	owed int
	// sent is how many owners were told. failed is how many could not be, and
	// are tried again after noticeRetryEvery. waiting is how many were left
	// for a later pass: tried too recently, past this pass's limit or its
	// time, or on a node that cannot coordinate or cannot send. nobody is how
	// many had no owner to tell and were settled.
	sent, failed, waiting, nobody int
}

// handleTellOwners runs the pass. See the file comment for who may.
func (i *Integration) handleTellOwners(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if !auth.IsEngineAutomationActor(ac, noticeAutomation) {
		return nil, refuse("%s", noticeRefusal)
	}
	counts, err := i.store.tellOwners(ctx, 0)
	if err != nil {
		return nil, i.failed("tellOwners", "", err)
	}
	if counts.owed > 0 {
		i.logger.Info("vendorkeys: the owners of API keys that stopped working were told",
			"owed", counts.owed, "sent", counts.sent, "failed", counts.failed,
			"waiting", counts.waiting, "nobody", counts.nobody)
	}
	return i.node("tellOwners", map[string]any{
		"owed": counts.owed, "sent": counts.sent, "failed": counts.failed,
		"waiting": counts.waiting, "nobody": counts.nobody,
	})
}

// tellOwners tells the owner of every key that owes a notice, the
// longest-standing first, at most limit of them (zero is the default). An
// error is a failure to READ which keys owe one: nobody was told.
func (s *Store) tellOwners(ctx context.Context, limit int) (noticeCounts, error) {
	var counts noticeCounts
	owed, err := s.owedNotices(ctx)
	if err != nil {
		return counts, err
	}
	counts.owed = len(owed)
	notify := s.notifier()
	if notify == nil {
		counts.waiting = len(owed)
		return counts, nil
	}
	if limit <= 0 {
		limit = defaultNoticeLimit
	}
	ctx, cancel := context.WithTimeout(ctx, noticePassBudget)
	defer cancel()
	for n, state := range owed {
		if n >= limit || ctx.Err() != nil {
			counts.waiting += len(owed) - n
			break
		}
		switch s.tellOwner(ctx, notify, state.KeyId) {
		case noticeSent:
			counts.sent++
		case noticeFailed:
			counts.failed++
		case noticeWaiting:
			counts.waiting++
		case noticeNobody:
			counts.nobody++
		}
	}
	return counts, nil
}

// owedNotices reads the state of every active key that owes a notice, the
// longest-standing evidence first (ties by id, so two replicas would agree).
// A removed key is not read at all.
func (s *Store) owedNotices(ctx context.Context) ([]State, error) {
	var owed []State
	for _, vendor := range []string{memql.VendorAnthropic, memql.VendorOpenAI} {
		keys, err := s.ActiveKeysAtVendor(ctx, vendor)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			ids = append(ids, key.Id)
		}
		states, err := s.States(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if state, found := states[id]; found && owesNotice(state) {
				owed = append(owed, state)
			}
		}
	}
	sort.SliceStable(owed, func(a, b int) bool {
		if !owed[a].EvidenceAt.Equal(owed[b].EvidenceAt) {
			return owed[a].EvidenceAt.Before(owed[b].EvidenceAt)
		}
		return owed[a].KeyId < owed[b].KeyId
	})
	return owed, nil
}

// noticeOutcome is what became of one key's notice in one pass.
type noticeOutcome int

const (
	// noticeNotOwed: by the time the key's gate was held, nothing was owed.
	// The key works again, or another replica told its owner.
	noticeNotOwed noticeOutcome = iota
	noticeClaimed
	noticeSent
	noticeFailed
	noticeWaiting
	noticeNobody
)

// tellOwner tells one key's owner, if a notice is still owed and nobody else
// is telling them.
func (s *Store) tellOwner(ctx context.Context, notify Notifier, keyId string) noticeOutcome {
	fall, outcome := s.claimNotice(ctx, keyId)
	if outcome != noticeClaimed {
		return outcome
	}
	key, found, err := s.Key(ctx, keyId)
	if err != nil {
		s.logger.Warn("vendorkeys: an API key stopped working and its owner was not told: the key could not be read",
			"keyId", keyId, "reason", failureReason(err))
		return noticeFailed
	}
	name, email, active := "", "", false
	if found && key.Active() {
		if name, email, active, err = s.owner(ctx, key.OwnerUserId); err != nil {
			s.logger.Warn("vendorkeys: an API key stopped working and its owner was not told: the owner could not be read",
				"keyId", keyId, "reason", failureReason(err))
			return noticeFailed
		}
	}
	if !active || email == "" {
		// Removed since the pass listed it, or its owner is no longer a user
		// here, or has no address. Settled, so it is not asked about again.
		if err := s.settleNotice(ctx, keyId, fall.EvidenceAt); err != nil {
			s.logger.Warn("vendorkeys: an API key with nobody to tell could not be settled; the next pass asks again",
				"keyId", keyId, "reason", failureReason(err))
		}
		return noticeNobody
	}

	sendCtx, cancel := context.WithTimeout(ctx, noticeSendTimeout)
	err = notify(sendCtx, KeyNotice{
		KeyId: key.Id, OwnerUserId: key.OwnerUserId, OwnerEmail: email, OwnerName: name,
		Label: key.Label, Vendor: key.Vendor, State: fall.State, Detail: bounded(fall.Detail), At: fall.EvidenceAt,
	})
	cancel()
	if err != nil {
		// The reason is a mail system's and may name an address; the sender
		// logs its own. The key's id is enough to find the rest.
		s.logger.Warn("vendorkeys: an API key stopped working and the notice to its owner was not sent; it is tried again later",
			"keyId", keyId, "state", fall.State, "retryAfter", noticeRetryEvery.String())
		return noticeFailed
	}
	if err := s.settleNotice(ctx, keyId, fall.EvidenceAt); err != nil {
		s.logger.Warn("vendorkeys: the owner of an API key was told it stopped working, and that could not be recorded; they may be told again",
			"keyId", keyId, "reason", failureReason(err), "retryAfter", noticeRetryEvery.String())
	}
	return noticeSent
}

// claimNotice records, under the key's state gate, that this pass is telling
// the key's owner, and returns the state it read there. Anything but
// noticeClaimed means this pass sends nothing for the key.
func (s *Store) claimNotice(ctx context.Context, keyId string) (State, noticeOutcome) {
	release, ok, err := s.lockState(ctx, keyId)
	if err != nil {
		if !errors.Is(err, memql.ErrWriteGateBusy) {
			s.logger.Warn("vendorkeys: the gate of an API key's state could not be taken to tell its owner",
				"keyId", keyId, "reason", failureReason(err))
		}
		return State{}, noticeWaiting
	}
	if !ok {
		// No gate pool: this node cannot coordinate with the others, and two
		// that cannot would each tell the owner.
		return State{}, noticeWaiting
	}
	defer release()

	state, found, err := s.state(ctx, keyId)
	if err != nil {
		s.logger.Warn("vendorkeys: an API key's state could not be read to tell its owner", "keyId", keyId, "reason", failureReason(err))
		return State{}, noticeFailed
	}
	if !found || !owesNotice(state) {
		return State{}, noticeNotOwed
	}
	now := s.now().UTC()
	if !state.NoticeTriedAt.IsZero() && now.Sub(state.NoticeTriedAt) < noticeRetryEvery {
		return State{}, noticeWaiting
	}
	if err := s.writeState(ctx, keyId, map[string]any{"noticeTriedAt": now.Format(time.RFC3339Nano)}); err != nil {
		s.logger.Warn("vendorkeys: that an API key's owner is being told could not be recorded, so nothing was sent",
			"keyId", keyId, "reason", failureReason(err))
		return State{}, noticeFailed
	}
	return state, noticeClaimed
}

// settleNotice records that the owner was told about the fall whose evidence
// is dated `fall`, or that there was nobody to tell. The stamp is the
// evidence's time and not the clock's: it is compared with goodAt, which is
// evidence time too, so a key that recovered while the mail was being sent
// still owes a notice the next time it falls.
func (s *Store) settleNotice(ctx context.Context, keyId string, fall time.Time) error {
	release, ok, err := s.lockState(ctx, keyId)
	if err != nil {
		return err
	}
	if !ok {
		return memql.ErrWriteGatePoolMissing
	}
	defer release()
	return s.writeState(ctx, keyId, map[string]any{"notifiedAt": fall.UTC().Format(time.RFC3339Nano)})
}
