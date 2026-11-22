package vendorkeys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// builtins.go -- the acts a person reaches an API key through (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K4, K5, K7, K11, K12 and K16, safety rules S5 to S7 and S9):
//
//	integration.vendorkeys.add       add a key           (dsl builtin vendorKeyAdd)
//	integration.vendorkeys.forActor  the keys I can see  (dsl builtin vendorKeysForActor)
//	integration.vendorkeys.update    its owner's settings (dsl builtin vendorKeyUpdate)
//	integration.vendorkeys.check     check it now        (dsl builtin vendorKeyCheck)
//	integration.vendorkeys.remove    remove it           (dsl builtin vendorKeyRemove)
//
// # Who may do what is decided HERE
//
// The rows are readable by no client and every query beneath is @serverOnly, so
// the store answers for anybody who reaches it (store.go). Each handler
// therefore establishes who is asking before it reads or writes a row:
//
//   - ADD: the part (`execute app:fleet/keys`, enforced by the dispatch from the
//     builtin's own annotation), and the person's own session.
//   - UPDATE: the key's owner, in their own session. Nobody else may rename a
//     key, make it the default or switch Auto on, a cluster owner included:
//     that would be deciding how somebody else's money is spent.
//   - CHECK and REMOVE: the key's owner, or a cluster owner, who must be able
//     to stop a credential on their cluster. The owner removes in their own
//     session.
//   - LIST: anybody signed in. Their own keys need the part; the short cards of
//     keys lent to them need nothing; every key on the cluster is a cluster
//     owner's to ask for.
//
// A CALLER WITH NO TIE TO A KEY IS TOLD WHAT AN UNKNOWN ID IS TOLD (S9). Every
// act answers one sentence for a key that does not exist, a key that was
// removed and a key that is somebody else's, so nothing here says which keys
// exist.
//
// # The person's own session
//
// A part says which rank may act. It does not say who is at the keyboard. A key
// spends its owner's money, so adding one and changing one is a person's
// decision: auth.RequirePersonSession refuses a personal access token, a badge,
// delegated access, every machine class, borrowed authority, the cluster
// acting, and any call made inside a run. A run is refused for a second reason:
// its journal keeps what a step was handed, and a step that adds a key was
// handed the key.
//
// # One owner's keys change one at a time
//
// A label must be free among a person's active keys, a person holds at most
// ten active keys per vendor, and exactly one of them is the default. Each is a
// decision over SEVERAL rows, so every act that depends on one runs under a
// write gate keyed on the owner, after a fresh read made while holding it. Two
// adds from two browsers cannot both be "the first key".
//
// # A refusal is a sentence
//
// The OS shows what a builtin refuses with, as written. A refusal here opens
// with a capital, says what is wrong and what to do, and never repeats a key.

const (
	// resultConcept is the synthetic node these capabilities return: an
	// in-flight result, never persisted.
	resultConcept = "integration:vendorkeys:result"

	// maxKeysPerVendor is how many active keys one person may hold at a vendor.
	maxKeysPerVendor = 10
	// maxLabelLength is counted in characters a person sees.
	maxLabelLength = 60

	// ownerGatePrefix keys the write gate of one person's keys.
	ownerGatePrefix = "vendorKeys:"

	// useOnlyNote is what a check says about a key that may not list models.
	useOnlyNote = "This key cannot list models, so it is checked when it is used."
)

// errNoSuchKey is the one answer for a key a caller cannot act on: an id that
// was never minted, a removed key, somebody else's key.
func errNoSuchKey() error {
	return refuse("No API key you can change has that id. Reload the list and choose one of yours.")
}

// refusal is a failure worded for the person who caused it.
type refusal struct {
	sentence string
	cause    error
}

func (r *refusal) Error() string { return r.sentence }
func (r *refusal) Unwrap() error { return r.cause }

func refuse(format string, args ...any) error {
	return &refusal{sentence: fmt.Sprintf(format, args...)}
}

// caller is the person behind an act.
type caller struct {
	userId       string // bare
	email, role  string
	clusterOwner bool
}

// callerOf is the signed-in person calling, and refuses everybody who is
// nobody: no access context, an anonymous reader, a connector, the cluster
// acting on its own and the operator credential.
func callerOf(ctx context.Context) (caller, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.IsAnonymousActor() || ac.IsConnector() || auth.ActsForNoPerson(ctx, ac.UserId) {
		return caller{}, refuse("API keys belong to people. Sign in as yourself and try again.")
	}
	userId := memql.BareShortId(strings.TrimSpace(ac.UserId))
	if userId == "" {
		return caller{}, refuse("API keys belong to people. Sign in as yourself and try again.")
	}
	return caller{userId: userId, email: ac.PrimaryEmail, role: string(ac.Role), clusterOwner: ac.IsClusterOwner()}, nil
}

// holdsKeysPart reports whether the caller may open their own keys: a holder
// of `read` on the part, or of `execute`, since a person who may add a key may
// see the keys they added.
func holdsKeysPart(ctx context.Context) bool {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok {
		return false
	}
	return auth.CapableFor(ctx, subject, auth.VerbRead, keysPart) || auth.CapableFor(ctx, subject, auth.VerbExecute, keysPart)
}

// keysPart is the Fleet part that gates a person's own keys (K4).
const keysPart = "app:fleet/keys"

// keyInARun is why a run does not add or change a key.
const keyInARun = "An API key is added and changed only by a person in Fleet. A run's journal keeps what a step was handed, and a key must not be kept there."

func refuseInARun(ctx context.Context) error {
	if _, inRun := common.RunFromContext(ctx); inRun {
		return refuse("%s", keyInARun)
	}
	return nil
}

// handleAdd checks, seals and stores a new key for the caller.
func (i *Integration) handleAdd(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx); err != nil {
		return nil, err
	}
	userId, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	me, err := callerOf(ctx)
	if err != nil || me.userId != memql.BareShortId(userId) {
		return nil, refuse("API keys belong to people. Sign in as yourself and try again.")
	}

	vendor := strings.ToLower(strings.TrimSpace(asString(args["vendor"])))
	label := strings.TrimSpace(asString(args["label"]))
	// The value is trimmed, checked for its shape and never logged. A key that
	// failed because of a trailing newline is a real support case; a key in a
	// log line is a worse one.
	key := strings.TrimSpace(asString(args["key"]))
	if vendor != memql.VendorAnthropic && vendor != memql.VendorOpenAI {
		return nil, refuse("Choose Anthropic or OpenAI.")
	}
	if err := checkLabel(label); err != nil {
		return nil, err
	}
	if sentence := memql.VendorKeyShapeRefusal(vendor, key); sentence != "" {
		return nil, refuse("%s", sentence)
	}

	// The cheap refusals first, so the vendor is not asked about a key that
	// would be refused anyway. They are asked again under the gate, where they
	// are decided.
	mine, err := i.store.ActiveKeysOfOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("add", "", err)
	}
	if err := roomFor(mine, vendor, label, ""); err != nil {
		return nil, err
	}

	// K11: ask the vendor before anything is saved.
	probe := i.probe(ctx, vendor, key)
	name := vendorName(vendor)
	checkedBy := CheckedByList
	switch probe.Verdict {
	case memql.VendorKeyProbeGood:
	case memql.VendorKeyProbeUseOnly:
		checkedBy = CheckedByUse
	case memql.VendorKeyProbeRefused:
		return nil, refuse("%s does not accept that key, so it was not saved. %s said: %s", name, name, saidOr(probe.Detail, "the key is not valid"))
	case memql.VendorKeyProbeCredit:
		return nil, refuse("%s says that key's account is out of credit or at a spend limit, so it was not saved. Add credit there and try again. %s said: %s", name, name, saidOr(probe.Detail, "the account cannot pay"))
	default:
		return nil, refuse("The key was not saved, because it could not be checked. %s", saidOr(probe.Detail, name+" could not be reached. Try again in a moment."))
	}

	sealed, fingerprint, err := secret.Encrypt(key)
	if err != nil {
		// secret.Encrypt's errors name the master key, never the value.
		i.logger.Error("vendorkeys: an API key could not be sealed", "error", err)
		return nil, refuse("The key was not saved, because this cluster cannot seal a secret right now. A cluster owner can find out why in the logs.")
	}

	release, err := i.lockOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("add", "", err)
	}
	defer release()
	mine, err = i.store.ActiveKeysOfOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("add", "", err)
	}
	if err := roomFor(mine, vendor, label, ""); err != nil {
		return nil, err
	}
	created := NewKey{
		Id: id.NewShortId(), Vendor: vendor, Label: label, Fingerprint: fingerprint,
		CheckedBy: checkedBy, IsDefault: len(atVendorOf(mine, vendor)) == 0, Sealed: sealedValue(sealed),
		CheckedAt: probe.At,
	}
	if err := i.store.Create(ctx, me.userId, created); err != nil {
		return nil, i.failed("add", created.Id, err)
	}
	i.audit(ctx, me, auditAdded, created.Id, map[string]any{
		"vendor": vendor, "checkedBy": checkedBy, "isDefault": created.IsDefault, "ownerUserId": me.userId,
	})
	return i.cardReply(ctx, me, created.Id, "")
}

// handleUpdate changes the owner's settings on one of their keys.
func (i *Integration) handleUpdate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx); err != nil {
		return nil, err
	}
	userId, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	me, err := callerOf(ctx)
	if err != nil || me.userId != memql.BareShortId(userId) {
		return nil, refuse("API keys belong to people. Sign in as yourself and try again.")
	}
	keyId := memql.BareShortId(asString(args["keyId"]))

	var change Settings
	if raw, given := args["label"]; given && raw != nil {
		label := strings.TrimSpace(asString(raw))
		if err := checkLabel(label); err != nil {
			return nil, err
		}
		change.Label = &label
	}
	if v, given := boolArg(args, "isDefault"); given {
		change.IsDefault = &v
	}
	if v, given := boolArg(args, "autoUse"); given {
		change.AutoUse = &v
	}
	if change.Label == nil && change.IsDefault == nil && change.AutoUse == nil {
		return nil, refuse("Say what to change: the key's name, whether it is your default, or whether Auto may use it.")
	}

	release, err := i.lockOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("update", keyId, err)
	}
	defer release()
	mine, err := i.store.ActiveKeysOfOwner(ctx, me.userId)
	if err != nil {
		return nil, i.failed("update", keyId, err)
	}
	key, ok := find(mine, keyId)
	if !ok {
		// Not the caller's, removed, or never minted: one answer.
		return nil, errNoSuchKey()
	}

	if change.Label != nil {
		if strings.EqualFold(*change.Label, key.Label) && *change.Label == key.Label {
			change.Label = nil
		} else if err := roomFor(mine, "", *change.Label, key.Id); err != nil {
			return nil, err
		}
	}
	var lostDefault []Key
	if change.IsDefault != nil {
		switch {
		case *change.IsDefault == key.IsDefault:
			change.IsDefault = nil
		case !*change.IsDefault:
			// Exactly one key is the default at a vendor. It is moved by
			// choosing another, never switched off.
			if len(atVendorOf(mine, key.Vendor)) > 1 {
				return nil, refuse("One of your %s keys is always your default. Choose the key that should be the default instead.", vendorName(key.Vendor))
			}
			return nil, refuse("Your only %s key is your default there.", vendorName(key.Vendor))
		default:
			for _, other := range atVendorOf(mine, key.Vendor) {
				if other.Id != key.Id && other.IsDefault {
					lostDefault = append(lostDefault, other)
				}
			}
		}
	}
	if change.AutoUse != nil && *change.AutoUse == key.AutoUse {
		change.AutoUse = nil
	}

	// The key that becomes the default is written FIRST. A failure between the
	// two writes then leaves two defaults, of which the resolver takes the
	// earlier, rather than none, which would stop the person's own key paying
	// with nothing to say why.
	if err := i.store.UpdateSettings(ctx, key.Id, change); err != nil {
		return nil, i.failed("update", key.Id, err)
	}
	off := false
	for _, other := range lostDefault {
		if err := i.store.UpdateSettings(ctx, other.Id, Settings{IsDefault: &off}); err != nil {
			return nil, i.failed("update", other.Id, err)
		}
		i.audit(ctx, me, auditDefaultChanged, other.Id, map[string]any{"vendor": other.Vendor, "isDefault": false, "ownerUserId": me.userId})
	}
	if change.Label != nil {
		i.audit(ctx, me, auditRenamed, key.Id, map[string]any{"vendor": key.Vendor, "ownerUserId": me.userId})
	}
	if change.AutoUse != nil {
		i.audit(ctx, me, auditAutoChanged, key.Id, map[string]any{"vendor": key.Vendor, "autoUse": *change.AutoUse, "ownerUserId": me.userId})
	}
	if change.IsDefault != nil {
		i.audit(ctx, me, auditDefaultChanged, key.Id, map[string]any{"vendor": key.Vendor, "isDefault": true, "ownerUserId": me.userId})
	}
	return i.cardReply(ctx, me, key.Id, "")
}

// handleRemove ends a key, for its owner or for a cluster owner.
func (i *Integration) handleRemove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx); err != nil {
		return nil, err
	}
	me, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	key, err := i.keyFor(ctx, me, asString(args["keyId"]))
	if err != nil {
		return nil, err
	}
	if key.OwnerUserId == me.userId {
		// Removing one's own key is a person's decision, like every other
		// change to it. A cluster owner stopping somebody else's key is the
		// one write a session is not asked of: it only ever narrows what can
		// be spent, and an incident is the wrong time to require a browser.
		if _, err := auth.RequirePersonSession(ctx); err != nil {
			return nil, err
		}
	}

	release, err := i.lockOwner(ctx, key.OwnerUserId)
	if err != nil {
		return nil, i.failed("remove", key.Id, err)
	}
	defer release()
	theirs, err := i.store.ActiveKeysOfOwner(ctx, key.OwnerUserId)
	if err != nil {
		return nil, i.failed("remove", key.Id, err)
	}
	current, ok := find(theirs, key.Id)
	if !ok {
		// Removed since it was read: removal is final and is not done twice.
		return nil, errNoSuchKey()
	}
	if err := i.store.Remove(ctx, current.Id, me.userId); err != nil {
		return nil, i.failed("remove", current.Id, err)
	}
	i.audit(ctx, me, auditRemoved, current.Id, map[string]any{
		"vendor": current.Vendor, "ownerUserId": current.OwnerUserId, "removedBy": me.userId,
	})
	if current.IsDefault {
		// The earliest of the owner's other keys at the vendor takes the
		// default, so their own calls keep an account to pay with.
		for _, other := range atVendorOf(theirs, current.Vendor) {
			if other.Id == current.Id {
				continue
			}
			on := true
			if err := i.store.UpdateSettings(ctx, other.Id, Settings{IsDefault: &on}); err != nil {
				i.logger.Warn("vendorkeys: a removed default was not replaced", "keyId", other.Id, "reason", failureReason(err))
				break
			}
			i.audit(ctx, me, auditDefaultChanged, other.Id, map[string]any{"vendor": other.Vendor, "isDefault": true, "ownerUserId": other.OwnerUserId})
			break
		}
	}
	return i.cardReply(ctx, me, current.Id, "")
}

// handleCheck lists the vendor's models with a key and records the answer.
func (i *Integration) handleCheck(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	me, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	key, err := i.keyFor(ctx, me, asString(args["keyId"]))
	if err != nil {
		return nil, err
	}
	result, err := i.check(ctx, key)
	if err != nil {
		return nil, err
	}
	return i.cardReply(ctx, me, key.Id, result.note)
}

// checkResult is what one check of a key came to.
type checkResult struct {
	// verdict is the vendor's answer, a memql.VendorKeyProbe constant. Only
	// good, refused and credit are verdicts on the key and were recorded.
	verdict string
	// note is a sentence for a person when the check decided nothing.
	note string
}

// check asks the vendor about one key and records what it said. It is the one
// check: a person's Check now and the daily pass both come through here.
func (i *Integration) check(ctx context.Context, key Key) (checkResult, error) {
	if key.CheckedBy == CheckedByUse {
		return checkResult{verdict: memql.VendorKeyProbeUseOnly, note: useOnlyNote}, nil
	}
	plain, err := i.resolver.Secret(ctx, accountOf(key))
	if err != nil {
		return checkResult{}, refuse("This key could not be checked, because it could not be unsealed on this node. A cluster owner can find out why in the logs.")
	}
	probe := i.probe(ctx, key.Vendor, plain)
	result := checkResult{verdict: probe.Verdict}
	switch probe.Verdict {
	case memql.VendorKeyProbeGood:
		err = i.store.observe(ctx, key.Id, memql.VendorKeyGood, probe.At, "", true)
	case memql.VendorKeyProbeRefused:
		err = i.store.observe(ctx, key.Id, memql.VendorKeyRefused, probe.At, probe.Detail, true)
	case memql.VendorKeyProbeCredit:
		err = i.store.observe(ctx, key.Id, memql.VendorKeyCredit, probe.At, probe.Detail, true)
	case memql.VendorKeyProbeUseOnly:
		// The key was restricted since it was added. Nothing was learned
		// about whether it works.
		result.note = useOnlyNote
		return result, nil
	default:
		// Not a verdict on the key: what is known about it stands.
		result.verdict = memql.VendorKeyProbeUnreachable
		result.note = saidOr(probe.Detail, vendorName(key.Vendor)+" could not be reached to check the key. Try again in a moment.")
		return result, nil
	}
	if err != nil {
		return checkResult{}, i.failed("check", key.Id, err)
	}
	return result, nil
}

// handleForActor lists the keys the caller can see.
func (i *Integration) handleForActor(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	me, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	all, _ := boolArg(args, "all")
	judge := i.resolver.newJudge(ctx, me.userId, memql.VendorAccountQuery{ActingUserId: me.userId, Chosen: true})

	var everything []Key
	for _, vendor := range []string{memql.VendorAnthropic, memql.VendorOpenAI} {
		keys, err := i.store.ActiveKeysAtVendor(ctx, vendor)
		if err != nil {
			return nil, i.failed("list", "", err)
		}
		everything = append(everything, keys...)
	}
	mayOpenMine := holdsKeysPart(ctx)
	var mine, lent, every []Key
	paused := map[string]bool{}
	for _, key := range everything {
		tie := judge.tie(key)
		switch tie {
		case tieOwnDefault, tieOwnOther:
			if mayOpenMine {
				mine = append(mine, key)
			}
		case tieLent:
			lent = append(lent, key)
		case tieLentPaused:
			lent = append(lent, key)
			paused[key.Id] = true
		}
		if all && me.clusterOwner {
			every = append(every, key)
		}
	}

	ids := make([]string, 0, len(mine)+len(lent)+len(every))
	seen := map[string]bool{}
	for _, group := range [][]Key{mine, lent, every} {
		for _, key := range group {
			if !seen[key.Id] {
				seen[key.Id] = true
				ids = append(ids, key.Id)
			}
		}
	}
	states, err := i.store.States(ctx, ids)
	if err != nil {
		return nil, i.failed("list", "", err)
	}
	names := i.ownerNames(ctx, mine, lent, every)

	payload := map[string]any{"mine": []any{}, "lent": []any{}, "all": []any{}}
	rows := func(keys []Key, render func(Key) map[string]any) []any {
		out := make([]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, render(key))
		}
		return out
	}
	now := i.store.now()
	payload["mine"] = rows(mine, func(k Key) map[string]any { return fullCard(k, states, names, relationMine, now) })
	payload["lent"] = rows(lent, func(k Key) map[string]any { return lentCard(k, states, names, paused[k.Id]) })
	payload["all"] = rows(every, func(k Key) map[string]any { return fullCard(k, states, names, relationOf(judge.tie(k)), now) })
	return i.node("list:"+me.userId, payload)
}

// keyFor reads the key an act names, for a caller who may act on it: its owner
// or a cluster owner. Everything else is one answer.
func (i *Integration) keyFor(ctx context.Context, me caller, keyId string) (Key, error) {
	key, found, err := i.store.Key(ctx, keyId)
	if err != nil {
		return Key{}, i.failed("read", memql.BareShortId(keyId), err)
	}
	if !found || !key.Active() || (key.OwnerUserId != me.userId && !me.clusterOwner) {
		return Key{}, errNoSuchKey()
	}
	return key, nil
}

// cardReply reads a key again and answers its card for the caller.
func (i *Integration) cardReply(ctx context.Context, me caller, keyId, note string) ([]memorynodes.MemoryNode, error) {
	key, found, err := i.store.Key(ctx, keyId)
	if err != nil || !found {
		return nil, i.failed("read", keyId, errors.Join(err, errors.New("the key was not read back")))
	}
	states, err := i.store.States(ctx, []string{key.Id})
	if err != nil {
		return nil, i.failed("read", keyId, err)
	}
	relation := relationOther
	if key.OwnerUserId == me.userId {
		relation = relationMine
	}
	card := fullCard(key, states, i.ownerNames(ctx, []Key{key}), relation, i.store.now())
	if note != "" {
		card["note"] = note
	}
	return i.node("key:"+key.Id, card)
}

// ownerNames reads the display name of each key's owner, once per owner.
func (i *Integration) ownerNames(ctx context.Context, groups ...[]Key) map[string]string {
	names := map[string]string{}
	for _, keys := range groups {
		for _, key := range keys {
			if _, done := names[key.OwnerUserId]; done {
				continue
			}
			names[key.OwnerUserId] = i.store.UserName(ctx, key.OwnerUserId)
		}
	}
	return names
}

// lockOwner takes the gate over one person's keys.
func (i *Integration) lockOwner(ctx context.Context, owner string) (func(), error) {
	if i.store.gate == nil {
		return nil, errors.New("vendorkeys: no gate pool")
	}
	pool := i.store.gate()
	if pool == nil {
		return nil, errors.New("vendorkeys: no gate pool")
	}
	return memql.AcquireWriteGate(ctx, pool, ownerGatePrefix+owner)
}

// roomFor refuses a label already used by one of the person's active keys and
// a key past the limit at a vendor. except is the key being renamed; vendor is
// empty for a rename, which adds no key.
func roomFor(mine []Key, vendor, label, except string) error {
	for _, key := range mine {
		if key.Id != except && strings.EqualFold(strings.TrimSpace(key.Label), label) {
			return refuse("You already have a key named %q. Give this one another name.", key.Label)
		}
	}
	if vendor != "" && len(atVendorOf(mine, vendor)) >= maxKeysPerVendor {
		return refuse("You have %d active %s keys, which is the most one person can hold. Remove one first.", maxKeysPerVendor, vendorName(vendor))
	}
	return nil
}

func checkLabel(label string) error {
	if n := utf8.RuneCountInString(label); n < 1 || n > maxLabelLength {
		return refuse("Give the key a name of 1 to %d characters.", maxLabelLength)
	}
	return nil
}

// atVendorOf is a person's active keys at one vendor, in the order they were
// added.
func atVendorOf(keys []Key, vendor string) []Key {
	var out []Key
	for _, key := range keys {
		if key.Vendor == vendor {
			out = append(out, key)
		}
	}
	return out
}

func find(keys []Key, keyId string) (Key, bool) {
	for _, key := range keys {
		if key.Id == keyId && keyId != "" {
			return key, true
		}
	}
	return Key{}, false
}

func saidOr(said, otherwise string) string {
	if strings.TrimSpace(said) == "" {
		return otherwise
	}
	return said
}

// failed is what a handler returns for whatever stopped an act that the person
// cannot act on beyond trying again. A refusal is already worded.
func (i *Integration) failed(act, keyId string, err error) error {
	var worded *refusal
	if errors.As(err, &worded) {
		return err
	}
	i.logger.Warn("vendorkeys: an act on an API key failed", "act", act, "keyId", keyId, "reason", failureReason(err))
	sentence := "That did not complete. Reload the list to see where it stands, and try again."
	if errors.Is(err, memql.ErrWriteGateBusy) {
		sentence = "Your keys are being changed from somewhere else at this moment. Try again."
	}
	return &refusal{sentence: sentence, cause: err}
}

// failureReason says why an act failed in words a log line can keep. NEVER the
// engine's own text: an engine error can repeat the statement it was given,
// and a create's statement holds the sealed value and the key's name.
func failureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.Is(err, memql.ErrWriteGateBusy):
		return "the owner's keys were locked by another act"
	default:
		return "the engine did not complete the statement"
	}
}

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("vendorkeys: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "vendorkeys:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// boolArg reads an optional boolean argument. given is false for a call that
// carried none; a value that is not a boolean is not given either, so a typo
// changes nothing rather than switching something off.
func boolArg(args map[string]any, name string) (value, given bool) {
	switch v := args[name].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}
