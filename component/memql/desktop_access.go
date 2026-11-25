package memql

// What model access a person has set up: the foundation of the desktop-access
// hold (design record docs/superpowers/specs/2026-11-24-desktop-access-design.md,
// section 1).
//
// WHY THIS IS NOT inferenceStatus.eligible. inferenceStatus answers "can a
// call be answered right now". This answers "has the person set something
// up", and the difference is exactly what is asleep or temporarily empty: a
// lent laptop that is closed, an app out of usage, a key whose vendor account
// is out of credit all come back, and keeping a person out of MemQL OS while
// they wait would send them to a setup page for a thing already set up. The
// two share their predicates (meetsChatMinimum, the app row tests in
// app_selection.go and core/airoute, the resolver's reasons) and drop only the
// "is it awake" tests, so they cannot drift apart in anything else. One difference is deliberate: a
// machine is judged by what IT advertised for a model (chatMachineMeetsMinimum),
// as per-call eligibility judges the machine that would serve, and never by
// the model's union over every machine behind it, revoked ones included, which
// could let a machine below the floor count because a revoked machine
// qualified its model (final review, minor 3). What does NOT count: a model
// that only embeds, a model below the context floor, a revoked machine, a key
// that was refused or paused or whose owner left, an app measured signed out
// or whose last session was refused as signed out.
//
// ONE PLAIN REASON PER DISTINCT CAUSE, so the line under a held person's
// reference says what is missing and never less (design section 3, "each
// source checked with why it does not count"; the anti-goal "the log line
// says less than what is missing"). The sentences are the accessReason*
// values below and keyReason's; several can stand together. A reason names a
// lender's id only for something lent to this person, and never a key, a
// machine name or a vendor's words. Reasons are sorted, so one state always
// reads and digests the same (accessReasonsDigest), and a re-check that finds
// the same reasons can reuse the reference it already logged.
//
// THE HOLD (desktopAccess, below) puts it together:
//   - Exempt: the person's STORED role passes auth.MayManageFleet (owner or
//     developer, never admin, never an account-scoped role). The stored row is
//     read fresh, never the AccessContext's role, because work continued on a
//     person's behalf carries a stand-in role (RoleStandIn) that says nothing
//     about who they are. An account-scoped owner or developer (design section
//     2, "per-account roles arrive later") is not exempt: only the stored
//     cluster role is.
//   - Admitted: any source counts. Held: none does.
//   - A held answer carries a reference (ERR- and six hex digits, the shape the
//     logs already use) and the line that explains it is written BEFORE the
//     reference is returned, so a person reading it out to an administrator
//     can always be found in Logs. A failed write holds with NO reference
//     rather than a reference nobody can look up. A re-check passing back its
//     reference and an unchanged reasons digest writes nothing, so a page
//     checking every 30 seconds adds one line, not one per poll; changed
//     reasons write a new line under the SAME reference so the person keeps
//     one number. A passed-back value that is not a reference is treated as
//     absent and never written (a client must not be able to put its own text
//     into the log under this name). Any replica can answer a re-check: the
//     reference travels with the client, not in a replica's memory.
//   - AN ERROR IS NEVER A HOLD. A read that fails (the catalog, the read of
//     the machines lent to them, apps, keys, the stored role, a grant or
//     membership read) comes back as an error, including a failure a tolerant
//     reader answered around and noted on the sources' failure holder; the
//     client reads that as "unknown" and lets the person in. A person is held
//     only on an answer the engine is sure of.
//
// Tests: desktop_access_test.go (sources), desktop_access_hold_test.go (hold),
// desktop_access_db_test.go (real rows: apps, the stored role, a scoped role).
// Not verified here: the builtin that exposes it (task 3) and the recorder
// that writes the line to the log store (task 4); until one is installed every
// held answer carries no reference.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
)

// The kinds of source a person can have set up.
const (
	AccessSourceMachine        = "machine"
	AccessSourceApp            = "app"
	AccessSourceKey            = "key"
	AccessSourceClusterAccount = "clusterAccount"
)

// The states a source that counts can be in. Only "ready" can answer now;
// the others are set up and will answer again.
const (
	AccessReady       = "ready"
	AccessAsleep      = "asleep"
	AccessOutOfCredit = "outOfCredit"
	AccessOutOfUsage  = "outOfUsage"
)

// The hold line's reasons, one per distinct cause (final review, Important 3).
// Each is checked only once nothing of its kind counts, and each names no
// machine or key.
const (
	// No machine at all, theirs or lent. The catalog lists a machine only
	// under a model it offers, so a registered machine that offers no model
	// reads the same; "offers a model" keeps the sentence true for both.
	accessReasonNoMachine = "no machine of theirs or lent to them offers a model"
	// A revoked registration of theirs (a revoked lent machine names its
	// lender: accessReasonLentMachineRevoked).
	accessReasonMachineRevoked = "a machine of theirs is revoked"
	// Every model on their live machines and the ones lent to them only
	// makes embeddings.
	accessReasonEmbeddingsOnly = "the machines theirs or lent to them offer only embedding models"
	// No app on their own machines is allowed and signed in.
	accessReasonNoApp = "no app on their machines is allowed and signed in"
	// The key reads found no key at all, theirs or lent. A key that exists
	// and does not count says why in its own sentence (keyReason).
	accessReasonNoKey = "no API key of theirs or lent to them"
	// The cluster's account is closed to them: they lack the permission
	// (execute app:settings/clusteraccount), whether or not an account is set
	// up. Granting it is the administrator's fix.
	accessReasonNoClusterPermission = "may not use the cluster's account"
	// They hold the permission and the cluster has no account to pay with.
	accessReasonNoClusterAccount = "may use the cluster's account, but none is set up"
)

// accessReasonBelowFloor: chat models on their live machines, none of which
// has structured output and a context window of at least MinimumContextWindow
// on that machine.
var accessReasonBelowFloor = fmt.Sprintf("no model on the machines theirs or lent to them has structured output and a context of at least %d", MinimumContextWindow)

// accessReasonLentMachineRevoked is a revoked machine lent to them, naming its
// lender by bare id, the spelling the hold line uses for everybody.
func accessReasonLentMachineRevoked(lenderUserId string) string {
	return fmt.Sprintf("a machine lent by %s is revoked", BareShortId(lenderUserId))
}

// AccessSource is one model source a person has set up. LenderUserId is set
// only for something lent to them; Own is true for their own machines, apps
// and keys.
type AccessSource struct {
	Kind string `json:"kind"`
	// App is the app id (claude-code, codex) of an app source and empty for
	// any other kind, so a message can name the app.
	App          string `json:"app,omitempty"`
	State        string `json:"state"`
	LenderUserId string `json:"lenderUserId,omitempty"`
	Label        string `json:"label,omitempty"`
	Own          bool   `json:"own"`
}

// desktopAccessSources lists what actingUserId has set up and, for each source
// that does not count, one reason. ctx must be the person's own (a machine
// catalog and a key are read as them). A read that fails is an error and not
// an empty answer, because "we could not ask" must never be reported as "they
// have nothing set up".
func (e *MemQLEngine) desktopAccessSources(ctx context.Context, actingUserId string) (sources []AccessSource, reasons []string, err error) {
	if e == nil || e.providers == nil {
		// Nothing to ask is not "nothing set up": answering empty would hold
		// a person for a node that was never wired.
		return nil, nil, errors.New("desktop access: no provider registry on this engine")
	}
	actingUserId = strings.TrimSpace(actingUserId)
	add := func(s AccessSource) { sources = append(sources, s) }
	why := func(r string) {
		for _, have := range reasons {
			if have == r {
				return
			}
		}
		reasons = append(reasons, r)
	}

	// ONE FAILURE HOLDER FOR EVERY READ BELOW. The tolerant readers answer
	// from what they could read when a read fails, on purpose, because that
	// is the right answer for routing: the catalog keeps the person's own
	// machines when the read of everybody else's fails (fleetcatalog's
	// Reader.Catalog), a membership read that fails answers "in no groups"
	// (activeGroupIdsForUser), and a grant read that fails falls back to the
	// role's answer (grant_resolver.go, ruling R-D8). Here each would report
	// "could not read" as "nothing set up" and hold a person on a transient
	// error, the lent machine they rely on included. So every read runs under
	// one holder, each of those readers notes what it swallowed on it, and a
	// noted failure is an error: unknown, which lets the person in.
	//
	// OrganizationReadContext rather than auth.ContextWithReadFailures alone:
	// it also gives these reads a fresh account-scope memo, because the
	// person's memberships memoised earlier in the request, with nobody
	// listening, could rest on a failure this holder never heard. The cost is
	// at most one more membership read per check.
	//
	// Tests: TestAFailedReadOfLentMachinesIsAnErrorNotAHold,
	// TestAFailedMembershipReadInTheLentReadIsAnError,
	// TestAFailedGrantReadBehindTheClusterAccountIsAnError.
	ctx, readFailures := e.OrganizationReadContext(ctx)

	// Machines: their own and the ones lent to them. One source per machine,
	// however many qualifying models it offers.
	models, err := e.fleetCatalogForCaller(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := readFailures.Err(); err != nil {
		return nil, nil, fmt.Errorf("desktop access: the machines theirs or lent to them could not be read: %w", err)
	}
	//
	// Each machine is judged by its OWN entry for a model, what that machine
	// advertised (chatMachineMeetsMinimum), and not by the model's union over
	// every machine behind it: the union counts revoked machines too, so a
	// machine below the floor would count because a revoked one qualified the
	// model (final review, minor 3). A machine appears once per model it
	// offers; the reasons are decided over all of those entries.
	var (
		seen            = map[string]bool{}
		machines        int  // machines that count
		listed          bool // the catalog names any machine, revoked included
		revokedOwn      bool
		revokedLenders  = map[string]bool{}
		live, embedOnly int // entries on machines not revoked, and those that only embed
	)
	for _, m := range models {
		for _, machine := range m.Machines {
			listed = true
			own := strings.TrimSpace(machine.OwnerUserId) == "" || sameUserId(machine.OwnerUserId, actingUserId)
			// A revoked registration stays in the catalog, marked offline; it is
			// not a machine the person has set up.
			if machine.Revoked {
				if own {
					revokedOwn = true
				} else {
					revokedLenders[BareShortId(machine.OwnerUserId)] = true
				}
				continue
			}
			live++
			if !chatMachineMeetsMinimum(machine) {
				// A model that chats and embeds counts (ruling R-D5); one that
				// cannot answer structured output and embeds only embeds.
				if machine.Capabilities.Embeddings && !machine.Capabilities.StructuredOutput {
					embedOnly++
				}
				continue
			}
			if seen[machine.RegistrationId] {
				continue
			}
			seen[machine.RegistrationId] = true
			s := AccessSource{Kind: AccessSourceMachine, State: AccessReady, Own: own, Label: accessLabel(machine.DisplayName, machine.Name, machine.RegistrationId)}
			if !machine.Online {
				s.State = AccessAsleep
			}
			if !own {
				s.LenderUserId = machine.OwnerUserId
			}
			add(s)
			machines++
		}
	}
	if machines == 0 {
		if !listed {
			why(accessReasonNoMachine)
		}
		if revokedOwn {
			why(accessReasonMachineRevoked)
		}
		for lender := range revokedLenders {
			why(accessReasonLentMachineRevoked(lender))
		}
		switch {
		case live == 0:
		case embedOnly == live:
			why(accessReasonEmbeddingsOnly)
		default:
			why(accessReasonBelowFloor)
		}
	}

	// Apps: one read of the person's own registrations, through the same row
	// predicates routing uses, with the "is it ready right now" tests dropped.
	// It does not go through AppDoors because routing's door must leave an app
	// out of usage out (TestAnAppTheMachineMeasuredAsOutOfUsageGetsNoDoor) and
	// desktop access counts it (ruling R-D6).
	apps, err := e.accessAppSources(ctx, actingUserId)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range apps {
		add(s)
	}
	if len(apps) == 0 {
		why(accessReasonNoApp)
	}

	// Keys: asked once per vendor, as a route the person chooses. A cluster
	// with no vendor text record asks nothing and reads as no key, which is
	// what it is for this check: nothing a key could pay at.
	keys := 0
	for _, vendor := range e.providers.textVendors() {
		answer := e.providers.VendorKeysAt(ctx, VendorAccountQuery{Vendor: vendor, ActingUserId: actingUserId, Chosen: true})
		for _, account := range answer.Usable {
			keys++
			add(keySource(account, AccessReady, actingUserId))
		}
		for _, passed := range answer.PassedOver {
			if passed.Account.Kind != VendorAccountKey {
				continue
			}
			if passed.Reason == VendorKeysUnreadableReason {
				// VendorKeysAt reports a failed read as a passed-over key so a
				// call can still go to the cluster's account. Here it would read
				// as "no key", and a person whose keys could not be looked at
				// would be held. A check that fails is unknown, not held.
				return nil, nil, fmt.Errorf("desktop access: the keys of %s could not be read", actingUserId)
			}
			keys++
			account := passed.Account
			if account.Vendor == "" {
				account.Vendor = vendor
			}
			switch passed.Reason {
			case VendorKeyCreditReason(account.Vendor):
				// Credit comes back the way a machine wakes.
				add(keySource(account, AccessOutOfCredit, actingUserId))
			default:
				why(keyReason(account, passed.Reason, actingUserId))
			}
		}
	}
	if keys == 0 {
		why(accessReasonNoKey)
	}

	// The cluster's account counts where it pays for this person. Where it
	// does not, the reason says which half is missing, because the two fixes
	// differ: a person without the permission needs it granted
	// (accessReasonNoClusterPermission, whether or not an account is set up,
	// since VendorDoorFor's Cluster is "set up and not closed to them"); a
	// person holding it needs the administrator to set an account up
	// (accessReasonNoClusterAccount).
	//
	// A grant or membership read that fails inside the gate falls back to the
	// role's answer on purpose (grant_resolver.go), which for a person whose
	// only source is this account would read as "closed" and hold them on a
	// transient database error (ruling R-D8). The holder above hears it, and a
	// noted failure is an error: unknown, not held.
	//
	// LIMIT (review minor 2): the holder hears only reads made through the
	// person's own session. Off that path (personSubject, personRole and
	// organizationUserRole, which swallows a failed principal read) R-D8 still
	// fails closed. desktopAccess runs on the person's own session, so it is
	// not affected; a caller on any other context would be.
	door := e.providers.VendorDoorFor(ctx, actingUserId)
	clusterClosed := ""
	if !door.Cluster && !door.ClusterSetUp {
		clusterClosed = e.providers.ClusterAccountClosedTo(ctx, actingUserId)
	}
	if err := readFailures.Err(); err != nil {
		return nil, nil, fmt.Errorf("desktop access: what the person may use could not be read: %w", err)
	}
	switch {
	case door.Cluster:
		add(AccessSource{Kind: AccessSourceClusterAccount, State: AccessReady, Label: "the cluster's account"})
	case !door.ClusterSetUp && clusterClosed == "":
		why(accessReasonNoClusterAccount)
	default:
		why(accessReasonNoClusterPermission)
	}

	sortAccessSources(sources)
	sort.Strings(reasons)
	return sources, reasons, nil
}

// accessAppReads are the two reads app sources need, so a test can answer them
// without a database. A nil seam reads the real ones.
type accessAppReads struct {
	// Rows are the person's own worker registrations.
	Rows func(ctx context.Context, actingUserId string) ([]map[string]any, error)
	// Block is appBlock for one machine and app.
	Block func(ctx context.Context, registration, app string, measures bool, readyAt time.Time) (until time.Time, signedOut bool, err error)
}

func (e *MemQLEngine) accessReads() accessAppReads {
	if e.accessAppReadsForTest != nil {
		return *e.accessAppReadsForTest
	}
	var (
		once   sync.Once
		recent []map[string]any
		recErr error
	)
	return accessAppReads{
		Rows: func(ctx context.Context, actingUserId string) ([]map[string]any, error) {
			res, err := e.Execute(ContextWithFreshRead(ctx), "workersForUser(ownerUserId: "+langparser.QuoteString(actingUserId)+")")
			if err != nil {
				return nil, err
			}
			return modelPullRows(res.OutputPayload()), nil
		},
		Block: func(ctx context.Context, registration, app string, measures bool, readyAt time.Time) (time.Time, bool, error) {
			once.Do(func() {
				res, err := e.Execute(ContextWithFreshRead(ctx), "query appRoutingEvidence()")
				if err != nil {
					recErr = err
					return
				}
				recent = MaterializeRows(res)
			})
			if recErr != nil {
				return time.Time{}, false, recErr
			}
			return e.appBlock(ctx, registration, app, recent, measures, readyAt)
		},
	}
}

// accessAppSources lists the apps on the person's own machines that count as
// set up (ruling R-D6). An app counts when all hold: the registration is not
// revoked and apps are on for it (appRegistrationAllowsApps, routing's own
// row test), the person's app routing allows it, and airoute.AppIsSetUp says
// its id is runnable, allowed and signed in and not measured signed out. A
// session that was refused as signed out also excludes it, and one refused on
// a usage limit makes it outOfUsage, as does a machine measuring it out of
// usage. "ready" and "unknown" (not checked yet, or a Cockpit that never
// measures) are ready when the machine holds a stream and asleep when not.
//
// Routing is not touched: AppDoors still leaves an app out of usage out.
// Tests: desktop_access_test.go (every state and exclusion) and
// TestAnAppOutOfUsageOnARegistrationCountsAsSetUp (a real row).
func (e *MemQLEngine) accessAppSources(ctx context.Context, actingUserId string) ([]AccessSource, error) {
	reads := e.accessReads()
	rows, err := reads.Rows(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	var out []AccessSource
	seen := map[string]bool{}
	for _, row := range rows {
		if !sameRowAuthzOwner(mapString(row, "ownerUserId"), actingUserId) || !appRegistrationAllowsApps(row) {
			continue
		}
		registration := trimConceptPrefix(mapString(row, "id"))
		policy := AppRoutingPolicyFrom(row["appRouting"])
		online := strings.TrimSpace(mapString(row, "connectedNodeId")) != ""
		entries, _ := row["apps"].([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			id := strings.TrimSpace(mapString(entry, "id"))
			readiness := mapString(entry, "readiness")
			if !airoute.AppIsSetUp(id, entry["allowed"] == true, entry["signedIn"] == true, readiness) || !policy.allows(id) {
				continue
			}
			if key := id + "@" + registration; seen[key] {
				continue
			} else {
				seen[key] = true
			}
			until, signedOut, err := reads.Block(ctx, mapString(row, "id"), id, readiness != "", appMeasuredReadyAt(row, id))
			if err != nil {
				return nil, err
			}
			blocked := time.Now().Before(until)
			if blocked && signedOut {
				continue
			}
			state := AccessReady
			switch {
			case readiness == airoute.AppReadinessOutOfUsage || blocked:
				state = AccessOutOfUsage
			case !online:
				state = AccessAsleep
			}
			out = append(out, AccessSource{Kind: AccessSourceApp, App: id, State: state, Own: true,
				Label: id + " on " + accessLabel(mapString(row, "displayName"), mapString(row, "name"), registration)})
		}
	}
	return out, nil
}

// keySource is a key that counts. A key's label is its vendor and never
// anything that identifies the key.
func keySource(a VendorAccount, state, actingUserId string) AccessSource {
	own := sameUserId(a.OwnerUserId, actingUserId)
	s := AccessSource{Kind: AccessSourceKey, State: state, Own: own, Label: vendorDisplayName(a.Vendor)}
	if !own {
		s.LenderUserId = a.OwnerUserId
	}
	return s
}

// keyReason is the plain reason a key does not count. A lender is named by
// bare id, the one spelling the hold line's subject and user id use too. The resolver's reasons
// are compared whole (VendorKey*Reason), never by words in them. A reason this
// does not know still produces a sentence, so a held person is never left
// with no reason for a key.
func keyReason(a VendorAccount, reason, actingUserId string) string {
	vendor := vendorDisplayName(a.Vendor)
	own := sameUserId(a.OwnerUserId, actingUserId)
	switch {
	case !own && reason == VendorKeyPausedReason:
		return fmt.Sprintf("the key lent by %s is paused by its owner", BareShortId(a.OwnerUserId))
	case !own && reason == VendorKeyOwnerInactiveReason:
		return fmt.Sprintf("the key lent by %s belongs to someone who is no longer an active user", BareShortId(a.OwnerUserId))
	case !own && reason == VendorKeyRefusedReason(a.Vendor):
		return fmt.Sprintf("the key lent by %s was refused by %s", BareShortId(a.OwnerUserId), vendor)
	case !own:
		return fmt.Sprintf("the key lent by %s cannot be used", BareShortId(a.OwnerUserId))
	case reason == VendorKeyRefusedReason(a.Vendor):
		return "their own key was refused by " + vendor
	default:
		return "their own key cannot be used"
	}
}

// textVendors are the vendors that have a text record, once each and in a
// fixed order. A person's keys are asked once per vendor, not per record.
func (r *ProviderRegistry) textVendors() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := map[string]bool{}
	for _, entry := range r.byName {
		if v := VendorOfEntry(entry); v != "" {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// accessLabel is what a person calls a machine: the name they gave it, then
// what the cockpit reported, then its registration id.
func accessLabel(displayName, name, registrationId string) string {
	for _, candidate := range []string{displayName, name} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return registrationId
}

// sortAccessSources fixes an order so the same setup always reads the same.
func sortAccessSources(s []AccessSource) {
	rank := map[string]int{AccessSourceMachine: 0, AccessSourceApp: 1, AccessSourceKey: 2, AccessSourceClusterAccount: 3}
	sort.SliceStable(s, func(i, j int) bool {
		if rank[s[i].Kind] != rank[s[j].Kind] {
			return rank[s[i].Kind] < rank[s[j].Kind]
		}
		if s[i].Label != s[j].Label {
			return s[i].Label < s[j].Label
		}
		return s[i].LenderUserId < s[j].LenderUserId
	})
}

// accessReasonsDigest is the first 16 hex characters of the sha256 of the
// sorted reasons joined by newline: the same reasons give the same digest in
// any order, so a re-check can tell "nothing changed" from "something did".
func accessReasonsDigest(reasons []string) string {
	sorted := append([]string(nil), reasons...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// accessPersonPartLen is the length of the person part of a digest: the first
// 8 hex characters of a sha256 of the person's bare id.
const accessPersonPartLen = 8

func accessPersonPart(userId string) string {
	sum := sha256.Sum256([]byte(BareShortId(strings.TrimSpace(userId))))
	return hex.EncodeToString(sum[:])[:accessPersonPartLen]
}

// personAccessDigest is the digest a held answer carries (ruling R-D9, as
// amended): the person part, then the reasons digest. The person part lets a
// later check tell "the same person, whose reference this is" from somebody
// else on the same page after a sign-in change, who must never be handed the
// first person's reference.
func personAccessDigest(userId string, reasons []string) string {
	return accessPersonPart(userId) + accessReasonsDigest(reasons)
}

// digestIsFor reports whether a passed digest parses (person part plus a
// 16-hex reasons digest) and its person part is this person's.
func digestIsFor(digest, userId string) bool {
	if len(digest) != accessPersonPartLen+16 {
		return false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return false
	}
	return digest[:accessPersonPartLen] == accessPersonPart(userId)
}

// AccessHoldLine is what is written when a person is held: enough for an
// administrator holding the reference to find the person and why.
type AccessHoldLine struct {
	Reference string
	UserId    string
	Reasons   []string
}

var accessHoldRecorder struct {
	sync.RWMutex
	record func(ctx context.Context, line AccessHoldLine) error
}

// SetAccessHoldRecorder installs the writer of the hold line (the log store,
// wired at boot), the SetEmbedderBindingReader pattern. With none installed
// writing counts as failed, so a node that cannot write the line holds people
// with no reference and never with one nobody can look up.
func SetAccessHoldRecorder(record func(ctx context.Context, line AccessHoldLine) error) {
	accessHoldRecorder.Lock()
	defer accessHoldRecorder.Unlock()
	accessHoldRecorder.record = record
}

func recordAccessHold(ctx context.Context, line AccessHoldLine) error {
	accessHoldRecorder.RLock()
	record := accessHoldRecorder.record
	accessHoldRecorder.RUnlock()
	if record == nil {
		return errors.New("desktop access: no hold recorder installed")
	}
	return record(ctx, line)
}

// accessReferencePattern is the shape of a reference: the ERR- ids the logs
// already carry (generateErrorId), so Logs search finds it as it finds those.
var accessReferencePattern = regexp.MustCompile("^ERR-[0-9a-f]{6}$")

// newAccessReference mints a reference from three random bytes.
func newAccessReference() string {
	var b [3]byte
	// crypto/rand.Read does not return an error on this Go version.
	_, _ = rand.Read(b[:])
	return "ERR-" + hex.EncodeToString(b[:])
}

// DesktopAccess is the answer to "may this person open MemQL OS now".
type DesktopAccess struct {
	Admitted, Exempt bool
	// Sources are what the person has set up, computed for the exempt too
	// (Ask uses them).
	Sources []AccessSource
	// Reference and ReasonsDigest are set only on a held answer. Both are
	// empty when the line could not be written.
	Reference, ReasonsDigest string
}

// desktopAccess decides. passedReference and passedDigest are what the client
// got from its previous held answer ("" for none); see the header for what
// each case writes. An error is never a hold.
//
// Tests: desktop_access_hold_test.go.
func (e *MemQLEngine) desktopAccess(ctx context.Context, actingUserId, passedReference, passedDigest string) (DesktopAccess, error) {
	actingUserId = strings.TrimSpace(actingUserId)
	if auth.NamesNoPerson(actingUserId) {
		return DesktopAccess{}, errors.New("desktop access: the call names no person")
	}

	role, found, err := e.accessStoredRole(ContextWithFreshRead(ctx), actingUserId)
	if err != nil {
		return DesktopAccess{}, fmt.Errorf("desktop access: the stored role could not be read: %w", err)
	}
	sources, reasons, err := e.desktopAccessSources(ctx, actingUserId)
	if err != nil {
		return DesktopAccess{}, err
	}
	switch {
	case found && auth.MayManageFleet(role):
		return DesktopAccess{Admitted: true, Exempt: true, Sources: sources}, nil
	case len(sources) > 0:
		return DesktopAccess{Admitted: true, Sources: sources}, nil
	}

	// The digest binds the person (ruling R-D9): a reference and digest carried
	// across a sign-in change on one page never answer for a second person. A
	// passed reference is reused only when the digest's person part is this
	// person's; for anybody else, or a digest that does not parse, a new
	// reference is minted and its line written, so a reference is never shared
	// by two people. The line's Reasons stay the plain reasons.
	digest := personAccessDigest(actingUserId, reasons)
	answer := DesktopAccess{Sources: sources, ReasonsDigest: digest}
	reference := ""
	if accessReferencePattern.MatchString(passedReference) && digestIsFor(passedDigest, actingUserId) {
		reference = passedReference
		if passedDigest == digest {
			answer.Reference = reference
			return answer, nil
		}
	} else {
		reference = newAccessReference()
	}
	// Written before the reference is returned. A failure answers held with
	// no reference and no digest (nothing to compare a re-check against), and
	// is logged: otherwise "no recorder installed" and "the log store refused
	// the line" would both show only as a page with no reference.
	if err := recordAccessHold(ctx, AccessHoldLine{Reference: reference, UserId: BareShortId(actingUserId), Reasons: reasons}); err != nil {
		slog.Warn("desktop access: the hold line could not be written; the person is held with no reference",
			"userId", actingUserId, "error", err)
		answer.ReasonsDigest = ""
		return answer, nil
	}
	answer.Reference = reference
	return answer, nil
}

func (e *MemQLEngine) accessStoredRole(ctx context.Context, userId string) (auth.Role, bool, error) {
	if e.storedRoleForTest != nil {
		return e.storedRoleForTest(ctx, userId)
	}
	return e.storedUserRole(ctx, userId)
}

// DesktopAccessConcept is the canonical id of the desktopAccess row.
const DesktopAccessConcept = "v1:platform:desktopAccess"

// evaluateDesktopAccess is the `desktopAccess` builtin: ONE row answering
// whether the caller may enter MemQL OS (design section 3). Virtual, never
// persisted (the answer is the caller's setup right now).
//
// THE PERSON IS THE ACTOR, NEVER AN ARGUMENT. The only arguments are the
// reference and digest the client got from its previous held answer; nothing
// can name another person, so the call cannot be used to read someone else's
// setup, and it runs on the person's own session, which keeps the
// cluster-account gate on its session path. A call that names nobody is an
// error (desktopAccess refuses it).
//
// AN ERROR STAYS AN ERROR. The client reads a failed call as "unknown" and
// lets the person in; dressing a read failure as a row would hold somebody
// out of the console because a database blinked.
func (e *MemQLEngine) evaluateDesktopAccess(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	reference, _ := args["reference"].(string)
	digest, _ := args["reasonsDigest"].(string)
	answer, err := e.desktopAccess(ctx, actingUserFromContext(ctx), reference, digest)
	if err != nil {
		return nil, err
	}
	sources := answer.Sources
	if sources == nil {
		sources = []AccessSource{}
	}
	payload := map[string]any{
		"admitted": answer.Admitted,
		"exempt":   answer.Exempt,
		"sources":  sources,
	}
	// Absent, not empty, when nothing was held or the line could not be
	// written: an empty reference is "no reference to show".
	if answer.Reference != "" {
		payload["reference"] = answer.Reference
	}
	if answer.ReasonsDigest != "" {
		payload["reasonsDigest"] = answer.ReasonsDigest
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:      "current",
		Concept: DesktopAccessConcept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}
