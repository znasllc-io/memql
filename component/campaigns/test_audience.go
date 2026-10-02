package campaigns

import (
	"context"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func testSettingsID(account string) string {
	return sha256Hex("campaign-test-settings\x00" + bare(account))
}
func testRunID(source, user, request string) string {
	return sha256Hex("campaign-test-run\x00" + bare(source) + "\x00" + bare(user) + "\x00" + request)
}

func (w *Worker) testSettings(ctx context.Context, account string) (map[string]any, error) {
	rows, err := w.store.rows(ctx, call("query", "campaignTestSettings", arg{"accountId", account}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (w *Worker) activeTestAudience(ctx context.Context, account, audience string) error {
	rows, err := w.store.rows(ctx, call("query", "audienceById", arg{"audienceId", audience}))
	if err != nil {
		return err
	}
	if len(rows) != 1 || !sameOrganization(account, str(rows[0], "accountId")) || str(rows[0], "status") != "active" {
		return fmt.Errorf("select an active testing audience in this organization")
	}
	return nil
}

func (w *Worker) handleConfigureTestAudience(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	account, audience := bare(argString(args, "accountId")), bare(argString(args, "audienceId"))
	if !templateIDPattern.MatchString(account) || callerUserID(ctx) == "" {
		return nil, fmt.Errorf("select an organization while signed in")
	}
	if err := w.requireSendAuthority(ctx, account); err != nil {
		return nil, err
	}
	if w.testGate == nil {
		return nil, fmt.Errorf("test coordination is unavailable")
	}
	release, err := w.testGate(ctx, testSettingsID(account))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	if audience != "" {
		if err := w.activeTestAudience(ctx, account, audience); err != nil {
			return nil, err
		}
	}
	prior, err := w.testSettings(ctx, account)
	if err != nil {
		return nil, err
	}
	revision := templateTime(prior["createdAt"])
	if prior != nil && bare(str(prior, "audienceId")) == audience {
		return resultNode("campaignTestAudienceSaved", map[string]any{"audienceId": audience, "saved": true})
	}
	if argString(args, "expectedRevision") != "" && !templateTime(args["expectedRevision"]).Equal(revision) || prior != nil && argString(args, "expectedRevision") == "" {
		return nil, fmt.Errorf("testing audience changed; refresh settings before saving")
	}
	err = w.store.execServerOnly(ctx, call("mutation", "saveCampaignTestSettings", arg{"settingsId", testSettingsID(account)}, arg{"accountId", account}, arg{"audienceId", audience}, arg{"versionTime", memql.VersionTimeAfter(revision, w.nowUTC()).Format(time.RFC3339Nano)}))
	if err != nil {
		return nil, err
	}
	return resultNode("campaignTestAudienceSaved", map[string]any{"audienceId": audience, "saved": true})
}

// A test is an ordinary campaign occurrence with an explicitly substituted
// audience. It uses startSend and DrainOnce, including the published snapshot,
// suppression, personalization, tracking, signed opt-out, retries and receipts.
// No in-memory authorization or test state needs to follow it to the worker.
func (w *Worker) handleTestAudienceSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	source, request, audience := bare(argString(args, "campaignId")), argString(args, "requestId"), bare(argString(args, "audienceId"))
	if !templateIDPattern.MatchString(source) || !templateIDPattern.MatchString(request) || callerUserID(ctx) == "" {
		return nil, fmt.Errorf("a campaign and stable test requestId are required while signed in")
	}
	if w.testGate == nil {
		return nil, fmt.Errorf("test coordination is unavailable")
	}
	runID := testRunID(source, callerUserID(ctx), request)
	release, err := w.testGate(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	original, found, err := w.store.CampaignByID(ctx, source)
	if err != nil {
		return nil, err
	}
	if !found || original.AccountID == "" || original.TestSourceCampaignID != "" {
		return nil, fmt.Errorf("select an accessible original campaign with an organization")
	}
	if err := w.requireSendAuthority(ctx, original.AccountID); err != nil {
		return nil, err
	}
	run, exists, err := w.store.CampaignByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	if exists {
		if run.TestSourceCampaignID != source || run.OwnerUserID != bare(callerUserID(ctx)) || !sameOrganization(run.AccountID, original.AccountID) || run.AudienceID != audience {
			return nil, fmt.Errorf("test request conflicts with an existing run; start a new test")
		}
		if run.Status != "draft" {
			return testRunReceipt(run)
		}
	} else {
		setting, err := w.testSettings(ctx, original.AccountID)
		if err != nil {
			return nil, err
		}
		if audience == "" || bare(str(setting, "audienceId")) != audience {
			return nil, fmt.Errorf("choose and review this organization's testing audience in Campaigns Settings")
		}
		if err := w.activeTestAudience(ctx, original.AccountID, audience); err != nil {
			return nil, err
		}
		definition := original
		series, err := w.seriesForCampaign(ctx, source)
		if err != nil {
			return nil, err
		}
		if series != nil {
			definition = seriesCampaign(series)
			if !sameOrganization(definition.AccountID, original.AccountID) {
				return nil, fmt.Errorf("recurring campaign belongs to another organization")
			}
		}
		if audience == original.AudienceID || audience == definition.AudienceID {
			return nil, fmt.Errorf("choose a testing audience separate from this campaign's live audience")
		}
		run = definition
		run.ID, run.TestSourceCampaignID, run.OwnerUserID = runID, source, bare(callerUserID(ctx))
		run.AudienceID, run.Name, run.Status, run.ScheduledAt = audience, original.Name+" · Test", "draft", time.Time{}
		// Refuse before writing anything when the normal send checks would fail.
		if _, _, err := w.preflight(ctx, "testAudienceSend", run); err != nil {
			return nil, err
		}
		values := []arg{{"campaignId", run.ID}, {"testSourceCampaignId", source}, {"name", run.Name}, {"accountId", run.AccountID}}
		for key, value := range seriesDefinition(run) {
			values = append(values, arg{key, value})
		}
		if err := w.store.execServerOnly(ctx, call("mutation", "createCampaignTestRun", values...)); err != nil {
			return nil, err
		}
	}
	// Same admission and enqueue operation as the Send now button. A crash
	// before commitment leaves a draft, which can safely resume under this lock.
	if _, err := w.handleStartSend(ctx, map[string]any{"campaignId": runID}, 0); err != nil {
		return nil, err
	}
	run.Status = "sending"
	return testRunReceipt(run)
}

func testRunReceipt(run Campaign) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignTestRunQueued", map[string]any{"campaignId": run.TestSourceCampaignID, "testRunId": run.ID, "audienceId": run.AudienceID, "status": run.Status})
}
