package wholesalepack

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// acceptance.go -- AN APPLICANT ACCEPTS THE STORE'S TERMS through their terms
// link, and the acceptance records its evidence (U8).
//
// # What is recorded is what the person was shown (ruling 49)
//
// The page posts the fingerprints of the terms and MAP versions the terms read
// answered. The accept reads the current versions again, and a fingerprint that
// is not the current version's refuses the post as terms_changed: nothing is
// written and the link stays usable, so the applicant reads the current texts
// and accepts those. So the versions an acceptance names are always the ones on
// the page.
//
// # Written once
//
// The acceptance is written at the derived id AcceptanceRowID, from one
// spelling of the application id, fenced on that row being absent. Of two posts
// at once both may pass the acceptance read; the fence lets one write and the
// other is told link_used. Then the link is marked used -- not under the fence,
// which would refuse the link's own write once the acceptance exists.
//
// # The evidence comes from the shopper handler only
//
// The address the edge saw and the browser's User-Agent ride the context
// (memql.ShopperEvidenceFrom), which no argument can set: a direct call of the
// builtin records neither.

// NotAcceptedSentence refuses an approval while the store requires its terms
// and the application has not accepted them. Shown to the store's people as
// written.
const NotAcceptedSentence = "This application has not accepted the terms yet."

// acceptanceConcept is the acceptance's concept id, for its row fence.
const acceptanceConcept = "v1:wholesale:acceptance"

// The bounds of the evidence an acceptance keeps, in bytes.
const (
	maxClientAddressBytes = 64
	maxUserAgentBytes     = 512
)

// boxOn is the value a checked HTML checkbox posts.
const boxOn = "on"

// AcceptanceRowID derives the one acceptance row id of an application, from
// its bare id whichever spelling the caller named: the fence protects one id,
// and two spellings must not become two rows.
func AcceptanceRowID(applicationID string) string {
	return "wholesale-acceptance-" + slugOf(bareID(strings.TrimSpace(applicationID)))
}

// acceptTerms answers wholesaleAcceptTerms.
func (p *Provider) acceptTerms(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	now := p.clock()
	link, app, err := p.linkFor(ctx, storeID, asString(args["requestToken"]), LinkPurposeTerms, now)
	if err != nil {
		return nil, err
	}
	applicationID := trimmed(app["id"])
	sys := auth.ContextWithSystemActor(ctx, linksActor)
	prior, err := p.acceptanceOf(sys, applicationID)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		return nil, refuseLink(RefusalLinkUsed, "the application has accepted the terms")
	}
	if trimmed(args["acceptTerms"]) != boxOn || trimmed(args["acceptMap"]) != boxOn {
		return nil, memql.NewShopperRefusal(refusalInvalid, "both boxes must be on")
	}
	name := trimmed(args["signedName"])
	if name == "" {
		return nil, memql.NewShopperRefusal(refusalInvalid, "the name typed is empty")
	}
	postedTerms, postedMap := trimmed(args["termsSha256"]), trimmed(args["mapSha256"])
	if !isFingerprint(postedTerms) || !isFingerprint(postedMap) {
		return nil, memql.NewShopperRefusal(refusalInvalid, "a fingerprint is not 64 hex characters")
	}
	// THE CURRENT VERSIONS, read again: what the acceptance names.
	current, err := p.currentBriefs(sys, storeID)
	if err != nil {
		return nil, err
	}
	terms, mapv := current[DocumentTerms], current[DocumentMap]
	if terms == nil || mapv == nil {
		return nil, p.linkProblem(applicationID, "the store has no current terms or MAP version")
	}
	if !strings.EqualFold(postedTerms, trimmed(terms["sha256"])) || !strings.EqualFold(postedMap, trimmed(mapv["sha256"])) {
		return nil, refuseLink(RefusalTermsChanged, "a version the page showed is no longer current")
	}
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}
	ev, _ := memql.ShopperEvidenceFrom(ctx)
	fenced := memql.ContextWithRowVersionFence(merchant, acceptanceConcept, AcceptanceRowID(applicationID), time.Time{})
	if err := p.write(fenced, "putAcceptanceRow", map[string]any{
		"acceptanceId":   AcceptanceRowID(applicationID),
		"ownerUserId":    userRef(trimmed(app["ownerUserId"])),
		"storeId":        bareID(trimmed(app["storeId"])),
		"accountId":      trimmed(app["accountId"]),
		"applicationId":  applicationRef(applicationID),
		"applicantEmail": trimmed(app["applicantEmail"]),
		"signedName":     name,
		"acceptedAt":     now.Format(time.RFC3339Nano),
		"termsVersionId": trimmed(terms["id"]),
		"termsSha256":    trimmed(terms["sha256"]),
		"mapVersionId":   trimmed(mapv["id"]),
		"mapSha256":      trimmed(mapv["sha256"]),
		"statement":      EffectiveSettings(settings).AcceptanceText,
		"clientAddress":  cutUTF8(ev.ClientAddress, maxClientAddressBytes),
		"userAgent":      cutUTF8(ev.UserAgent, maxUserAgentBytes),
		"linkIssuedAt":   timeText(link["issuedAt"]),
	}); err != nil {
		// THE FENCE'S LOSER: the other post was recorded.
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return nil, refuseLink(RefusalLinkUsed, "the application has accepted the terms")
		}
		return nil, err
	}
	used := map[string]any{}
	for k, v := range link {
		used[k] = v
	}
	// The lookup projects no hash; the token in hand is the link's.
	used["tokenHash"] = tokenHashOf(strings.TrimSpace(asString(args["requestToken"])))
	used["issuedAt"], used["expiresAt"] = timeText(link["issuedAt"]), timeText(link["expiresAt"])
	used["usedAt"] = now.Format(time.RFC3339Nano)
	if err := p.putLink(merchant, bareID(trimmed(link["id"])), used); err != nil {
		return nil, err
	}
	return confirm("wholesale:terms:accepted", map[string]any{"accepted": true})
}

// acceptanceOf is an application's acceptance, either spelling, or nil.
func (p *Provider) acceptanceOf(ctx context.Context, applicationID string) (map[string]any, error) {
	rows, err := p.rowsForApplication(ctx, "acceptanceForApplication", applicationID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// isFingerprint reports whether s is 64 hex characters, either case.
func isFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// cutUTF8 is s cut to at most max bytes, at the start of the character the
// bound falls in.
func cutUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// acceptanceView is an application's acceptance as wholesaleGet shows it, with
// the labels of the versions it names, or nil when it has none. Read as the
// deployment.
func (p *Provider) acceptanceView(sys context.Context, acceptance map[string]any) (any, error) {
	if acceptance == nil {
		return nil, nil
	}
	label := func(versionID string) (string, error) {
		if versionID == "" {
			return "", nil
		}
		rows, err := p.rowsFor(sys, "wholesaleDocumentVersionById", map[string]string{"documentVersionId": versionID})
		if err != nil || len(rows) == 0 {
			return "", err
		}
		return asString(rows[0]["label"]), nil
	}
	termsLabel, err := label(trimmed(acceptance["termsVersionId"]))
	if err != nil {
		return nil, err
	}
	mapLabel, err := label(trimmed(acceptance["mapVersionId"]))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"signedName":    asString(acceptance["signedName"]),
		"acceptedAt":    timeText(acceptance["acceptedAt"]),
		"termsLabel":    termsLabel,
		"termsSha256":   trimmed(acceptance["termsSha256"]),
		"mapLabel":      mapLabel,
		"mapSha256":     trimmed(acceptance["mapSha256"]),
		"clientAddress": asString(acceptance["clientAddress"]),
		"userAgent":     asString(acceptance["userAgent"]),
	}, nil
}

// termsRequiredAt reports whether a store requires its terms accepted, read
// through sys.
func (p *Provider) termsRequiredAt(sys context.Context, storeID string) (bool, error) {
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return false, err
	}
	return EffectiveSettings(settings).TermsRequired, nil
}
