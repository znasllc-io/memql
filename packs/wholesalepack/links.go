package wholesalepack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// links.go -- THE PRIVATE LINKS A STORE SENDS ONE APPLICANT (U8): the terms
// link, on which they read the store's current terms and MAP policy and accept
// them (acceptance.go), and the price-sheet link, on which an approved buyer
// reads the current price sheet.
//
// # The token is the authority, and it is never kept
//
// A link is 32 random bytes, base64url, minted by mintLink and answered to its
// one caller (the resolver that mails it); the row holds only its SHA-256. The
// three link builtins carry no capability, as the reviews pack's do: whoever
// holds the token is the applicant, and is nobody to MemQL. So each does
// nothing and reveals nothing until linkFor has found a link of THE STORE IT
// WAS CALLED WITH carrying that token's hash, and a token of another store is
// refused in exactly the words an unknown one is.
//
// # What is read as whom
//
// The token lookup runs under the caller: on the shopper path, the site owner's
// borrowed authority, which is the link's merchant (reviewspack's
// reviewRequestByTokenHash runs the same way); anybody else reads no link.
// Once a link is found the caller has been admitted by its token, and the
// application, its decisions, its entitlement, the settings and the documents
// are read as the deployment.
//
// # One row per application and purpose
//
// A link sent again is a new version of the same row with a new token hash, so
// the token before it matches nothing: a replaced link is link_invalid.

// The two things a link opens.
const (
	LinkPurposeTerms      = "terms"
	LinkPurposePriceSheet = "priceSheet"
)

// The refusals a link answers by name.
const (
	RefusalLinkInvalid = "link_invalid"
	RefusalLinkExpired = "link_expired"
	RefusalLinkUsed    = "link_used"
	// RefusalTermsChanged refuses an acceptance whose page showed a version that
	// is no longer current (ruling 49).
	RefusalTermsChanged = "terms_changed"
)

// LinkRefusals is every code the two link reads may refuse with.
var LinkRefusals = []string{RefusalLinkExpired, RefusalLinkUsed, RefusalLinkInvalid}

// AcceptanceRefusals is every code the acceptance form may refuse with: the
// link's, and a version that changed under the page.
var AcceptanceRefusals = []string{RefusalLinkExpired, RefusalLinkUsed, RefusalLinkInvalid, RefusalTermsChanged}

// refusalInvalid is the surface's own invalid, which a construct may answer for
// a field that breaks a rule its declaration cannot state.
const refusalInvalid = "invalid"

// linksActor is the system actor a link's subject is read as once its token
// admitted the caller.
const linksActor = "wholesale-links"

// LinkRefusal is a refusal of a link by name. It unwraps to the
// memql.ShopperRefusal the shopper handler forwards on ?reason=.
type LinkRefusal struct {
	Code string
	why  string
}

func (e *LinkRefusal) Error() string { return "wholesale: refused (" + e.Code + "): " + e.why }

// Unwrap is the refusal as the shopper surface reads it.
func (e *LinkRefusal) Unwrap() error { return memql.NewShopperRefusal(e.Code, e.why) }

func refuseLink(code, why string) error { return &LinkRefusal{Code: code, why: why} }

// errNoSuchLink is the ONE refusal of a token that finds no link of this store,
// whatever the reason: unknown, replaced, of another store, or unreadable to
// the caller. One value, so none of them can be told from another.
var errNoSuchLink = refuseLink(RefusalLinkInvalid, "no link of this store carries this token")

// PrivateLinkRowID derives the one link row id of an application and purpose,
// from the application's bare id whichever spelling the caller named.
func PrivateLinkRowID(purpose, applicationID string) string {
	return "wholesale-link-" + slugOf(purpose) + "-" + slugOf(bareID(strings.TrimSpace(applicationID)))
}

// mintToken makes one link token and the hash that is stored for it.
// A deliberate copy of packs/reviewspack/request.go's mintToken.
func mintToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("wholesale: minting a link token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	// The stored form of a token: its hex SHA-256 (sha256Hex).
	return token, sha256Hex(token), nil
}

// validPurpose refuses a purpose outside the two.
func validPurpose(purpose string) error {
	if purpose != LinkPurposeTerms && purpose != LinkPurposePriceSheet {
		return fmt.Errorf("wholesale: %q is not a link purpose; a link is terms or priceSheet", purpose)
	}
	return nil
}

// mintLink writes a new version of app's link of purpose with a new token, and
// answers the token: the only place it exists. A terms link expires the store's
// linkLifetimeDays after now; a price-sheet link does not. Called only by the
// resolver that mails the link, which has decided the application is owed one.
func (p *Provider) mintLink(ctx context.Context, app map[string]any, purpose string, now time.Time) (string, time.Time, error) {
	if err := validPurpose(purpose); err != nil {
		return "", time.Time{}, err
	}
	applicationID := trimmed(app["id"])
	storeID := trimmed(app["storeId"])
	if applicationID == "" || storeID == "" {
		return "", time.Time{}, fmt.Errorf("wholesale: a link is minted for an application with an id and a store")
	}
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	if purpose == LinkPurposeTerms {
		settings, err := p.settingsRowAt(auth.ContextWithSystemActor(ctx, linksActor), storeID)
		if err != nil {
			return "", time.Time{}, err
		}
		expires = now.UTC().Add(time.Duration(EffectiveSettings(settings).LinkLifetimeDays) * 24 * time.Hour)
	}
	token, hash, err := mintToken()
	if err != nil {
		return "", time.Time{}, err
	}
	link := map[string]any{
		"ownerUserId":   userRef(trimmed(app["ownerUserId"])),
		"storeId":       bareID(storeID),
		"accountId":     trimmed(app["accountId"]),
		"siteId":        trimmed(app["siteId"]),
		"applicationId": applicationRef(applicationID),
		"purpose":       purpose,
		"tokenHash":     hash,
		"issuedAt":      now.UTC().Format(time.RFC3339Nano),
		"expiresAt":     "",
		"usedAt":        "",
	}
	if !expires.IsZero() {
		link["expiresAt"] = expires.Format(time.RFC3339Nano)
	}
	if err := p.putLink(merchant, PrivateLinkRowID(purpose, applicationID), link); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// putLink writes one version of a link at rowID with EVERY field of link, the
// empty ones included: a write at an existing id merges into its current
// version, so an omitted usedAt would carry the version before it.
func (p *Provider) putLink(ctx context.Context, rowID string, link map[string]any) error {
	args := map[string]any{"linkId": rowID}
	for _, k := range []string{"ownerUserId", "storeId", "accountId", "siteId", "applicationId", "purpose", "tokenHash",
		"issuedAt", "expiresAt", "usedAt"} {
		args[k] = asString(link[k])
	}
	return p.write(ctx, "putPrivateLinkRow", args)
}

// linkFor verifies token against a link of purpose on storeID at now, and
// answers the link (without its hash) and its application.
//
// NOTHING IS READ BEFORE THE TOKEN MATCHES A LINK OF THIS STORE, and every miss
// is errNoSuchLink. Then: a terms link that was used, or whose application is
// no longer submitted, is link_used, and one at or past its expiry is
// link_expired; a price-sheet link is link_used unless its application is
// approved and its entitlement granted. An application that cannot be read is
// link_invalid to the shopper and one line for the operator.
func (p *Provider) linkFor(ctx context.Context, storeID, token, purpose string, now time.Time) (map[string]any, map[string]any, error) {
	token, storeID = strings.TrimSpace(token), strings.TrimSpace(storeID)
	if token == "" || storeID == "" {
		return nil, nil, errNoSuchLink
	}
	rows, err := p.rowsFor(ctx, "privateLinkByTokenHash", map[string]string{"storeId": bareID(storeID), "tokenHash": sha256Hex(token)})
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, errNoSuchLink
	}
	link := rows[0]
	if trimmed(link["purpose"]) != purpose || bareID(trimmed(link["storeId"])) != bareID(storeID) {
		return nil, nil, errNoSuchLink
	}
	applicationID := trimmed(link["applicationId"])
	sys := auth.ContextWithSystemActor(ctx, linksActor)
	apps, err := p.rowsFor(sys, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, nil, err
	}
	if len(apps) == 0 || bareID(trimmed(apps[0]["storeId"])) != bareID(storeID) {
		return nil, nil, p.linkProblem(applicationID, "the link's application cannot be read on this store")
	}
	app := apps[0]
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return nil, nil, err
	}
	switch purpose {
	case LinkPurposeTerms:
		if !rowTime(link["usedAt"]).IsZero() {
			return nil, nil, refuseLink(RefusalLinkUsed, "the terms were accepted through this link")
		}
		// AN ACCEPTANCE EXISTS, whatever usedAt says: the usedAt write after the
		// acceptance may have failed, and the link is used all the same.
		accepted, err := p.acceptanceOf(sys, applicationID)
		if err != nil {
			return nil, nil, err
		}
		if accepted != nil {
			return nil, nil, refuseLink(RefusalLinkUsed, "the application has accepted the terms")
		}
		if state != StateSubmitted {
			return nil, nil, refuseLink(RefusalLinkUsed, "the application is "+state)
		}
		if expires := rowTime(link["expiresAt"]); expires.IsZero() || !now.Before(expires) {
			return nil, nil, refuseLink(RefusalLinkExpired, "the link expired at "+timeText(link["expiresAt"]))
		}
	case LinkPurposePriceSheet:
		if state != StateApproved {
			return nil, nil, refuseLink(RefusalLinkUsed, "the application is "+state)
		}
		ent, err := p.entitlementRow(sys, applicationID)
		if err != nil {
			return nil, nil, err
		}
		if ent == nil || trimmed(ent["state"]) != EntitlementGranted {
			return nil, nil, refuseLink(RefusalLinkUsed, "the buyer's access is not granted")
		}
	}
	return link, app, nil
}

// linkProblem is a link the shopper is told is invalid and the operator is told
// why, by the application's id. Never the token.
func (p *Provider) linkProblem(applicationID, reason string) error {
	p.log().Warn("wholesale: a private link could not be served",
		"component", "wholesale", "applicationId", applicationID, "reason", reason)
	return refuseLink(RefusalLinkInvalid, reason)
}

// log is the pack's logger, or the default one.
func (p *Provider) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// termsForLink answers wholesaleTermsForLink: the current terms and MAP policy.
func (p *Provider) termsForLink(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	link, app, err := p.linkFor(ctx, storeID, asString(args["requestToken"]), LinkPurposeTerms, p.clock())
	if err != nil {
		return nil, err
	}
	sys := auth.ContextWithSystemActor(ctx, linksActor)
	// THE TWO BODIES SHOWN AND NO OTHER: the heads of the terms and the MAP
	// policy, then each one's body by id. Nothing of the price sheet.
	shown := make([]map[string]any, 0, 2)
	for _, kind := range []string{DocumentTerms, DocumentMap} {
		brief, err := p.currentBrief(sys, storeID, kind)
		if err != nil {
			return nil, err
		}
		if brief == nil {
			return nil, p.linkProblem(trimmed(app["id"]), "the store has no current "+kind+" version")
		}
		v, err := p.documentVersionByID(sys, brief)
		if err != nil {
			return nil, err
		}
		shown = append(shown, map[string]any{"kind": kind, "label": asString(v["label"]), "body": asString(v["body"]),
			"sha256": trimmed(v["sha256"])})
	}
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	statement := EffectiveSettings(settings).AcceptanceText
	return confirm("wholesale:terms", map[string]any{
		"companyName": asString(app["companyName"]),
		"documents":   shown,
		"statement":   statement,
		// ruling 52: the page posts this back, so the acceptance names the
		// sentence that was shown.
		"statementSha256": sha256Hex(statement),
		"expiresAt":       timeText(link["expiresAt"]),
	})
}

// priceSheetForLink answers wholesalePriceSheetForLink: the current price sheet.
func (p *Provider) priceSheetForLink(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	_, app, err := p.linkFor(ctx, storeID, asString(args["requestToken"]), LinkPurposePriceSheet, p.clock())
	if err != nil {
		return nil, err
	}
	sys := auth.ContextWithSystemActor(ctx, linksActor)
	brief, err := p.currentBrief(sys, storeID, DocumentPriceSheet)
	if err != nil {
		return nil, err
	}
	if brief == nil {
		return nil, p.linkProblem(trimmed(app["id"]), "the store has no current price sheet")
	}
	v, err := p.documentVersionByID(sys, brief)
	if err != nil {
		return nil, err
	}
	return confirm("wholesale:priceSheet", map[string]any{
		"companyName": asString(app["companyName"]),
		"document": map[string]any{"label": asString(v["label"]), "body": asString(v["body"]),
			"sha256": trimmed(v["sha256"]), "publishedAt": timeText(v["publishedAt"])},
	})
}

// termsLinkView is an application's terms link as wholesaleGet shows it,
// {expiresAt, expired}, or nil when it has none. Read as the deployment.
func (p *Provider) termsLinkView(sys context.Context, applicationID string, now time.Time) (any, error) {
	link, err := p.linkOf(sys, applicationID, LinkPurposeTerms)
	if err != nil || link == nil {
		return nil, err
	}
	expires := rowTime(link["expiresAt"])
	return map[string]any{"expiresAt": timeText(link["expiresAt"]), "expired": expires.IsZero() || !now.Before(expires)}, nil
}

// linkOf is an application's current link of purpose, either spelling, or nil.
func (p *Provider) linkOf(ctx context.Context, applicationID, purpose string) (map[string]any, error) {
	seen := map[string]bool{}
	var newest map[string]any
	for _, form := range uniqueForms(bareID(strings.TrimSpace(applicationID)), applicationRef(applicationID)) {
		rows, err := p.rowsFor(ctx, "privateLinkForApplication", map[string]string{"applicationId": form, "purpose": purpose})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id := trimmed(row["id"])
			if seen[id] {
				continue
			}
			seen[id] = true
			if newest == nil || rowTime(row["createdAt"]).After(rowTime(newest["createdAt"])) {
				newest = row
			}
		}
	}
	return newest, nil
}
