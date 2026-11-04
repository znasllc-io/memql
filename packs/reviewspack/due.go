package reviewspack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// due.go -- THE DAILY ASK (reviewsSendDueRequests).
//
// Once a day the store's own automation runs this. For every store whose
// requests are on, with a reviewed request template and its organization set,
// every paid order processed the store's delay ago (14 days unless it said
// otherwise) earns one link per paid product -- EligibleLines decides which
// -- and each is mailed to that order's buyer through the order mail route
// (campaignSendOrderMessage), which reads the address off the order itself,
// checks the store's suppression list and adds the one-click unsubscribe.
//
// Every buyer is asked: the job looks at nothing but whether an order was
// paid. A line already asked (sent, or skipped by the suppression list) is
// not asked again; one whose send failed or was deferred is, with a new link,
// since only the old link's hash was kept. The window reaches back a few days
// past the delay, so a day the job did not run is caught up rather than lost.
//
// It reads under a system identity (the orders are cluster-owner tier), and
// writes each request AS THE STORE'S MERCHANT, whose borrowed authority the
// shopper path reads it under. It answers only to the store's automation: a
// call without internal origin is refused before anything is read.

const (
	dueActor       = "reviews-daily-ask"
	dueCatchUpDays = 3
)

// orderMailer sends one message an order entitles.
type orderMailer interface {
	SendOrderMessage(ctx context.Context, args map[string]any) (status, reason string, err error)
}

// engineMailer is the production orderMailer: the campaigns order route.
type engineMailer struct {
	engine memql.IntegrationEngineAccess
}

func (m *engineMailer) SendOrderMessage(ctx context.Context, args map[string]any) (string, string, error) {
	if m == nil || m.engine == nil {
		return "", "", fmt.Errorf("reviews: the pack has no engine handle, so no request can be mailed")
	}
	result, err := m.engine.Execute(ctx, "builtin "+renderMutationCall("campaignSendOrderMessage", args))
	if err != nil {
		return "", "", err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return "", "", fmt.Errorf("reviews: the order mail route answered nothing")
	}
	row := rows[0]
	switch {
	case row["sent"] == true:
		return "sent", "", nil
	case row["skipped"] == true:
		return "skipped", asString(row["reason"]), nil
	case row["deferred"] == true:
		return "pending", asString(row["reason"]), nil
	}
	return "failed", "the order mail route's answer named no outcome", nil
}

func (p *Provider) sendDueRequests(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("reviews: reviewsSendDueRequests is run by the store's automation, not by a client")
	}
	only := strings.TrimSpace(asString(args["storeId"]))
	sys := auth.ContextWithSystemActor(ctx, dueActor)
	settingsRows, err := p.rowsFor(sys, "reviewSettingsAll", map[string]string{})
	if err != nil {
		return nil, err
	}
	now := p.clock()
	tally := map[string]int{"stores": 0, "considered": 0, "sent": 0, "skipped": 0, "pending": 0, "failed": 0, "ambiguous": 0}
	for _, settings := range settingsRows {
		eff := EffectiveSettings(settings)
		storeID := strings.TrimSpace(asString(settings["storeId"]))
		accountID := strings.TrimSpace(asString(settings["accountId"]))
		// A STORE ASKS ONLY WHEN IT CAN BE ASKED FOR PROPERLY: requests on, its
		// own wording, a sender an operator named, and its organization.
		if storeID == "" || (only != "" && storeID != only) || !eff.RequestsEnabled || eff.RequestTemplateID == "" ||
			eff.RequestSenderIdentityID == "" || accountID == "" {
			continue
		}
		site, ambiguous := p.servingSite(sys, storeID, accountID)
		if ambiguous {
			tally["ambiguous"]++
			continue
		}
		if site == nil {
			continue
		}
		tally["stores"]++
		to := now.Add(-time.Duration(eff.RequestDelayDays) * 24 * time.Hour)
		from := to.Add(-dueCatchUpDays * 24 * time.Hour)
		orders, err := p.rowsFor(sys, "reviewDueOrders", map[string]string{
			"storeId": storeID, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)})
		if err != nil {
			return nil, err
		}
		for _, order := range orders {
			p.askOrder(ctx, sys, site, settings, eff, order, now, tally)
		}
	}
	body := map[string]any{}
	for k, v := range tally {
		body[k] = v
	}
	return receipt("v1:reviews:asked", "asked-"+now.Format("2006-01-02"), body)
}

// askOrder issues and mails the links one order earns.
func (p *Provider) askOrder(ctx, sys context.Context, site, settings map[string]any, eff Settings, order map[string]any, now time.Time, tally map[string]int) {
	storeID := strings.TrimSpace(asString(settings["storeId"]))
	accountID := strings.TrimSpace(asString(settings["accountId"]))
	orderGid := strings.TrimSpace(asString(order["gid"]))
	lines, err := p.rowsFor(sys, "reviewOrderLines", map[string]string{"storeId": storeID, "orderGid": orderGid})
	if err != nil {
		tally["failed"]++
		return
	}
	titles := map[string]string{}
	handleOf := func(productGid string) string {
		rows, err := p.rowsFor(sys, "shopifyProductByGid", map[string]string{"storeId": storeID, "gid": productGid})
		if err != nil || len(rows) == 0 {
			return ""
		}
		h := strings.TrimSpace(asString(rows[0]["handle"]))
		titles[h] = strings.TrimSpace(asString(rows[0]["title"]))
		return h
	}
	merchant := auth.ContextWithUserActor(ctx, strings.TrimSpace(asString(site["ownerUserId"])))
	for _, l := range EligibleLines(order, lines, handleOf) {
		tally["considered"]++
		requestID := requestIDFor(storeID, orderGid, l.ProductHandle)
		if existing, err := p.rowsFor(sys, "reviewRequestById", map[string]string{"requestId": requestID}); err == nil && len(existing) > 0 {
			switch asString(existing[0]["sendState"]) {
			case "sent", "skipped":
				continue // asked already
			}
		}
		token, hash, err := mintToken()
		if err != nil {
			tally["failed"]++
			continue
		}
		expires := now.Add(time.Duration(eff.LinkLifetimeDays) * 24 * time.Hour)
		title := l.ProductTitle
		if t := titles[l.ProductHandle]; t != "" {
			title = t
		}
		row := map[string]any{
			"requestId":     requestID,
			"storeId":       storeID,
			"accountId":     accountID,
			"siteId":        strings.TrimSpace(asString(site["id"])),
			"orderGid":      orderGid,
			"productHandle": l.ProductHandle,
			"email":         strings.TrimSpace(asString(order["email"])),
			"tokenHash":     hash,
			"issuedAt":      now.Format(time.RFC3339Nano),
			"expiresAt":     expires.Format(time.RFC3339Nano),
			"sendState":     "pending",
		}
		setIfPresent(row, "orderName", strings.TrimSpace(asString(order["name"])))
		setIfPresent(row, "productTitle", title)
		if err := p.writer.Write(merchant, "putReviewRequest", row); err != nil {
			tally["failed"]++
			continue
		}
		fields := map[string]any{
			"productTitle": title,
			"reviewUrl":    linkURL(strings.TrimSpace(asString(site["hostname"])), l.ProductHandle, token),
			// THE LINK'S PARTS, for a template: a value may not choose where a
			// template's link goes, so the template writes the store's own
			// address and takes these in its query.
			"productHandle": l.ProductHandle,
			"reviewToken":   token,
			"expiresOn":     expires.Format("January 2, 2006"),
			"orderName":     strings.TrimSpace(asString(order["name"])),
		}
		if billing, _ := order["billingAddress"].(map[string]any); billing != nil {
			setIfPresentAny(fields, "firstName", strings.TrimSpace(asString(billing["firstName"])))
		}
		status, reason, err := p.mailer.SendOrderMessage(merchant, map[string]any{
			"storeId": storeID, "orderGid": orderGid, "accountId": accountID,
			"templateId": strings.TrimSpace(eff.RequestTemplateID), "requestId": requestID, "fields": fields,
			// The sender the store's operator named, which may be the
			// installation's own: naming it is an operator's act (settings.go),
			// so the job may say it was chosen.
			"senderIdentityId": eff.RequestSenderIdentityID, "installationSender": true,
		})
		// THE OUTCOME IS A NEW VERSION OF THE SAME ROW, written from its own
		// copy: the pending version above stays what it was.
		done := make(map[string]any, len(row)+2)
		for k, v := range row {
			done[k] = v
		}
		switch {
		case err != nil:
			done["sendState"], done["sendNote"] = "failed", clip(err.Error(), 1000)
		case status == "sent":
			done["sendState"], done["sentAt"] = "sent", p.clock().Format(time.RFC3339Nano)
		default:
			done["sendState"] = status
			setIfPresent(done, "sendNote", clip(reason, 1000))
		}
		tally[asString(done["sendState"])]++
		_ = p.writer.Write(merchant, "putReviewRequest", done)
	}
}

// servingSite is the ONE live site of the store's organization that serves
// the store: its address is the link's, and its owner writes the requests.
//
// It is chosen strictly, because both of those are authority. The serving
// binding must name the store (a preview binding never makes a store a
// site's), the site must be the organization's and live, and an id must be
// of the right concept in either spelling -- a slug that only looks like the
// store's or the organization's is not theirs. Two live sites serving one
// store is a choice this job cannot make, so it answers ambiguous and the
// store is skipped until one remains.
func (p *Provider) servingSite(sys context.Context, storeID, accountID string) (map[string]any, bool) {
	bare := bareAccount(storeID)
	rows, err := p.reader.RowsWithList(sys, "sitesBoundToStore", map[string]string{}, "storeIds", []string{storeID, bare, storePrefix + bare})
	if err != nil {
		return nil, false
	}
	var found map[string]any
	for _, site := range rows {
		b, _ := site["binding"].(map[string]any)
		if !sameIn(storePrefix, asString(b["storeId"]), storeID) || !sameIn(accountPrefix, asString(site["accountId"]), accountID) {
			continue
		}
		if strings.TrimSpace(asString(site["status"])) != "live" {
			continue
		}
		if strings.TrimSpace(asString(site["ownerUserId"])) == "" || strings.TrimSpace(asString(site["hostname"])) == "" {
			continue
		}
		if found != nil {
			return nil, true
		}
		found = site
	}
	return found, false
}

const (
	storePrefix   = "v1:shopify:store:"
	accountPrefix = "v1:accounts:account:"
)

// sameIn reports whether two ids name the same row of one concept. Either may
// be written bare or canonically; an id of any other concept names no row of
// this one, whatever its slug.
func sameIn(prefix, a, b string) bool {
	ca, cb := canonicalIn(prefix, a), canonicalIn(prefix, b)
	return ca != "" && ca == cb
}

func canonicalIn(prefix, id string) string {
	id = strings.TrimSpace(id)
	switch {
	case id == "":
		return ""
	case strings.HasPrefix(id, prefix):
		if id == prefix {
			return ""
		}
		return id
	case strings.Contains(id, ":"):
		return ""
	}
	return prefix + id
}

func setIfPresentAny(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func clip(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
