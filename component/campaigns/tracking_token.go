package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// tracking_token.go -- the open/click capability (memql#4823, design D13).
//
// This file is token.go's twin and is deliberately built the same way: a
// keyed MAC over base64url-encoded parts, joined by ".", with the key id
// INSIDE the signed body, verified with hmac.Equal against a rotation ring of
// two. Everything token.go argues for applies here unchanged --
//
//   - STATELESS. A stored token would mean one row per (delivery, link) at
//     SEND time, which multiplies the write cost of a campaign by the number
//     of links in it and leaves a table that can never be pruned.
//   - The KEY ID IS DERIVED FROM THE SECRET, not from a slot. A pixel
//     fetched next month is verified by a node on which today's secret has
//     since become the previous one, so a positional label would be wrong
//     exactly when it is needed.
//   - hmac.Equal throughout. These endpoints are unauthenticated and
//     rate-limited only by the network.
//
// # What is DIFFERENT, and why each difference exists
//
// 1. THE KEY RING IS SHARED; THE DOMAIN SEPARATION IS NOT.
//
// Both token families are signed by MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET and
// its _PREVIOUS, because they are minted by the same process, delivered in
// the same message and reached at the same public origin -- a second secret
// would be a second thing to rotate for no property gained, and an operator
// who rotated one and not the other would break half the links in every
// mailbox.
//
// Sharing a key makes CROSS-VERIFICATION the risk: a tracking token that
// verified as an unsubscribe token would be an opt-out anybody could trigger
// by loading an image, and an unsubscribe token that verified as a tracking
// token would let a click be attributed to a delivery it did not belong to.
// Three independent things prevent it, and any one would be enough:
//
//	the key-id LABEL   trackingKeyIDLabel differs from unsubscribeKeyIDLabel,
//	                   so the same secret produces a different key id and
//	                   neither parser recognises the other's
//	the CONTEXT string trackingContext rides INSIDE the signed body, so a tag
//	                   computed over an unsubscribe body can never match one
//	                   computed over a tracking body even under one key
//	the part COUNT     six parts versus eight, refused before any MAC is
//	                   computed
//
// The context string is the one that would still hold if the other two were
// changed by somebody who did not know why they were there, which is why it
// is in the body rather than merely in the prefix.
//
// 2. IT CARRIES A URL, AND THE SIGNATURE OVER IT IS THE OPEN-REDIRECT
// DEFENCE.
//
// The click endpoint 302s to the target the token names. That is an open
// redirect unless the target is authenticated, so the URL is inside the
// signed body: a tampered or invented target does not redirect ANYWHERE, it
// renders the same "link not valid" page an altered unsubscribe link gets.
// There is deliberately no allowlist of hosts and no scheme filter at
// verification time -- the campaign author put the link in the message, and
// the signature is the statement that we are the ones who put it there.
//
// 3. THE TOKEN IS A SINGLE PATH SEGMENT, AND THAT IS A CONSTRAINT NOT A
// COINCIDENCE.
//
// The unsubscribe token rides in a QUERY parameter; these ride as a PATH
// SEGMENT, because a mail client fetches /t/o/<token> as an image src. The
// self-authenticated bypass that lets an unauthenticated request reach the
// handler is bounded to ONE further segment under the mount (memql#3128), so
// a token containing "/" is not merely mis-parsed -- it is not exempted, and
// the fetch is 401'd before the handler runs, which reaches a person as a
// broken image. base64url emits no "/" and "." is legal in a path segment,
// which is what makes this encoding the one that works.
//
// 4. NO EXPIRY, same as the unsubscribe token and for a weaker reason. An
// unsubscribe link must work forever because a stale one is a compliance
// failure. A tracking token has no such obligation -- but expiry would mean
// carrying a timestamp and deciding what a hit past it MEANS, and the honest
// answer ("count it, it happened") is the same as having no expiry. What
// bounds the window in practice is a second rotation, exactly as for
// unsubscribe.

const (
	// EngagementOpen and EngagementClick are the two kinds
	// v1:campaigns:engagementEvent records. A closed pair: an open is a
	// pixel fetch and a click is a redirect, and there is no third thing
	// this transport can observe.
	EngagementOpen  = "open"
	EngagementClick = "click"

	// TrackingOpenPath and TrackingClickPath are the routes the two
	// endpoints mount at. They are PREFIXES -- the token is the segment
	// after them -- and their literals must agree with
	// server.TrackingPaths(), which is what the front-door generator reads.
	// A disagreement is a path the ingress routes and nothing serves, or a
	// path something serves and the ingress does not route; the second
	// reaches a recipient as a broken image.
	TrackingOpenPath  = "/t/o/"
	TrackingClickPath = "/t/c/"

	// trackingTokenVersion prefixes every token. t1 is the first format;
	// there is no earlier one to accept.
	trackingTokenVersion = "t1"

	// trackingContext domain-separates this token family from the
	// unsubscribe one under the SHARED signing key. It is inside the signed
	// body rather than only in the prefix, so it is covered by the tag.
	trackingContext = "memql/campaigns/track/v1"

	// trackingTagBytes matches the unsubscribe token's truncation: 16 bytes
	// / 128 bits, the standard floor for a MAC, and short enough that the
	// resulting URL survives a mail client's line wrapping. A wrapped image
	// src is a broken image.
	trackingTagBytes = 16

	// trackingKeyIDBytes is the length of the key label in the token. Four
	// bytes picks one of at most two keys; a collision costs a second
	// constant-time comparison rather than an admission.
	trackingKeyIDBytes = 4

	// trackingKeyIDLabel is DIFFERENT from unsubscribeKeyIDLabel on purpose
	// -- see the file doc's first difference. The same secret must produce a
	// different key id in each family.
	trackingKeyIDLabel = "memql/campaigns/track/key-id"

	// trackingTokenParts is the part count of a well-formed token:
	// version, key id, context, deliveryId, campaignId, kind, url, tag.
	trackingTokenParts = 8
)

var (
	errBadTrackingToken = errors.New("campaigns: tracking token is not valid")

	// errUnknownTrackingKey mirrors errUnknownUnsubscribeKey and exists for
	// the same single reason: it is the ONE failure an operator can fix, and
	// distinguishing it lets the log say "a rotation dropped a secret that
	// had already signed live links" instead of "somebody probed us". The
	// recipient sees the same answer either way.
	errUnknownTrackingKey = errors.New("campaigns: tracking token names a signing key this node does not hold")
)

// TrackingPayload is what a verified tracking token carries.
type TrackingPayload struct {
	DeliveryID string
	CampaignID string
	Kind       string
	// URL is the redirect target for a click, and empty for an open. Signed,
	// which is what makes the redirect safe.
	URL string
}

// TrackingKeyID is the short public id of a signing secret under the TRACKING
// label. Stable for a given secret and independent of which slot it occupies.
func TrackingKeyID(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(trackingKeyIDLabel))
	return hex.EncodeToString(mac.Sum(nil)[:trackingKeyIDBytes])
}

// MintTrackingToken produces the token embedded in a tracked URL.
//
// campaignId is covered by the tag alongside deliveryId, so the handler can
// resolve the campaign's owner without trusting anything the request said --
// the same reason the unsubscribe token carries an owner. kind is covered so
// a click token cannot be replayed at the open endpoint to inflate a
// different counter.
func MintTrackingToken(secret string, p TrackingPayload) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("campaigns: no unsubscribe secret configured, so no tracking token can be signed")
	}
	if strings.TrimSpace(p.DeliveryID) == "" || strings.TrimSpace(p.CampaignID) == "" {
		return "", errors.New("campaigns: a tracking token needs both a delivery and a campaign")
	}
	if p.Kind != EngagementOpen && p.Kind != EngagementClick {
		return "", fmt.Errorf("campaigns: tracking kind %q is not open or click", p.Kind)
	}
	body := encodeTrackingBody(secret, p)
	return body + "." + base64.RawURLEncoding.EncodeToString(trackingTag(secret, body)), nil
}

// ParseTrackingToken verifies a token against the ring and returns what it
// covers. keys is Config.UnsubscribeKeys(): current first, then previous.
func ParseTrackingToken(keys []string, token string) (TrackingPayload, error) {
	if len(keys) == 0 {
		return TrackingPayload{}, errors.New("campaigns: no unsubscribe secret configured, so no tracking token can be verified")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != trackingTokenParts || parts[0] != trackingTokenVersion {
		return TrackingPayload{}, errBadTrackingToken
	}
	body := strings.Join(parts[:trackingTokenParts-1], ".")
	presented, decodeErr := base64.RawURLEncoding.DecodeString(parts[trackingTokenParts-1])
	if decodeErr != nil || len(presented) != trackingTagBytes {
		return TrackingPayload{}, errBadTrackingToken
	}

	// Narrow to the key the token names FIRST, exactly as the unsubscribe
	// parser does: without it every probe costs a MAC per held key, and an
	// expired link and a forged one would be the same event in the log.
	named := 0
	verified := false
	for _, key := range keys {
		if !hmac.Equal([]byte(TrackingKeyID(key)), []byte(parts[1])) {
			continue
		}
		named++
		if hmac.Equal(presented, trackingTag(key, body)) {
			verified = true
		}
	}
	if named == 0 {
		return TrackingPayload{}, errUnknownTrackingKey
	}
	if !verified {
		return TrackingPayload{}, errBadTrackingToken
	}

	context, cerr := base64.RawURLEncoding.DecodeString(parts[2])
	delivery, derr := base64.RawURLEncoding.DecodeString(parts[3])
	campaign, kerr := base64.RawURLEncoding.DecodeString(parts[4])
	kind, xerr := base64.RawURLEncoding.DecodeString(parts[5])
	target, uerr := base64.RawURLEncoding.DecodeString(parts[6])
	if cerr != nil || derr != nil || kerr != nil || xerr != nil || uerr != nil {
		return TrackingPayload{}, errBadTrackingToken
	}
	// The context is checked AFTER the MAC, which is the only order that
	// means anything: a body whose context segment was edited fails the tag
	// first. Checking it here is what catches a token minted by a future
	// version of this code under a different context but the same key.
	if !hmac.Equal(context, []byte(trackingContext)) {
		return TrackingPayload{}, errBadTrackingToken
	}
	if len(delivery) == 0 || len(campaign) == 0 {
		return TrackingPayload{}, errBadTrackingToken
	}
	if k := string(kind); k != EngagementOpen && k != EngagementClick {
		return TrackingPayload{}, errBadTrackingToken
	}
	return TrackingPayload{
		DeliveryID: string(delivery),
		CampaignID: string(campaign),
		Kind:       string(kind),
		URL:        string(target),
	}, nil
}

// encodeTrackingBody joins the version, the key id, the context and the four
// base64url-encoded payload fields.
//
// Each field is ENCODED rather than escaped, so the "." separator cannot
// occur inside one and the split above is unambiguous -- the same injectivity
// argument the DSL's composite-id derivation makes about its separator
// (authoring rule 20), and the reason a URL containing dots is safe to carry
// here.
func encodeTrackingBody(secret string, p TrackingPayload) string {
	enc := base64.RawURLEncoding.EncodeToString
	return strings.Join([]string{
		trackingTokenVersion,
		TrackingKeyID(secret),
		enc([]byte(trackingContext)),
		enc([]byte(p.DeliveryID)),
		enc([]byte(p.CampaignID)),
		enc([]byte(p.Kind)),
		enc([]byte(p.URL)),
	}, ".")
}

func trackingTag(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)[:trackingTagBytes]
}

// trackingKeyIDOf pulls the (public, non-secret) key id out of a token for a
// log line. Returns "" for anything not shaped like a token -- a log must
// never echo attacker-chosen bytes.
func trackingKeyIDOf(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != trackingTokenParts || parts[0] != trackingTokenVersion {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil || len(parts[1]) != trackingKeyIDBytes*2 {
		return ""
	}
	return parts[1]
}

// trackingRenderFor builds the render-time tracking configuration for one
// message.
//
// The delivery id is DERIVED here rather than read back, and that is what
// makes tracking possible at all on the send path: the delivery row's id is
// hash(hash(canonicalId(campaign)) || hash(canonicalId(recipient))) --
// computed by the mutation, at insert, AFTER the message has already been
// handed to the transport. A body cannot contain an id that does not exist
// yet, so the renderer computes the same derivation the mutation will.
func (c Config) trackingRenderFor(campaign Campaign, deliveryID string) TrackingRender {
	return TrackingRender{
		BaseURL: c.UnsubscribeBaseURL,
		Opens:   campaign.TrackOpens,
		Clicks:  campaign.TrackClicks,
		Mint: func(kind, url string) (string, error) {
			return MintTrackingToken(c.UnsubscribeSecret, TrackingPayload{
				DeliveryID: deliveryID,
				CampaignID: campaign.ID,
				Kind:       kind,
				URL:        url,
			})
		},
	}
}
