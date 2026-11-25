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
// two share their predicates (chatModelMeetsMinimum, AppDoors, the resolver's
// reasons) and drop only the "is it awake" tests, so they cannot drift apart
// in anything else. What does NOT count: a model that only embeds, a model
// below the context floor, a key that was refused or paused or whose owner
// left, an app measured signed out.
//
// EVERY SOURCE THAT DOES NOT COUNT ADDS ONE PLAIN REASON, so the page's
// reference can be tied to why a person was held. A reason names a lender's id
// only for something lent to this person, and never a key, a machine name or
// a vendor's words. Reasons are digested (accessReasonsDigest) so a re-check
// that finds the same reasons can reuse the reference it already logged.
//
// Tests: desktop_access_test.go. Not verified here: the exemption, the
// reference and the log line (later tasks).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
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

// AccessSource is one model source a person has set up. LenderUserId is set
// only for something lent to them; Own is true for their own machines, apps
// and keys.
type AccessSource struct {
	Kind         string `json:"kind"`
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
		return nil, nil, nil
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

	// Machines: their own and the ones lent to them. One source per machine,
	// however many qualifying models it offers.
	models, err := e.fleetCatalogForCaller(ctx)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	machines := 0
	for _, m := range models {
		if !chatModelMeetsMinimum(m) {
			continue
		}
		for _, machine := range m.Machines {
			if seen[machine.RegistrationId] {
				continue
			}
			seen[machine.RegistrationId] = true
			own := strings.TrimSpace(machine.OwnerUserId) == "" || sameUserId(machine.OwnerUserId, actingUserId)
			s := AccessSource{Kind: AccessSourceMachine, State: AccessReady, Own: own, Label: accessMachineLabel(machine)}
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
		why("no machine of theirs or lent to them offers a chat model")
	}

	// Apps: AppDoors keeps own machines only and keeps them offline.
	doors, err := e.providers.AppDoors(ctx, actingUserId)
	if err != nil {
		return nil, nil, err
	}
	apps := 0
	for _, door := range doors {
		for _, machine := range door.Machines {
			state := AccessReady
			switch {
			case machine.OutOfUsage:
				state = AccessOutOfUsage
			case !machine.Online:
				state = AccessAsleep
			}
			add(AccessSource{Kind: AccessSourceApp, State: state, Own: true,
				Label: door.AppId + " on " + accessAppMachineLabel(machine)})
			apps++
		}
	}
	if apps == 0 {
		why("no app on their machines is allowed and signed in")
	}

	// Keys: asked once per vendor, as a route the person chooses.
	for _, vendor := range e.providers.textVendors() {
		answer := e.providers.VendorKeysAt(ctx, VendorAccountQuery{Vendor: vendor, ActingUserId: actingUserId, Chosen: true})
		for _, account := range answer.Usable {
			add(keySource(account, AccessReady, actingUserId))
		}
		for _, passed := range answer.PassedOver {
			if passed.Account.Kind != VendorAccountKey {
				continue
			}
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

	// The cluster's account counts where it pays for this person. Holding the
	// permission with no account set up is a reason of its own, because the
	// fix is the administrator's and not the person's.
	door := e.providers.VendorDoorFor(ctx, actingUserId)
	switch {
	case door.Cluster:
		add(AccessSource{Kind: AccessSourceClusterAccount, State: AccessReady, Label: "the cluster's account"})
	case !door.ClusterSetUp && e.providers.ClusterAccountClosedTo(ctx, actingUserId) == "":
		why("may use the cluster's account, but none is set up")
	}

	sortAccessSources(sources)
	sort.Strings(reasons)
	return sources, reasons, nil
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

// keyReason is the plain reason a key does not count. The resolver's reasons
// are compared whole (VendorKey*Reason), never by words in them. A reason this
// does not know still produces a sentence, so a held person is never left
// with no reason for a key.
func keyReason(a VendorAccount, reason, actingUserId string) string {
	if a.KeyId == "" {
		return "their API keys could not be read"
	}
	vendor := vendorDisplayName(a.Vendor)
	own := sameUserId(a.OwnerUserId, actingUserId)
	switch {
	case !own && reason == VendorKeyPausedReason:
		return fmt.Sprintf("the key lent by %s is paused by its owner", a.OwnerUserId)
	case !own && reason == VendorKeyOwnerInactiveReason:
		return fmt.Sprintf("the key lent by %s belongs to someone who is no longer an active user", a.OwnerUserId)
	case !own && reason == VendorKeyRefusedReason(a.Vendor):
		return fmt.Sprintf("the key lent by %s was refused by %s", a.OwnerUserId, vendor)
	case !own:
		return fmt.Sprintf("the key lent by %s cannot be used", a.OwnerUserId)
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

func accessMachineLabel(m FleetMachine) string {
	switch {
	case strings.TrimSpace(m.DisplayName) != "":
		return m.DisplayName
	case strings.TrimSpace(m.Name) != "":
		return m.Name
	}
	return m.RegistrationId
}

func accessAppMachineLabel(m AppMachine) string {
	switch {
	case strings.TrimSpace(m.DisplayName) != "":
		return m.DisplayName
	case strings.TrimSpace(m.Name) != "":
		return m.Name
	}
	return m.RegistrationId
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
