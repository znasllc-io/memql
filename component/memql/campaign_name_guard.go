package memql

import (
	"fmt"
	"unicode/utf8"
)

// The longest names a campaign's mail can show (memql#6033).
//
// Every message carries a view-in-browser link, and the link carries the
// campaign's name and the sender's so the page can show the message as it was
// sent. A link token has a size limit (component/campaigns/link_token.go), and a
// name is free text somebody typed, so both names are capped where they are
// saved. v1:campaigns:campaign.name, campaign.fromName and senderIdentity.fromName
// declare the same numbers as @maxLength, which is what bounds a write that
// reaches the store by any road; the numbers here are the ones component/campaigns
// sizes its tokens by, and a test there holds the two together.
//
// # Why there is a guard as well as the annotation
//
// The annotation says no, in the engine's words: `argument "name": value too
// long (201 runes, max 200)`. The app shows the server's refusal as it is, beside
// the control that caused it, so that is what a person reads. A mutation cannot
// state its own refusal -- @maxLength takes a number and nothing else, and a
// mutation body is declarative -- so the sentence lives where every other
// write rule that needs a sentence lives: here, at the write chokepoint, beside
// shopify_store_guard.go and the site guards. It runs on the merged row, so a
// name saved before the cap existed is told about too, on whatever write next
// touches the row, instead of failing the schema with a message about a field
// that write never mentioned.
//
// For the same reason the mutations that save these names do not carry
// @maxLength on their arguments: the argument check runs first and would answer
// before this does.

const (
	conceptCampaignsCampaign       = "v1:campaigns:campaign"
	conceptCampaignsSenderIdentity = "v1:campaigns:senderIdentity"

	// CampaignNameMaxRunes is the most characters a campaign's name may have.
	CampaignNameMaxRunes = 200

	// SenderNameMaxRunes is the most characters a sender's display name may have,
	// whether it is the sending identity's or a campaign's own override.
	SenderNameMaxRunes = 100
)

// nameCapRefusal is a refusal worded for the person who typed the name: what to
// do, in a sentence, and nothing about the engine.
type nameCapRefusal string

func (r nameCapRefusal) Error() string { return string(r) }

var (
	errCampaignNameTooLong = nameCapRefusal(fmt.Sprintf("Use a campaign name of %d characters or fewer.", CampaignNameMaxRunes))
	errSenderNameTooLong   = nameCapRefusal(fmt.Sprintf("Use a sender name of %d characters or fewer.", SenderNameMaxRunes))
)

// validateCampaignNameLengths refuses a campaign or sending-identity row whose
// name is over its cap. Characters are counted, not bytes, as @maxLength counts
// them. payload is the merged row.
func validateCampaignNameLengths(concept string, payload map[string]any) error {
	switch concept {
	case conceptCampaignsCampaign:
		if nameRunes(payload["name"]) > CampaignNameMaxRunes {
			return errCampaignNameTooLong
		}
		if nameRunes(payload["fromName"]) > SenderNameMaxRunes {
			return errSenderNameTooLong
		}
	case conceptCampaignsSenderIdentity:
		if nameRunes(payload["fromName"]) > SenderNameMaxRunes {
			return errSenderNameTooLong
		}
	}
	return nil
}

// nameRunes is the length in characters of a name field, or zero for a value
// that is not a string (the concept's schema refuses those on its own).
func nameRunes(v any) int {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	return utf8.RuneCountInString(s)
}
