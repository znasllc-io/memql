package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Campaigns is core. This is a declaration on the existing shopper-form
// carrier, not a second public API or a pack shadowing the campaigns domain.
// Both the deployable switch and its explicit newsletter binding start off.
func init() {
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "campaigns", Name: "subscribe", Construct: "campaignSubscribe", Kind: memql.ShopperReadKindBuiltin,
		Description: "Opt in to this deployable's organization newsletter.",
		Fields: []memql.ShopperField{
			{Name: "email", Required: true, MaxLength: 320},
			{Name: "displayName", MaxLength: 120},
			{Name: "consent", Required: true, MaxLength: 3, Enum: []string{"yes"}},
			{Name: "consentRevision", Required: true, MaxLength: 64},
		},
		RedirectOK: "/newsletter/thank-you", RedirectError: "/newsletter/problem",
	})
}

func newsletterID(site string) string { return sha256Hex("newsletter-site\x00" + bare(site)) }
func newsletterSignupID(audience, address string) string {
	return sha256Hex("newsletter-signup\x00" + bare(audience) + "\x00" + NormalizeEmail(address))
}

func (w *Worker) newsletterRow(ctx context.Context, query, key, value string) (map[string]any, error) {
	rows, err := w.store.rows(memql.ContextWithFreshRead(ctx), call("query", query, arg{key, value}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (w *Worker) newsletterWrite(ctx context.Context, name string, values map[string]any) error {
	statement, err := langparser.RenderCall(name, values)
	if err != nil {
		return err
	}
	return w.store.execServerOnly(ctx, "mutation "+statement)
}

func (w *Worker) handleConfigureNewsletter(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	siteID := bare(argString(args, "siteId"))
	if !templateIDPattern.MatchString(siteID) || callerUserID(ctx) == "" || w.newsletterGate == nil {
		return nil, fmt.Errorf("select a deployable while connected to the cluster")
	}
	release, err := w.newsletterGate(ctx, "site:"+siteID)
	if err != nil {
		return nil, err
	}
	defer release()
	site, err := w.newsletterRow(ctx, "siteById", "siteId", siteID)
	if err != nil {
		return nil, err
	}
	if site == nil || str(site, "accountId") == "" || str(site, "ownerUserId") == "" || booleanOr(site, "systemOwned", false) {
		return nil, fmt.Errorf("choose an organization-owned deployable")
	}
	account := bare(str(site, "accountId"))
	if err = w.requireSendAuthority(ctx, account); err != nil {
		return nil, err
	}
	if !w.store.engine.OrganizationCapable(ctx, account, auth.VerbRead, "app:deployables") {
		return nil, fmt.Errorf("newsletter setup requires permission to update this deployable")
	}
	prior, err := w.newsletterRow(ctx, "newsletterForSite", "siteId", siteID)
	if err != nil {
		return nil, err
	}
	enabled, ok := args["enabled"].(bool)
	if !ok {
		return nil, fmt.Errorf("choose whether to accept newsletter signups")
	}
	values := map[string]any{
		"newsletterId": newsletterID(siteID), "siteId": siteID, "accountId": account,
		"audienceId": bare(argString(args, "audienceId")), "templateId": bare(argString(args, "templateId")),
		"senderIdentityId": bare(argString(args, "senderIdentityId")), "enabled": enabled,
		"consentText": strings.TrimSpace(argString(args, "consentText")),
		"storeId":     bare(str(objectField(site, "binding"), "storeId")),
	}
	if len(str(values, "consentText")) < 10 || len(str(values, "consentText")) > 1000 || strings.IndexFunc(str(values, "consentText"), unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("write a short consent sentence for the signup form (10–1000 characters)")
	}
	if prior != nil && !sameOrganization(str(prior, "accountId"), account) {
		return nil, fmt.Errorf("a newsletter cannot change organizations")
	}
	// Pausing the existing binding must remain possible after a referenced
	// template or sender is no longer sendable. Changed references still need
	// validation, even when the new binding will be disabled.
	unchangedReferences := prior != nil
	for _, key := range []string{"audienceId", "templateId", "senderIdentityId"} {
		unchangedReferences = unchangedReferences && bare(str(prior, key)) == bare(str(values, key))
	}
	if enabled || !unchangedReferences {
		if _, err = w.newsletterPreflight(ctx, values, enabled); err != nil {
			return nil, err
		}
	}
	if prior != nil && sameNewsletter(prior, values) {
		if enabled && !booleanOr(site, "shopperForms", false) {
			if err = w.store.exec(ctx, call("mutation", "updateSiteShopperForms", arg{"siteId", siteID}, arg{"shopperForms", true})); err != nil {
				return nil, err
			}
		}
		return newsletterReceipt(prior)
	}
	if prior == nil && argString(args, "expectedRevision") != "" || prior != nil && !templateTime(prior["createdAt"]).Equal(templateTime(args["expectedRevision"])) {
		return nil, fmt.Errorf("this newsletter changed; reload before saving")
	}
	// Turning on the shared public-form carrier is an explicit part of Enable.
	// Disabling this newsletter leaves other declared forms on the site alone.
	if enabled && !booleanOr(site, "shopperForms", false) {
		if err = w.store.exec(ctx, call("mutation", "updateSiteShopperForms", arg{"siteId", siteID}, arg{"shopperForms", true})); err != nil {
			return nil, err
		}
	}
	values["versionTime"] = memql.VersionTimeAfter(templateTime(prior["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)
	if err = w.newsletterWrite(ctx, "configureNewsletter", values); err != nil {
		return nil, err
	}
	values["id"], values["createdAt"] = values["newsletterId"], values["versionTime"]
	return newsletterReceipt(values)
}

func sameNewsletter(row, values map[string]any) bool {
	for _, key := range []string{"siteId", "accountId", "audienceId", "templateId", "senderIdentityId", "storeId"} {
		if bare(str(row, key)) != bare(str(values, key)) {
			return false
		}
	}
	return row["enabled"] == values["enabled"] && str(row, "consentText") == str(values, "consentText")
}

func newsletterReceipt(row map[string]any) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignNewsletterSaved", map[string]any{"saved": true, "newsletterId": bare(str(row, "id")), "revision": templateTime(row["createdAt"]).Format(time.RFC3339Nano), "enabled": row["enabled"]})
}

func (w *Worker) newsletterPreflight(ctx context.Context, row map[string]any, sending bool) (Template, error) {
	campaign := Campaign{AccountID: bare(str(row, "accountId")), AudienceID: bare(str(row, "audienceId")), TemplateID: bare(str(row, "templateId")), SenderIdentityID: bare(str(row, "senderIdentityId"))}
	if campaign.SenderIdentityID == "" {
		return Template{}, fmt.Errorf("select this organization's sender")
	}
	if sending {
		_, snapshot, err := w.preflightAudience(ctx, "newsletter", campaign, true)
		return snapshot, err
	}
	template, found, err := w.store.TemplateByID(ctx, campaign.TemplateID)
	if err != nil {
		return Template{}, err
	}
	if !found {
		return Template{}, fmt.Errorf("select a readable welcome template")
	}
	return template, w.validateCampaignOrganization(ctx, campaign, template)
}

// The browser supplies only personal details and an affirmative choice. The
// carrier stamps site/store/owner; this capability re-reads their binding.
func (w *Worker) handleSubscribe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	address := NormalizeEmail(argString(args, "email"))
	name := strings.TrimSpace(argString(args, "displayName"))
	if !plausibleAddress(address) || len(address) > 320 || len(name) > 120 || strings.IndexFunc(name, unicode.IsControl) >= 0 || argString(args, "consent") != "yes" {
		return nil, fmt.Errorf("enter an email address and agree to receive the newsletter")
	}
	siteID := bare(argString(args, "siteId"))
	if w.newsletterGate == nil {
		return nil, fmt.Errorf("signup coordination is unavailable")
	}
	release, err := w.newsletterGate(ctx, "site:"+siteID)
	if err != nil {
		return nil, err
	}
	defer release()
	row, err := w.newsletterRow(ctx, "newsletterForSite", "siteId", siteID)
	if err != nil {
		return nil, err
	}
	site, err := w.newsletterRow(ctx, "siteById", "siteId", siteID)
	if err != nil {
		return nil, err
	}
	if row == nil || site == nil || !booleanOr(row, "enabled", false) || !booleanOr(site, "shopperForms", false) || str(site, "status") == "disabled" || bare(str(site, "ownerUserId")) != callerUserID(ctx) || !sameOrganization(str(row, "accountId"), str(site, "accountId")) || bare(argString(args, "storeId")) != bare(str(row, "storeId")) || bare(str(objectField(site, "binding"), "storeId")) != bare(str(row, "storeId")) {
		return nil, fmt.Errorf("newsletter signups are unavailable for this deployable")
	}
	if !templateTime(row["createdAt"]).Equal(templateTime(args["consentRevision"])) {
		return nil, fmt.Errorf("the signup form changed; refresh it before subscribing")
	}
	if err = w.requireSendAuthority(ctx, str(row, "accountId")); err != nil {
		return nil, err
	}
	audienceID := bare(str(row, "audienceId"))
	// All forms pointing at one audience share the lock and one welcome key.
	releaseAudience, err := w.newsletterGate(ctx, "audience:"+audienceID)
	if err != nil {
		return nil, err
	}
	defer releaseAudience()
	signupID := newsletterSignupID(audienceID, address)
	existing, err := w.newsletterRow(ctx, "newsletterSignupById", "signupId", signupID)
	if err != nil {
		return nil, err
	}
	accepted := func() ([]memorynodes.MemoryNode, error) {
		return resultNode("campaignSubscription", map[string]any{"accepted": true})
	}
	if existing != nil {
		return accepted()
	}
	// Repeated opt-ins never clear any existing suppression or recipient state.
	if _, suppressed, err := w.store.SuppressionForSend(w.systemActorContext(ctx), str(row, "accountId"), EmailDigest(address)); err != nil {
		return nil, err
	} else if suppressed {
		return accepted()
	}
	roster, err := w.store.Roster(ctx, audienceID)
	if err != nil {
		return nil, err
	}
	var recipient *Recipient
	for _, r := range roster {
		if NormalizeEmail(r.Email) == address {
			if r.SubscriptionStatus != "" && r.SubscriptionStatus != "subscribed" {
				return accepted()
			}
			if recipient == nil {
				copy := r
				recipient = &copy
			}
		}
	}
	if recipient == nil && len(roster) >= w.cfg.MaxAudience {
		return nil, fmt.Errorf("this newsletter cannot accept more subscribers right now")
	}
	snapshot, err := w.newsletterPreflight(ctx, row, true)
	if err != nil {
		return nil, err
	}
	// Consent lands first, by address digest. If enrollment fails afterwards,
	// no unconsented recipient can become sendable. Its stable event key is
	// checked on retry, so replay cannot turn an old grant into a new grant.
	eventID := sha256Hex("newsletter-consent\x00" + signupID)
	consent, err := w.newsletterRow(ctx, "newsletterConsentById", "eventId", eventID)
	if err != nil {
		return nil, err
	}
	requestedAt := w.nowUTC()
	if consent == nil {
		if err = w.store.RecordConsent(ctx, ConsentGrant, ConsentRecord{EventID: eventID, EmailDigest: EmailDigest(address), Source: "signup", AccountID: bare(str(row, "accountId")), OccurredAt: requestedAt}); err != nil {
			return nil, err
		}
	} else {
		requestedAt = templateTime(consent["occurredAt"])
	}
	if recipient == nil {
		recipientID := sha256Hex("newsletter-recipient\x00" + signupID)
		if err = w.store.AddRecipient(ctx, recipientID, audienceID, address, name, "signup", bare(str(row, "accountId")), nil); err != nil {
			return nil, err
		}
		recipient = &Recipient{ID: recipientID}
	}
	encodedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var frozen map[string]any
	if err = json.Unmarshal(encodedSnapshot, &frozen); err != nil {
		return nil, err
	}
	values := map[string]any{
		"signupId": signupID, "newsletterId": bare(str(row, "id")), "siteId": siteID, "storeId": bare(str(row, "storeId")),
		"accountId": bare(str(row, "accountId")), "audienceId": audienceID, "recipientId": bare(recipient.ID),
		"emailDigest": EmailDigest(address), "email": address, "displayName": name, "submissionId": argString(args, "submissionId"), "consentText": str(row, "consentText"),
		"consentRevision": templateTime(row["createdAt"]).Format(time.RFC3339Nano),
		"requestedAt":     requestedAt.Format(time.RFC3339Nano), "templateId": snapshot.ID, "templateSnapshot": frozen, "senderIdentityId": bare(str(row, "senderIdentityId")),
	}
	if err = w.newsletterWrite(ctx, "recordNewsletterSignup", values); err != nil {
		return nil, err
	}
	return accepted()
}

func (w *Worker) drainNewsletterWelcomes(ctx, systemCtx context.Context) {
	if w.newsletterGate == nil {
		return
	}
	systemCtx = memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(systemCtx))
	cursor := ""
	for {
		rows, next, err := w.store.rowsPage(systemCtx, cursor, call("query", "pendingNewsletterWelcomes"))
		if err != nil {
			w.logger.Debug("campaigns: newsletter scan unavailable", "error", err)
			return
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			if err = w.sendNewsletterWelcome(ctx, systemCtx, row); err != nil {
				w.logger.Error("campaigns: newsletter progress not saved", "signup", bare(str(row, "id")), "error", err)
			}
		}
		if next == "" || next == cursor {
			return
		}
		cursor = next
	}
}

func (w *Worker) sendNewsletterWelcome(ctx, systemCtx context.Context, scanned map[string]any) error {
	releaseSite, err := w.newsletterGate(ctx, "site:"+bare(str(scanned, "siteId")))
	if err != nil {
		return err
	}
	defer releaseSite()
	release, err := w.newsletterGate(ctx, "audience:"+bare(str(scanned, "audienceId")))
	if err != nil {
		return err
	}
	defer release()
	systemCtx = memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(systemCtx))
	row, err := w.newsletterRow(systemCtx, "newsletterSignupById", "signupId", bare(str(scanned, "id")))
	if err != nil || row == nil || str(row, "status") != "pending" {
		return err
	}
	owner := memql.ContextWithFreshRead(w.ownerActorContext(ctx, bare(str(row, "ownerUserId"))))
	status, reason := "blocked", "The newsletter or its deployable is no longer enabled."
	config, readErr := w.newsletterRow(owner, "newsletterForSite", "siteId", bare(str(row, "siteId")))
	site, siteErr := w.newsletterRow(owner, "siteById", "siteId", bare(str(row, "siteId")))
	if readErr == nil && siteErr == nil && config != nil && site != nil && booleanOr(config, "enabled", false) && booleanOr(site, "shopperForms", false) && str(site, "status") != "disabled" && bare(str(site, "ownerUserId")) == callerUserID(owner) && sameOrganization(str(site, "accountId"), str(row, "accountId")) && sameOrganization(str(config, "accountId"), str(row, "accountId")) && bare(str(objectField(site, "binding"), "storeId")) == bare(str(row, "storeId")) {
		var snapshot Template
		encoded, _ := json.Marshal(objectField(row, "templateSnapshot"))
		if json.Unmarshal(encoded, &snapshot) != nil || snapshot.ID == "" {
			reason = "The reviewed welcome template could not be read."
		} else {
			nodes, sendErr := w.sendToRecipientOnce(owner, map[string]any{"requestId": "newsletter-" + bare(str(row, "id")), "templateId": bare(str(row, "templateId")), "recipientId": bare(str(row, "recipientId")), "senderIdentityId": bare(str(row, "senderIdentityId"))}, &snapshot, str(row, "emailDigest"))
			// No provider attempt has begun. Leave the unchanged row pending
			// for the next drain; a rate ceiling must not lose a welcome.
			if errors.Is(sendErr, errCampaignRateLimited) {
				return nil
			}
			if sendErr != nil {
				reason = sendErr.Error()
			} else if len(nodes) == 1 {
				var result map[string]any
				if json.Unmarshal(nodes[0].Payload, &result) == nil {
					switch {
					case result["sent"] == true:
						status, reason = "sent", ""
					case result["uncertain"] == true:
						status, reason = "uncertain", str(result, "reason")
					case result["skipped"] == true:
						status, reason = "suppressed", str(result, "reason")
					}
				}
			}
		}
	}
	return w.newsletterWrite(systemCtx, "updateNewsletterWelcome", map[string]any{"signupId": bare(str(row, "id")), "status": status, "lastError": truncateError(reason), "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
}

func (w *Worker) handleRetryNewsletterWelcome(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	row, err := w.newsletterRow(ctx, "newsletterSignupById", "signupId", bare(argString(args, "signupId")))
	if err != nil {
		return nil, err
	}
	if row == nil || w.newsletterGate == nil {
		return nil, fmt.Errorf("welcome message is unavailable")
	}
	if err = w.requireSendAuthority(ctx, str(row, "accountId")); err != nil {
		return nil, err
	}
	release, err := w.newsletterGate(ctx, "audience:"+bare(str(row, "audienceId")))
	if err != nil {
		return nil, err
	}
	defer release()
	row, err = w.newsletterRow(ctx, "newsletterSignupById", "signupId", bare(argString(args, "signupId")))
	if err != nil {
		return nil, err
	}
	if row == nil || str(row, "status") != "blocked" || !templateTime(row["createdAt"]).Equal(templateTime(args["expectedRevision"])) {
		return nil, fmt.Errorf("only the current blocked welcome can be checked again")
	}
	err = w.newsletterWrite(ctx, "updateNewsletterWelcome", map[string]any{"signupId": bare(str(row, "id")), "status": "pending", "lastError": "", "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
	if err != nil {
		return nil, err
	}
	return resultNode("campaignNewsletterRetry", map[string]any{"queued": true})
}
