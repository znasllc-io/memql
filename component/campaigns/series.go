package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	_ "time/tzdata"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// The series is an instruction, never the delivery ledger. Stable occurrence
// IDs make recovery after any intermediate write converge on one campaign.
func seriesID(campaign string) string { return sha256Hex("campaign-series\x00" + bare(campaign)) }
func occurrenceID(series string, at time.Time) string {
	return sha256Hex("campaign-occurrence\x00" + bare(series) + "\x00" + at.UTC().Format(time.RFC3339Nano))
}

func seriesDefinition(c Campaign) map[string]any {
	return map[string]any{"audienceId": c.AudienceID, "templateId": c.TemplateID, "senderIdentityId": c.SenderIdentityID, "fromName": c.FromName, "replyTo": c.ReplyTo, "trackOpens": c.TrackOpens, "trackClicks": c.TrackClicks}
}

func seriesCampaign(row map[string]any) Campaign {
	d := objectField(row, "definition")
	return Campaign{OwnerUserID: bare(str(row, "authorizedByUserId")), AccountID: bare(str(row, "accountId")), Name: str(row, "name"), AudienceID: bare(str(d, "audienceId")), TemplateID: bare(str(d, "templateId")), SenderIdentityID: bare(str(d, "senderIdentityId")), FromName: str(d, "fromName"), ReplyTo: str(d, "replyTo"), TrackOpens: booleanOr(d, "trackOpens", true), TrackClicks: booleanOr(d, "trackClicks", true)}
}

func (w *Worker) seriesForCampaign(ctx context.Context, id string) (map[string]any, error) {
	rows, err := w.store.rows(ctx, call("query", "campaignSeriesForCampaign", arg{"campaignId", id}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func seriesReceipt(row map[string]any) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignSeriesSaved", map[string]any{"seriesId": bare(str(row, "id")), "campaignId": bare(str(row, "sourceCampaignId")), "revision": templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano), "status": str(row, "status"), "nextAt": templateTime(row["nextAt"]).UTC().Format(time.RFC3339Nano), "saved": true})
}

func (w *Worker) handleConfigureSeries(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	source, action := bare(argString(args, "campaignId")), argString(args, "action")
	if !templateIDPattern.MatchString(source) || callerUserID(ctx) == "" {
		return nil, fmt.Errorf("select a campaign while signed in")
	}
	if action != "save" && action != "pause" && action != "resume" {
		return nil, fmt.Errorf("unknown repeating campaign action")
	}
	if w.seriesGate == nil {
		return nil, fmt.Errorf("recurring campaign coordination is unavailable")
	}
	release, err := w.seriesGate(ctx, seriesID(source))
	if err != nil {
		return nil, err
	}
	defer release()
	campaign, found, err := w.store.CampaignByID(ctx, source)
	if err != nil {
		return nil, err
	}
	if !found || campaign.AccountID == "" {
		return nil, fmt.Errorf("select an accessible campaign with an organization")
	}
	if err = w.requireSendAuthority(ctx, campaign.AccountID); err != nil {
		return nil, err
	}
	prior, err := w.seriesForCampaign(ctx, source)
	if err != nil {
		return nil, err
	}
	if prior != nil && !sameOrganization(str(prior, "accountId"), campaign.AccountID) {
		return nil, fmt.Errorf("a recurring campaign cannot change organizations")
	}
	if prior == nil && action != "save" {
		return nil, fmt.Errorf("save the recurring campaign first")
	}

	// The complete captured instruction is compared before revision validation so
	// retrying an unchanged save after a lost response has no additional effect.
	next := map[string]any{}
	for k, v := range prior {
		next[k] = v
	}
	next["id"], next["accountId"], next["sourceCampaignId"] = seriesID(source), campaign.AccountID, source
	next["authorizedByUserId"] = callerUserID(ctx)
	if action == "save" {
		weeks, err := strconv.Atoi(fmt.Sprint(args["intervalWeeks"]))
		if err != nil || weeks < 1 || weeks > 52 {
			return nil, fmt.Errorf("choose an interval from 1 to 52 whole weeks")
		}
		zone := argString(args, "timeZone")
		if _, err = seriesLocation(zone); err != nil {
			return nil, err
		}
		anchor, err := time.Parse(time.RFC3339Nano, argString(args, "firstSendAt"))
		if err != nil {
			return nil, fmt.Errorf("choose the first send date and time")
		}
		next["name"], next["definition"], next["intervalWeeks"], next["timeZone"], next["anchorAt"] = campaign.Name, seriesDefinition(campaign), weeks, zone, anchor.UTC().Format(time.RFC3339Nano)
		next["status"] = "active"
		next["nextAt"] = next["anchorAt"]
		if prior != nil && sameSeriesConfiguration(prior, next) {
			return seriesReceipt(prior)
		}
		if prior != nil && anchor.Equal(templateTime(prior["anchorAt"])) {
			following, err := nextSeriesTime(anchor, weeks, zone, w.nowUTC())
			if err != nil {
				return nil, err
			}
			next["nextAt"] = following.UTC().Format(time.RFC3339Nano)
		} else if !anchor.After(w.nowUTC()) || anchor.After(w.nowUTC().AddDate(5, 0, 0)) {
			return nil, fmt.Errorf("choose a first send in the future, within five years")
		}
	} else {
		next["status"] = map[string]string{"pause": "paused", "resume": "active"}[action]
		if str(prior, "status") == str(next, "status") && bare(str(prior, "authorizedByUserId")) == callerUserID(ctx) {
			return seriesReceipt(prior)
		}
		if action == "resume" {
			following, err := nextSeriesTime(templateTime(prior["anchorAt"]), integer(prior, "intervalWeeks"), str(prior, "timeZone"), w.nowUTC())
			if err != nil {
				return nil, err
			}
			next["nextAt"] = following.UTC().Format(time.RFC3339Nano)
		}
	}
	revision := templateTime(prior["createdAt"])
	expected := argString(args, "expectedRevision")
	if prior == nil && expected != "" || prior != nil && (revision.IsZero() || !revision.Equal(templateTime(expected))) {
		return nil, fmt.Errorf("this recurring campaign changed; reload it before saving")
	}
	if str(next, "status") == "active" {
		if _, _, err = w.preflightAudience(ctx, "configureSeries", seriesCampaign(next), true); err != nil {
			return nil, err
		}
	}
	next["createdAt"] = memql.VersionTimeAfter(revision, w.nowUTC()).Format(time.RFC3339Nano)
	next["lastError"] = ""
	values := []arg{{"seriesId", str(next, "id")}, {"versionTime", str(next, "createdAt")}}
	for _, key := range []string{"accountId", "sourceCampaignId", "name", "definition", "intervalWeeks", "timeZone", "anchorAt", "nextAt", "status", "lastCampaignId", "lastOccurrenceAt"} {
		if value, ok := next[key]; ok && value != nil {
			values = append(values, arg{key, value})
		}
	}
	named := map[string]any{}
	for _, a := range values {
		named[a.name] = a.value
	}
	statement, err := langparser.RenderCall("configureCampaignSeries", named)
	if err != nil {
		return nil, err
	}
	if err = w.store.execServerOnly(ctx, "mutation "+statement); err != nil {
		return nil, err
	}
	return seriesReceipt(next)
}

func sameSeriesConfiguration(a, b map[string]any) bool {
	for _, key := range []string{"accountId", "sourceCampaignId", "authorizedByUserId"} {
		if bare(str(a, key)) != bare(str(b, key)) {
			return false
		}
	}
	if !templateTime(a["anchorAt"]).Equal(templateTime(b["anchorAt"])) {
		return false
	}
	for _, key := range []string{"name", "definition", "intervalWeeks", "timeZone", "status"} {
		left, _ := json.Marshal(a[key])
		right, _ := json.Marshal(b[key])
		if string(left) != string(right) {
			return false
		}
	}
	return true
}

func seriesLocation(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, fmt.Errorf("choose an IANA timezone such as America/Phoenix or UTC")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("unknown campaign timezone")
	}
	return location, nil
}

// Calculate from the original local calendar time every time, so a DST gap
// normalized by Go on one occurrence does not permanently shift later sends.
func nextSeriesTime(anchor time.Time, weeks int, zone string, after time.Time) (time.Time, error) {
	location, err := seriesLocation(zone)
	if err != nil {
		return time.Time{}, err
	}
	if anchor.IsZero() || weeks < 1 || weeks > 52 {
		return time.Time{}, fmt.Errorf("invalid recurring campaign cadence")
	}
	local := anchor.In(location)
	if anchor.After(after) {
		return anchor, nil
	}
	date := func(t time.Time) time.Time {
		t = t.In(location)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	days := (date(after).Unix() - date(anchor).Unix()) / 86400
	period := num.ClampInt64(days / int64(7*weeks))
	if period < 0 {
		period = 0
	}
	for {
		candidate := local.AddDate(0, 0, period*weeks*7)
		if candidate.After(after) {
			return candidate.UTC(), nil
		}
		period++
	}
}

func (w *Worker) promoteDueSeries(ctx, systemCtx context.Context) {
	if w.seriesGate == nil {
		return
	}
	systemCtx = memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(systemCtx))
	cursor := ""
	for {
		rows, next, err := w.store.rowsPage(systemCtx, cursor, call("query", "dueCampaignSeries", arg{"dueAt", w.nowUTC().Format(time.RFC3339Nano)}))
		if err != nil {
			w.logger.Debug("campaigns: recurring scan unavailable", "error", err)
			return
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			if err = w.promoteSeries(ctx, systemCtx, row); err != nil {
				w.logger.Error("campaigns: recurring occurrence not committed", "series", bare(str(row, "id")), "error", err)
			}
		}
		if next == "" || next == cursor {
			return
		}
		cursor = next
	}
}

func (w *Worker) promoteSeries(ctx, systemCtx context.Context, scanned map[string]any) error {
	release, err := w.seriesGate(ctx, bare(str(scanned, "id")))
	if err != nil {
		return err
	}
	defer release()
	systemCtx = memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(systemCtx))
	row, err := w.seriesForCampaign(systemCtx, bare(str(scanned, "sourceCampaignId")))
	if err != nil || row == nil {
		return err
	}
	when := templateTime(row["nextAt"])
	if str(row, "status") != "active" || when.After(w.nowUTC()) {
		return nil
	}
	if when.IsZero() {
		return w.advanceSeries(systemCtx, row, "blocked", when, "", "The next occurrence has no valid date.")
	}
	campaign := seriesCampaign(row)
	ownerCtx := memql.ContextWithFreshRead(w.ownerActorContext(ctx, campaign.OwnerUserID))
	if _, _, err = w.preflightAudience(ownerCtx, "recurringSend", campaign, true); err != nil {
		return w.advanceSeries(systemCtx, row, "blocked", when, "", err.Error())
	}
	campaign.ID = occurrenceID(bare(str(row, "id")), when)
	campaign.Name += " · " + when.UTC().Format("2006-01-02 15:04 UTC")
	existing, found, err := w.store.CampaignByID(ownerCtx, campaign.ID)
	if err != nil {
		return err
	}
	if found {
		if existing.OwnerUserID != campaign.OwnerUserID || !sameOrganization(existing.AccountID, campaign.AccountID) {
			return fmt.Errorf("occurrence identity conflicts with an existing campaign")
		}
		campaign = existing
	} else {
		values := []arg{{"campaignId", campaign.ID}, {"accountId", campaign.AccountID}, {"name", campaign.Name}}
		for key, value := range seriesDefinition(campaign) {
			values = append(values, arg{key, value})
		}
		if err = w.store.exec(ownerCtx, call("mutation", "createCampaign", values...)); err != nil {
			return err
		}
		campaign.Status = "draft"
	}
	job, hasJob, err := w.store.JobByID(systemCtx, campaign.ID)
	if err != nil {
		return err
	}
	if !hasJob {
		if campaign.Status != "draft" {
			return fmt.Errorf("occurrence has changed without a send job; refusing to recreate it")
		}
		_, snapshot, err := w.preflightAudience(ownerCtx, "recurringSend", campaign, true)
		if err != nil {
			return err
		}
		if err = w.store.EnqueueSend(systemCtx, SendJob{CampaignID: campaign.ID, CampaignOwnerUserID: campaign.OwnerUserID, CampaignAccountID: campaign.AccountID, AudienceID: campaign.AudienceID, TemplateID: campaign.TemplateID, TemplateSnapshot: &snapshot, Status: "scheduled", ScheduledAt: when}); err != nil {
			return err
		}
	} else if job.CampaignID != campaign.ID || job.CampaignOwnerUserID != campaign.OwnerUserID || !sameOrganization(job.CampaignAccountID, campaign.AccountID) {
		return fmt.Errorf("occurrence send job does not match its campaign")
	}
	// The inert job precedes commitment. If a later retry sees a completed or
	// cancelled occurrence, leave it alone; never reset its status or ledger.
	if campaign.Status == "draft" {
		if err = w.store.ScheduleCampaign(ownerCtx, campaign.ID, when); err != nil {
			return err
		}
	}
	next, err := nextSeriesTime(templateTime(row["anchorAt"]), integer(row, "intervalWeeks"), str(row, "timeZone"), w.nowUTC())
	if err != nil {
		return err
	}
	return w.advanceSeries(systemCtx, row, "active", next, campaign.ID, "")
}

func (w *Worker) advanceSeries(ctx context.Context, row map[string]any, status string, next time.Time, campaignID, reason string) error {
	values := []arg{{"seriesId", bare(str(row, "id"))}, {"status", status}, {"nextAt", next.UTC().Format(time.RFC3339Nano)}, {"lastError", truncateError(reason)}, {"versionTime", memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)}}
	if campaignID != "" {
		values = append(values, arg{"lastCampaignId", campaignID}, arg{"lastOccurrenceAt", templateTime(row["nextAt"]).UTC().Format(time.RFC3339Nano)})
	}
	return w.store.execServerOnly(ctx, call("mutation", "advanceCampaignSeries", values...))
}
