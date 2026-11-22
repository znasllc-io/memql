package memql

import (
	"fmt"
	"sort"
	"strings"
)

// SOME ROWS NEVER CHANGE HANDS, WHOEVER WRITES.
//
// A row of these concepts is one person's own credential, or the engine's
// record about it: an API key that spends their money (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K5 and K16), and the sealed access to their repositories that a
// private source fetches under (owner decision 2026-11-22). Each declares the
// owned row tier over `ownerUserId`, and that field is what decides whose
// credential it is: the key resolver reads it to choose who pays
// (integrations/vendorkeys/resolver.go), and a fetch resolves a credential
// under the package owner's actor, so only its owner's packages fetch with it
// (component/packages/credentials.go). The value is stamped once, from the
// actor who added the credential, and no builtin takes it as an argument.
//
// That was not enough, and this file is why. @serverWritten and the source
// credential's own rule ask only for internal origin, and a row tier's
// cluster-owner arm admits a cluster owner, so a raw write naming only
// `ownerUserId` passed every check the row had. integrations/identity's
// ownership transfer makes exactly that write for every owned concept. It let
// a cluster owner take a developer's API key by naming it, and during an
// ordinary offboarding it handed a leaver's keys and source access to their
// heir: keys still active and still a default, and access the heir then used
// to fetch as the person who left.
//
// THE RULE: on a stored row of a concept listed below, the owner field does
// not change, for any writer. Not for a cluster owner, not under internal
// origin, not for server code. Nobody has a reason to move one: a credential
// is removed or revoked, and its new owner adds or connects their own.
//
// KEYED ON A CHANGE. A read-merge carries the field through every ordinary
// write unchanged, and a write that names the same owner again is not a
// change. A write that names no owner at all is not judged.
//
// WHAT IT COSTS. A package handed to somebody else no longer fetches under the
// access of the person who left: its fetch answers credential_not_found or
// source_connection_unavailable until its new owner selects a connection of
// their own on the package's Source stop. That is the decision, not a side
// effect of it.
//
// The transfer does not rely on this refusal: every concept here is on its
// stampedAtBirth list, so an offboarding skips them and completes
// (TestTransferSkipsEveryConceptWhoseOwnerTheEngineHolds holds the two lists
// together; a concept only here would fail a whole-user transfer partway).
// This is what holds the rule for every other writer, present and future.
//
// Regression tests: TestAVendorKeysOwnerNeverChanges
// (vendor_key_rows_db_test.go), TestAPersonsSourceAccessNeverChangesHands
// (platform_source_credential_write_db_test.go), and in integrations/identity
// TestTransferLeavesAPersonsApiKeysWhereTheyAre and
// TestTransferLeavesAPersonsSourceAccessWhereItIs.

// ownerHeldField is the owner field of every concept below.
const ownerHeldField = "ownerUserId"

// conceptPlatformSourceConnection is a person's selection from their own
// GitHub access.
const conceptPlatformSourceConnection = "v1:platform:sourceConnection"

// ownerHeld lists the concepts whose stored owner no writer may change, with
// what the refusal says about each.
var ownerHeld = map[string]string{
	vendorKeyConcept: "an API key's owner never changes. A key spends the money of the person who added it, " +
		"so nobody else comes to own it, whoever writes. Remove the key, and its new owner adds their own",
	vendorKeyStateConcept: "an API key's owner never changes. A key spends the money of the person who added it, " +
		"so nobody else comes to own it, whoever writes. Remove the key, and its new owner adds their own",
	conceptPlatformSourceCredential: "a source credential's owner never changes. It is one person's access to " +
		"their repositories, so nobody else comes to own it, whoever writes. Its new owner connects their own",
	conceptPlatformSourceConnection: "a source connection's owner never changes. It is a selection one person " +
		"made from their own source access and stays beside it. Its new owner connects their own",
}

// OwnerHeldConcepts lists the concepts whose stored owner the engine refuses
// to change, sorted. The ownership transfer's test reads it, so that every one
// of them is a concept the transfer skips.
func OwnerHeldConcepts() []string {
	names := make([]string, 0, len(ownerHeld))
	for name := range ownerHeld {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// guardOwnerHeld refuses a write that would change who owns a stored row of a
// concept on ownerHeld. prior is the stored payload and incoming the write's
// own, before the two are merged.
func guardOwnerHeld(conceptName string, prior, incoming map[string]any) error {
	why, held := ownerHeld[conceptName]
	if !held {
		return nil
	}
	named, writes := incoming[ownerHeldField]
	if !writes {
		return nil
	}
	was := BareShortId(strings.TrimSpace(stringFromAny(prior[ownerHeldField])))
	now := BareShortId(strings.TrimSpace(stringFromAny(named)))
	if was == now {
		return nil
	}
	return fmt.Errorf("%s: %s", conceptName, why)
}
