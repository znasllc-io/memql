package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// export.go -- ONE STORE'S WHOLE WHOLESALE RECORD (wholesaleExportStore).
//
// One JSON file holding every application the caller reads -- each with the
// client's own fields on it and the state its log gives it -- their decisions,
// entitlements and acceptances, the store's settings and every version of its
// documents, so a store can keep its record or take it elsewhere.
//
// THE SAME RULE AS THE LIST. The applications are read under the caller, so
// what a caller cannot read is not in their export; an application's decisions
// and entitlement are then read as the deployment, so the record holds its
// whole log (X27). The settings are read as the deployment once siteForStore
// has checked the site is the store's.
//
// A TOKEN HASH NEVER LEAVES: it is a credential's digest, not the store's data,
// and every row is stripped of one, whatever concept a later task adds.

// exportFormat names the file's shape, so a later reader knows what it holds.
const exportFormat = "memql.wholesale.export/v1"

// tokenHashField is the field a credential's digest is kept in.
const tokenHashField = "tokenHash"

// exportAdmission is wholesaleExportStore's own check before it reads
// anything: the site must serve the store (admitStore). wholesaleStoreSettings'
// may.export is this check, called.
func (p *Provider) exportAdmission(ctx context.Context, args map[string]any) (string, error) {
	storeID, _, err := p.admitStore(ctx, args)
	return storeID, err
}

func (p *Provider) exportStore(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, err := p.exportAdmission(ctx, args)
	if err != nil {
		return nil, err
	}
	// THE WHOLE STORE, page after page, in every form its id is held in: an
	// export that stopped at a thousand would be a record that silently lost
	// its oldest applications.
	apps, _, err := p.storeApplications(ctx, storeID, false)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, trimmed(app["id"]))
	}
	// Each application's whole log and entitlement, as the deployment, in
	// batches read to their end.
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	logs, err := p.rowsForApplications(sys, "decisionsForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	ents, err := p.rowsForApplications(sys, "entitlementsForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	// EVERY ACCEPTANCE, whether or not the store requires its terms today: what
	// an applicant accepted is part of the record.
	accs, err := p.rowsForApplications(sys, "acceptancesForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	// AND EVERY EMAIL SENT, each attempt's record: what the store told whom and
	// when is part of the record. The rows hold no address and no token.
	sends, err := p.rowsForApplications(sys, "emailSendsForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	// EACH EMAIL AS THE LANE SAYS IT STANDS (effectiveSend), with the lane's
	// record of its delivery: a deferral the lane later sent is sent, at its
	// time. Read once per organization, a hundred request ids at a time.
	byAccount := map[string][]string{}
	for _, app := range apps {
		account := bareID(trimmed(app["accountId"]))
		for _, send := range sends[bareID(trimmed(app["id"]))] {
			byAccount[account] = append(byAccount[account], trimmed(send["requestId"]))
		}
	}
	// A STATUS THAT CANNOT BE READ does not refuse the export: each attempt of
	// that organization says, in its own field, that its delivery could not be
	// read, and is exported as it was recorded.
	deliveries := map[string]map[string]any{}
	unreadable := map[string]bool{}
	for account, requestIDs := range byAccount {
		got, err := p.laneDeliveries(ctx, map[string]any{"accountId": account}, requestIDs)
		if err != nil {
			for _, id := range requestIDs {
				unreadable[id] = true
			}
			continue
		}
		for id, d := range got {
			deliveries[id] = d
		}
	}
	termsRequired, err := p.termsRequiredAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	applications := make([]map[string]any, 0, len(apps))
	decisions := []map[string]any{}
	entitlements := []map[string]any{}
	acceptances := []map[string]any{}
	emailSends := []map[string]any{}
	for _, app := range apps {
		id := trimmed(app["id"])
		if id == "" {
			continue
		}
		log := logs[bareID(id)]
		details, err := p.details(ctx, app)
		if err != nil {
			return nil, err
		}
		out := happenedRow(app, fieldSubmittedAt)
		out["details"] = details
		out["state"] = FoldState(log)
		out["stage"] = StageOf(FoldState(log), termsRequired, len(accs[bareID(id)]) > 0)
		applications = append(applications, out)
		// ONE FORM OF THE APPLICATION'S ID throughout the file: the row's own,
		// whichever form an older decision or entitlement was written under.
		for _, d := range log {
			row := happenedRow(d, fieldDecidedAt)
			row["applicationId"] = id
			decisions = append(decisions, row)
		}
		if versions := ents[bareID(id)]; len(versions) > 0 {
			row := happenedEntitlementRow(versions[0])
			row["applicationId"] = id
			// WHY ACCESS IS NOT ON, as the application's page and the buyers list say
			// it: a sentence for a person (accessReason), never the record's own text.
			if asString(row["error"]) != "" {
				row["error"] = accessReason(versions[0])
			}
			entitlements = append(entitlements, row)
		}
		if rows := accs[bareID(id)]; len(rows) > 0 {
			row := withoutTokenHash(rows[0])
			row["applicationId"] = id
			acceptances = append(acceptances, row)
		}
		for _, send := range sends[bareID(id)] {
			d := deliveries[trimmed(send["requestId"])]
			row := withoutTokenHash(p.effectiveSend(send, d, !unreadable[trimmed(send["requestId"])]))
			row["applicationId"] = id
			row["deliveryUnreadable"] = unreadable[trimmed(send["requestId"])]
			if d != nil {
				row["delivery"] = d
			} else {
				row["delivery"] = nil
			}
			emailSends = append(emailSends, row)
		}
	}
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	var settingsOut any
	if settings != nil {
		settingsOut = effectiveSettingsRow(settings)
	}
	// EVERY VERSION OF EVERY DOCUMENT, bodies included, read to the end in
	// every form of the store id: what an applicant accepted is part of the
	// record.
	versions, err := p.storeDocumentVersions(ctx, storeID)
	if err != nil {
		return nil, err
	}
	documentVersions, omitted := boundedDocumentVersions(versions)
	now := time.Now().UTC()
	if p.now != nil {
		now = p.now().UTC()
	}
	doc, err := json.MarshalIndent(map[string]any{
		"format":       exportFormat,
		"exportedAt":   now.Format(time.RFC3339Nano),
		"storeId":      storeID,
		"applications": applications,
		"decisions":    decisions,
		"entitlements": entitlements,
		"acceptances":  acceptances,
		"emailSends":   emailSends,
		"settings":     settingsOut,
		// Kind by kind (terms, map, priceSheet), each newest first.
		"documentVersions":      documentVersions,
		"documentBodiesOmitted": omitted,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal export: %w", err)
	}
	return confirm("wholesale:export", map[string]any{
		"fileName": "wholesale-" + slugOf(storeID) + "-" + now.Format("20060102") + ".json",
		"json":     string(doc),
	})
}

// maxExportBodyBytes is the most document body text one export carries.
const maxExportBodyBytes = 4 << 20

// boundedDocumentVersions is the export's versions, in their order, with the
// bodies carried NEWEST FIRST across every kind until maxExportBodyBytes: the
// first body that does not fit, and every older one, is left out, and its
// version is exported with every other field and bodyOmitted true. It answers
// how many were left out. A body left out is read with wholesaleDocuments and
// its versionId.
func boundedDocumentVersions(versions []map[string]any) ([]map[string]any, int) {
	byAge := append([]map[string]any(nil), versions...)
	sort.SliceStable(byAge, func(i, j int) bool { return newerVersion(byAge[i], byAge[j]) })
	omit := map[string]bool{}
	budget, full := maxExportBodyBytes, false
	for _, v := range byAge {
		size := len(asString(v["body"]))
		if full || size > budget {
			full = true
			omit[bareID(trimmed(v["id"]))] = true
			continue
		}
		budget -= size
	}
	out := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		row := withoutTokenHash(v)
		if omit[bareID(trimmed(v["id"]))] {
			delete(row, "body")
			row["bodyOmitted"] = true
		}
		out = append(out, row)
	}
	return out, len(omit)
}

// effectiveSettingsRow is a settings row as the store runs on it: the stored
// row without its token hash, with every setting EffectiveSettings defaults at
// its effective value, as wholesaleStoreSettings answers it (ruling 47).
func effectiveSettingsRow(row map[string]any) map[string]any {
	out := withoutTokenHash(row)
	s := EffectiveSettings(row)
	out["applicationsOpen"] = s.ApplicationsOpen
	out["entitlementAdapter"] = s.EntitlementAdapter
	out["introText"] = s.IntroText
	out["staffEmail"] = s.StaffEmail
	out["termsRequired"] = s.TermsRequired
	out["linkLifetimeDays"] = s.LinkLifetimeDays
	out["acceptanceText"] = s.AcceptanceText
	return out
}

// happenedRow is row as the export holds it, without its token hash: its
// createdAt and its field answer when the row's thing happened (happenedAt),
// never the write time of its newest version, which any later write moves.
func happenedRow(row map[string]any, field string) map[string]any {
	out := withoutTokenHash(row)
	at := timeText(happenedAt(row, field))
	out["createdAt"], out[field] = at, at
	return out
}

// happenedEntitlementRow is an entitlement as the export holds it: dated by its
// attempt (happenedRow), with the times of its CURRENT spell only -- grantedAt
// while it is granted (the start of the unbroken granted period), revokedAt
// while it is revoked -- each as the row holds it, or, on a row written before
// those fields, its write time. A time from a spell that is over (a removal
// before a grant again, a grant before a failure) is left out, as the buyers
// list and the page leave it out.
func happenedEntitlementRow(row map[string]any) map[string]any {
	out := happenedRow(row, fieldAttemptedAt)
	for field, state := range map[string]string{fieldGrantedAt: EntitlementGranted, fieldRevokedAt: EntitlementRevoked} {
		if trimmed(row["state"]) != state {
			delete(out, field)
			continue
		}
		out[field] = timeText(happenedAt(row, field))
	}
	return out
}

// withoutTokenHash is a copy of row with every token hash removed, at any depth.
func withoutTokenHash(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		if k == tokenHashField {
			continue
		}
		out[k] = stripTokenHash(v)
	}
	return out
}

func stripTokenHash(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return withoutTokenHash(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = stripTokenHash(item)
		}
		return out
	}
	return v
}
