package campaigns

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// delivery_id.go -- re-deriving the delivery row's id in Go (memql#4823).
//
// # Why this exists at all
//
// A tracking token names the DELIVERY it belongs to, and the token has to be
// inside the message body. The delivery row's id is derived by the mutation,
// at insert, from (campaignId, recipientId) -- and the insert happens AFTER
// the message has been handed to the transport, because a delivery is the
// record of an outcome and the outcome is not known until the send returns.
// So the id cannot be read back before it is needed. A body cannot contain an
// id that does not exist yet.
//
// The alternatives were both worse. Writing a `pending` delivery row first,
// reading its id and then sending would double the writes on the hot path and
// leave a `pending` row behind every message the process died halfway
// through. Putting a synthetic id in the token would make
// v1:campaigns:engagementEvent.deliveryId point at nothing, so "which send
// did this open come from" -- the question the row exists to answer -- would
// have no answer.
//
// # This is a SECOND IMPLEMENTATION of a derivation the DSL owns, and that is
// the risk
//
// `recordCampaignDelivery` derives:
//
//	id: hash(concat(
//	      hash(canonicalId(args.campaignId, campaign)),
//	      hash(canonicalId(args.recipientId, recipient))
//	    ))
//
// with `hash(x)` = hex(sha256(x)) and `canonicalId(v, C)` = v when v is
// already tagged with C, else "C:v". This file reproduces exactly that, and
// the failure mode of a drift is silent in the worst way: every engagement
// event would reference a delivery row that does not exist, the counts would
// still tally, and nothing would report an error. Nothing here reads back to
// confirm it, because on the send path there is nothing to read yet.
//
// So the DSL expression itself is the gate. delivery_id_test.go parses the id
// block out of dsl/campaigns/mutations.memql and fails when it stops matching
// the shape this file assumes -- which is the only check available that is
// not a copy of the thing being checked.

const (
	campaignConcept  = "v1:campaigns:campaign"
	recipientConcept = "v1:campaigns:recipient"
)

// deliveryRowID reproduces recordCampaignDelivery's derived id.
//
// PER-PART HASHED before concatenation, exactly as the mutation does it
// (authoring rule 20): hashing each part first means no separator inside a
// caller-supplied id can make two different (campaign, recipient) pairs
// collide onto one row. Concatenating the raw ids and hashing once would be
// one fewer operation and a genuine collision surface.
func deliveryRowID(campaignID, recipientID string) string {
	campaign := sha256Hex(canonicalIDFor(campaignConcept, campaignID))
	recipient := sha256Hex(canonicalIDFor(recipientConcept, recipientID))
	return sha256Hex(campaign + recipient)
}

// canonicalIDFor mirrors the engine's canonicalizeIdValue for the one shape
// this package produces.
//
// The engine's version also VALIDATES -- an unknown concept, or a value
// already tagged with a different one, is an error there. This one does not,
// deliberately: it is called on ids that came off rows the engine itself
// handed back moments earlier, so there is nothing new to validate, and an
// error return here would have to be handled on a path whose only sensible
// response is to send the message anyway.
func canonicalIDFor(concept, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, concept+":") {
		return value
	}
	if strings.ContainsRune(value, ':') {
		// Already tagged, and with something else. Left exactly as given:
		// re-tagging it would invent an id, and the engine treats this case
		// as an error rather than a rewrite for the same reason.
		return value
	}
	return concept + ":" + value
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
