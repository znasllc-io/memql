package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

// notify_test.go -- a notify stage delivers over the outbound path (epic
// memql#5480, design record D16), driven over the fakes the driver's own tests
// use: the real workjournal over the fake work spine, and the fake store's
// outbox, whose rows a test moves the way the outbound worker does. Nothing
// here hands the driver a delivery's outcome any other way: what it reports
// is what it read off the rows it staged.

// notifyManifest runs tests on every run and, on a push, a deploy and a notify
// stage announcing the run on the Discord channel releases, with a Docs link.
// The unit step names SHOP_TOKEN, so the drive holds a secret value by the
// time the notify stage composes its message.
const notifyManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: tests
      steps:
        - name: unit
          run: go test ./...
          secrets: [SHOP_TOKEN]
    - name: deploy
      on: [push]
      steps:
        - name: rollout
          run: ./deploy.sh
    - name: notify
      on: [push]
      channel: releases
      links:
        - label: Docs
          url: https://docs.example.test/shop
`

const (
	// discordHook is DISCORD_RELEASES's value: a Discord webhook's URL, with
	// its token in the path. It is a credential, and no row, receipt, check
	// run or log line may carry it.
	discordHook = "https://discord.com/api/webhooks/1234567890/hook-token-7f3a9c"
	// hookToken is the part of it that is the credential.
	hookToken = "hook-token-7f3a9c"
	// releasesID is the releases channel's id, bare.
	releasesID = "ch-releases"
)

// notifyRequestIDRe is a notify row's id: a short prefix and 128 random bits.
var notifyRequestIDRe = regexp.MustCompile(`^pn[0-9a-f]{32}$`)

// namesAsked records every name the secret resolver was asked for.
type namesAsked struct {
	mu    sync.Mutex
	names []string
}

func (a *namesAsked) add(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names = append(a.names, name)
}

func (a *namesAsked) count(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, asked := range a.names {
		if asked == name {
			n++
		}
	}
	return n
}

func (a *namesAsked) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.names)
}

// newNotifyHarness is newDriveHarness with the releases channel: the
// pipeline owner's, active, a Discord webhook named by DISCORD_RELEASES, and
// allowed on the pipeline -- by its canonical id there and its bare one on the
// channel, which is sameID's to reconcile.
func newNotifyHarness(t *testing.T, manifest string) *driveHarness {
	t.Helper()
	dh := newDriveHarness(t, manifest)
	dh.secrets["DISCORD_RELEASES"] = discordHook
	dh.store.addChannel(Channel{
		ID: releasesID, OwnerUserID: dh.p.OwnerUserID, AccountID: dh.p.AccountID,
		Name: "releases", Kind: "discord", SecretRef: "DISCORD_RELEASES", Status: "active",
	})
	dh.allowChannels("v1:pipelines:channel:" + releasesID)
	return dh
}

// allowChannels sets the channels the pipeline delivers to.
func (dh *driveHarness) allowChannels(ids ...string) {
	dh.p.ChannelIDs = ids
	dh.store.addPipeline(dh.p)
}

// setChannel rewrites the channel with id through change.
func (dh *driveHarness) setChannel(t *testing.T, id string, change func(*Channel)) {
	t.Helper()
	c, ok := dh.store.channel(id)
	if !ok {
		t.Fatalf("no channel %s", id)
	}
	change(&c)
	dh.store.addChannel(c)
}

// addOps adds the ops channel: email, allowed on the pipeline, to recipients.
func (dh *driveHarness) addOps(recipients ...string) {
	dh.store.addChannel(Channel{
		ID: "ch-ops", OwnerUserID: dh.p.OwnerUserID, AccountID: dh.p.AccountID,
		Name: "ops", Kind: "email", Status: "active", Recipients: recipients,
	})
	dh.allowChannels("ch-ops")
}

// opsManifest is notifyManifest announcing on the email channel ops.
var opsManifest = strings.Replace(notifyManifest, "channel: releases", "channel: ops", 1)

// moveAt moves every row the n-th read of the outbox asks for to status, as
// the outbound worker would between two of the stage's polls.
func (dh *driveHarness) moveAt(n int, status string, attempts int, lastError string) {
	dh.store.mu.Lock()
	defer dh.store.mu.Unlock()
	dh.store.onOutboundRead = func(read int, ids []string) {
		if read == n {
			for _, id := range ids {
				dh.store.setOutbound(id, status, attempts, lastError)
			}
		}
	}
}

// deliverAt delivers every row the n-th read asks for.
func (dh *driveHarness) deliverAt(n int) { dh.moveAt(n, "sent", 1, "") }

// deliverEvery delivers every row any read asks for.
func (dh *driveHarness) deliverEvery() {
	dh.store.mu.Lock()
	defer dh.store.mu.Unlock()
	dh.store.onOutboundRead = func(_ int, ids []string) {
		for _, id := range ids {
			dh.store.setOutbound(id, "sent", 1, "")
		}
	}
}

// receiptOf is the one receipt of the step keyed key.
func (dh *driveHarness) receiptOf(t *testing.T, key string) map[string]any {
	t.Helper()
	receipts := dh.work.receiptsOf(key)
	if len(receipts) != 1 {
		t.Fatalf("%s has %d receipts, want exactly one: %+v", key, len(receipts), receipts)
	}
	return receipts[0].Args
}

// assertUnpublished fails t when a needle is on a journal write, a run-row
// write, a check-run write or a log line: everywhere a person or GitHub reads.
func (dh *driveHarness) assertUnpublished(t *testing.T, needles ...string) {
	t.Helper()
	dh.store.mu.Lock()
	patches, _ := json.Marshal(dh.store.runUpdates)
	dh.store.mu.Unlock()
	for _, needle := range needles {
		for _, c := range dh.work.recorded() {
			if strings.Contains(c.Raw, needle) {
				t.Errorf("a journal write carries %q: %s", needle, c.Raw)
			}
		}
		if strings.Contains(string(patches), needle) {
			t.Errorf("a run-row write carries %q", needle)
		}
		for _, w := range append(dh.github.createdRuns(), dh.github.updatedRuns()...) {
			if o := w.Run.Output; o != nil && strings.Contains(o.Title+o.Summary+o.Text, needle) {
				t.Errorf("a check-run write carries %q:\n%s\n%s", needle, o.Summary, o.Text)
			}
		}
		if strings.Contains(dh.logs.String(), needle) {
			t.Errorf("a log line carries %q:\n%s", needle, dh.logs.String())
		}
	}
}

// assertNowhere is assertUnpublished and the staged rows too: what no
// outbound row may carry either.
func (dh *driveHarness) assertNowhere(t *testing.T, needles ...string) {
	t.Helper()
	dh.assertUnpublished(t, needles...)
	for _, needle := range needles {
		for _, n := range dh.store.stagedNotifications() {
			if raw, _ := json.Marshal(n); strings.Contains(string(raw), needle) {
				t.Errorf("a staged row carries %q: %s", needle, raw)
			}
		}
	}
}

// discordMessage is a staged Discord body, decoded.
type discordMessage struct {
	Username        string `json:"username"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
	Embeds []struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Fields      []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	} `json:"embeds"`
}

func decodeDiscord(t *testing.T, body string) discordMessage {
	t.Helper()
	var m discordMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("the staged body is not Discord's JSON: %v\n%s", err, body)
	}
	if len(m.Embeds) != 1 {
		t.Fatalf("a message is one embed, got %d", len(m.Embeds))
	}
	return m
}

// fields is an embed's fields by name.
func (m discordMessage) fields() map[string]string {
	out := map[string]string{}
	for _, f := range m.Embeds[0].Fields {
		out[f.Name] = f.Value
	}
	return out
}

// onlyStaged is the one row staged, failing t when there is not exactly one.
func (dh *driveHarness) onlyStaged(t *testing.T) NotificationRequest {
	t.Helper()
	staged := dh.store.stagedNotifications()
	if len(staged) != 1 {
		t.Fatalf("want one staged row, got %d: %+v", len(staged), staged)
	}
	return staged[0]
}

// waitFor waits for cond, failing t after ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Delivering
// ---------------------------------------------------------------------------

func TestANotifyStageDeliversToDiscordWithTheRunPageAsItsReport(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	dh.store.addFile(dh.p.OwnerUserID, "file-art-tests.unit", "coverage.html")
	dh.deliverAt(2)
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	staged := dh.store.stagedNotifications()
	if len(staged) != 1 {
		t.Fatalf("one row is staged for a Discord channel, got %d: %+v", len(staged), staged)
	}
	row := staged[0]
	if row.Medium != "webhook" || row.TargetSecret != "DISCORD_RELEASES" || row.Target != "" || row.Subject != "" {
		t.Errorf("the row names the secret holding the URL, and never a URL: %+v", row)
	}
	if !notifyRequestIDRe.MatchString(row.RequestID) || row.DedupeKey != row.RequestID ||
		row.RequestedBy != "pipelines:notify:"+bareID(run.ID) {
		t.Errorf("request %q, dedupe key %q, requested by %q", row.RequestID, row.DedupeKey, row.RequestedBy)
	}

	msg := decodeDiscord(t, row.Body)
	runPage := pipelines.RunPageURL(testOSOrigin, bareID(run.ID))
	e := msg.Embeds[0]
	if msg.Username != "MemQL Pipelines" || msg.AllowedMentions.Parse == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Errorf("username %q, mentions %v", msg.Username, msg.AllowedMentions.Parse)
	}
	if e.Title != "shop · Push to main passed" || e.URL != runPage {
		t.Errorf("title %q url %q, want the run's headline linked to its page %q", e.Title, e.URL, runPage)
	}
	if want := "Land the cart (1111111)\nAll 2 stages passed."; e.Description != want {
		t.Errorf("description %q, want %q", e.Description, want)
	}
	for name, want := range map[string]string{
		"Version":            "1111111",
		"MemQL OS":           "[Open](" + testOSOrigin + "/)",
		"Deployment details": "[Open the run](" + runPage + ")",
		"Docs":               "[docs.example.test/shop](https://docs.example.test/shop)",
		// The run's files: the one the Library names by its name, the other by
		// its place, each opening in the Library.
		"Artifacts": "[coverage.html](" + testOSOrigin + "/?libraryFile=file-art-tests.unit)\n" +
			"[artifact 2](" + testOSOrigin + "/?libraryFile=file-art-deploy.rollout)",
	} {
		if got := msg.fields()[name]; got != want {
			t.Errorf("field %s = %q, want %q", name, got, want)
		}
	}

	// The step's receipt comes after the row reads sent, and from nothing else.
	if reads := dh.store.reads(); reads != 2 {
		t.Errorf("the stage reads the outbox until its row says sent, and no further: %d reads", reads)
	}
	args := dh.receiptOf(t, "notify.notify")
	result := argObject(args, "result")
	if argString(args, "status") != WorkStepDone || argString(args, "errorCode") != "" ||
		argString(args, "errorMessage") != "Delivered to releases (Discord)." {
		t.Errorf("receipt = %v", args)
	}
	if result["channel"] != "releases" || result["kind"] != "discord" ||
		!slices.Equal(argStrings(result, "requestIds"), []string{row.RequestID}) || result["sentAt"] != testNow.Format(time.RFC3339) {
		t.Errorf("result = %v", result)
	}
	if slices.Contains(dh.exec.sentKeys(), "notify.notify") {
		t.Errorf("a notify step is never handed to the step runner")
	}
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || len(got.Stages) != 3 || got.Stages[2].Name != "notify" || got.Stages[2].Status != StagePassed {
		t.Errorf("run %s stages %+v", got.Conclusion, got.Stages)
	}
	if last := dh.lastUpdate(t).Run; last.Conclusion != "success" || !strings.Contains(last.Output.Summary, "| notify | Passed | 1 passed |") {
		t.Errorf("check run %s:\n%s", last.Conclusion, last.Output.Summary)
	}

	// The channel and the previous runs are cross-replica facts the message
	// depends on: both are read fresh.
	for _, method := range []string{"ChannelForOwnerByName", "PreviousRuns"} {
		if calls := dh.store.freshCalls(method); len(calls) != 1 || !calls[0] {
			t.Errorf("%s is read once, fresh: %v", method, calls)
		}
	}
	// The URL was resolved to be checked, and goes nowhere.
	if n := dh.asked.count("DISCORD_RELEASES"); n != 1 {
		t.Errorf("the channel's secret is resolved once, to check it: %d", n)
	}
	dh.assertNowhere(t, discordHook, hookToken)
}

// A notify stage runs after a failed stage -- announcing the failure is what it
// is for -- while every other stage after it stays blocked.
func TestANotifyStageAnnouncesTheFailureTheStagesBeforeItEndedOn(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	// A secret holding Markdown: escaped for Discord it is no longer the string
	// a mask looks for, so the message's parts are masked before they are
	// escaped.
	const marked = "hunter2_shop*token"
	escaped := strings.NewReplacer("_", `\_`, "*", `\*`).Replace(marked)
	dh.secrets["SHOP_TOKEN"] = marked
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "tests.unit" {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
			res.Failure = &pipelines.Failure{Code: pipelines.CodeCloneFailed, Message: "could not fetch with " + marked}
		}
		return res, nil
	}
	dh.deliverAt(1)
	o := pushOpening()
	o.Title = "Land the cart for " + marked
	run := dh.openRun(t, o)
	deliver(t, dh.integ, run)

	if keys := dh.exec.sentKeys(); !slices.Equal(keys, []string{"tests.unit"}) {
		t.Errorf("only the tests stage reaches the runner: %v", keys)
	}
	rollout := dh.receiptOf(t, "deploy.rollout")
	if argString(rollout, "status") != WorkStepSkipped || argString(rollout, "errorCode") != pipelines.CodeStageBlocked ||
		argString(rollout, "errorMessage") != "Not run: stage tests failed." {
		t.Errorf("the deploy stage stays blocked: %v", rollout)
	}
	staged := dh.store.stagedNotifications()
	if len(staged) != 1 {
		t.Fatalf("the notify stage ran and staged one row, got %d", len(staged))
	}
	e := decodeDiscord(t, staged[0].Body).Embeds[0]
	if e.Title != "shop · Push to main failed at tests" {
		t.Errorf("title %q", e.Title)
	}
	want := `Land the cart for \*\*\* (1111111)` + "\n" + `tests/unit failed: pipeline\_clone\_failed. could not fetch with \*\*\*`
	if e.Description != want {
		t.Errorf("description:\n got %q\nwant %q", e.Description, want)
	}
	if strings.Contains(staged[0].Body, marked) || strings.Contains(e.Description+e.Title, escaped) {
		t.Errorf("the secret reaches the message: %s", staged[0].Body)
	}
	if args := dh.receiptOf(t, "notify.notify"); argString(args, "status") != WorkStepDone {
		t.Errorf("the announcement was delivered: %v", args)
	}
	got, _ := dh.store.run(run.ID)
	wantStages := []string{"tests:" + StageFailed, "deploy:" + StageBlocked, "notify:" + StagePassed}
	var stages []string
	for _, s := range got.Stages {
		stages = append(stages, s.Name+":"+s.Status)
	}
	if got.Conclusion != ConclusionFailure || !slices.Equal(stages, wantStages) {
		t.Errorf("run %s stages %v, want %v", got.Conclusion, stages, wantStages)
	}
	if last := dh.lastUpdate(t).Run; last.Output.Title != "Failed at tests: tests.unit" {
		t.Errorf("the check run fails at the stage that failed: %q", last.Output.Title)
	}
	dh.assertNowhere(t, marked, discordHook)
}

// A notify stage that fails blocks the stages after it, as any failed stage
// does; and one that runs after an earlier failure does not take its blame.
func TestAFailedNotifyStageBlocksWhatFollowsAndBlamesNothingBeforeIt(t *testing.T) {
	const manifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: tests
      steps:
        - name: unit
          run: go test ./...
    - name: notify
      on: [push]
      channel: nobody-made-this
    - name: deploy
      on: [push]
      steps:
        - name: rollout
          run: ./deploy.sh
`
	for name, c := range map[string]struct {
		failTests bool
		blocked   string
	}{
		"the tests pass": {false, "Not run: stage notify failed."},
		"the tests fail": {true, "Not run: stage tests failed."},
	} {
		t.Run(name, func(t *testing.T) {
			dh := newNotifyHarness(t, manifest)
			dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
				res := passed(req)
				if req.StepKey == "tests.unit" && c.failTests {
					res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
				}
				return res, nil
			}
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			if args := dh.receiptOf(t, "notify.notify"); argString(args, "errorCode") != pipelines.CodeChannelMissing {
				t.Errorf("the notify stage ran and failed on its channel: %v", args)
			}
			if args := dh.receiptOf(t, "deploy.rollout"); argString(args, "errorCode") != pipelines.CodeStageBlocked ||
				argString(args, "errorMessage") != c.blocked {
				t.Errorf("deploy: %v, want %q", args, c.blocked)
			}
		})
	}
}

// A run that is cancelled before its notify stage sends nothing: no row, no
// read of the outbox, and the channel's secret is never resolved.
func TestACancelledRunSendsNothing(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "tests.unit")
	if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDrives(t, dh.integ)

	if staged := dh.store.stagedNotifications(); len(staged) != 0 {
		t.Errorf("a cancelled run staged %d notifications", len(staged))
	}
	if reads, asked := dh.store.reads(), dh.asked.count("DISCORD_RELEASES"); reads != 0 || asked != 0 {
		t.Errorf("nothing about a delivery is read or resolved: %d reads, %d resolutions", reads, asked)
	}
	if args := dh.receiptOf(t, "notify.notify"); argString(args, "status") != WorkStepCancelled {
		t.Errorf("the notify step is settled cancelled: %v", args)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// A cancel that arrives while the stage waits on its delivery ends the step
// cancelled, and says what is true: the notification was handed over already,
// so it may still arrive.
func TestACancelWhileANotificationIsOnItsWaySaysItMayStillArrive(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	run := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	waitFor(t, "the notification to be staged", func() bool { return len(dh.store.stagedNotifications()) == 1 })
	if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDrives(t, dh.integ)

	args := dh.receiptOf(t, "notify.notify")
	if argString(args, "status") != WorkStepCancelled ||
		argString(args, "errorMessage") != "The run was cancelled while this step waited on its delivery to releases. "+
			"The notification had already been handed to the outbound worker, so it may still arrive." {
		t.Errorf("receipt = %v", args)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s", got.Conclusion)
	}
	if n := len(dh.store.stagedNotifications()); n != 1 {
		t.Errorf("nothing more is staged after the cancel: %d", n)
	}
}

// ---------------------------------------------------------------------------
// The channel
// ---------------------------------------------------------------------------

func TestANotifyStageRefusesAChannelItMayNotUse(t *testing.T) {
	const missing = "No channel named releases belongs to this pipeline's owner. Create it in Deployables > Settings > Channels, then re-run."
	for _, c := range []struct {
		name    string
		arrange func(t *testing.T, dh *driveHarness)
		code    string
		message string
	}{
		{"no channel of that name", func(t *testing.T, dh *driveHarness) {
			dh.setChannel(t, releasesID, func(c *Channel) { c.Name = "announcements" })
		}, pipelines.CodeChannelMissing, missing},
		{"another person's channel of that name", func(t *testing.T, dh *driveHarness) {
			dh.setChannel(t, releasesID, func(c *Channel) { c.OwnerUserID = "v1:identity:user:" + otherID })
		}, pipelines.CodeChannelMissing, missing},
		{"an archived channel", func(t *testing.T, dh *driveHarness) {
			dh.setChannel(t, releasesID, func(c *Channel) { c.Status = "archived" })
		}, pipelines.CodeChannelArchived,
			"Channel releases is archived, so it delivers nothing. Name an active channel in the notify stage, or create one in Deployables > Settings > Channels, then re-run."},
		{"a channel that does not accept this pipeline", func(t *testing.T, dh *driveHarness) {
			dh.allowChannels("v1:pipelines:channel:ch-elsewhere")
		}, pipelines.CodeChannelNotAllowed,
			"Channel releases exists, but pipeline shop is not one it accepts. Allow the pipeline on the channel in Deployables > Settings > Channels, then re-run."},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, notifyManifest)
			c.arrange(t, dh)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			args := dh.receiptOf(t, "notify.notify")
			if argString(args, "status") != WorkStepFailed || argString(args, "errorCode") != c.code || argString(args, "errorMessage") != c.message {
				t.Errorf("receipt = %v, want %s %q", args, c.code, c.message)
			}
			if staged, asked := dh.store.stagedNotifications(), dh.asked.count("DISCORD_RELEASES"); len(staged) != 0 || asked != 0 {
				t.Errorf("nothing is staged and no secret resolved: %d staged, %d resolutions", len(staged), asked)
			}
			if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
				t.Errorf("a notification that cannot go fails the run: %s", got.Conclusion)
			}
		})
	}
}

// A Discord channel names the globalSecret holding its webhook's URL. The
// name is held to the outbound worker's rule and kept out of the platform's
// namespace BEFORE anything is resolved; the value is resolved only to check
// that it is a Discord webhook's URL, and no failure repeats it.
func TestADiscordChannelThatCannotNameAWebhookSendsNothing(t *testing.T) {
	for _, c := range []struct {
		name      string
		secretRef string
		value     string // "" stores none
		code      string
		message   string
		never     []string // in the message, or anywhere a person reads
		asked     int
	}{
		{"a value that is not a Discord webhook", "DISCORD_RELEASES", "https://example.test/hook", pipelines.CodeChannelInvalid,
			"The value stored for DISCORD_RELEASES, which channel releases names, is not a Discord webhook URL, so nothing was sent. Store the channel's webhook URL under that name, then re-run.",
			[]string{"https://example.test/hook", "example.test/hook"}, 1},
		{"no secret named", "", "", pipelines.CodeChannelInvalid,
			"Channel releases delivers to Discord and names no secret holding its webhook URL, so nothing was sent. Name the globalSecret holding the URL on the channel, then re-run.",
			nil, 0},
		{"a name that breaks the rule", "lower_case", "", pipelines.CodeChannelInvalid,
			"Channel releases names a secret whose name does not match ^[A-Z][A-Z0-9_]{0,63}$, so it was not resolved and nothing was sent. Name the globalSecret holding the webhook URL on the channel, then re-run.",
			[]string{"lower_case"}, 0},
		{"the webhook's URL where its name belongs", discordHook, "", pipelines.CodeChannelInvalid,
			"Channel releases names a secret whose name does not match ^[A-Z][A-Z0-9_]{0,63}$, so it was not resolved and nothing was sent. Name the globalSecret holding the webhook URL on the channel, then re-run.",
			[]string{discordHook, hookToken}, 0},
		{"a name in the platform's namespace", "MEMQL_MASTER_KEY", "", pipelines.CodeChannelInvalid,
			"Channel releases names MEMQL_MASTER_KEY, in the platform's own MEMQL_ namespace, which a channel never reads; it was not resolved and nothing was sent. Name the globalSecret holding the webhook URL on the channel, then re-run.",
			nil, 0},
		{"no value stored", "DISCORD_RELEASES", "", pipelines.CodeSecretMissing,
			"No value is stored on this cluster for DISCORD_RELEASES, which channel releases names. Store the channel's Discord webhook URL under that name, then re-run.",
			nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, notifyManifest)
			dh.setChannel(t, releasesID, func(ch *Channel) { ch.SecretRef = c.secretRef })
			delete(dh.secrets, "DISCORD_RELEASES")
			if c.value != "" {
				dh.secrets[c.secretRef] = c.value
			}
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			args := dh.receiptOf(t, "notify.notify")
			if argString(args, "status") != WorkStepFailed || argString(args, "errorCode") != c.code || argString(args, "errorMessage") != c.message {
				t.Errorf("receipt = %v\nwant %s %q", args, c.code, c.message)
			}
			if message := argString(args, "errorMessage"); c.value != "" && strings.Contains(message, "example.test") {
				t.Errorf("the message names the value's host: %q", message)
			}
			if staged := dh.store.stagedNotifications(); len(staged) != 0 {
				t.Errorf("nothing is staged: %+v", staged)
			}
			for _, name := range dh.asked.all() {
				if name != "SHOP_TOKEN" && name != "DISCORD_RELEASES" {
					t.Errorf("the resolver was asked for %q, a name no rule admits", name)
				}
			}
			if n := dh.asked.count("DISCORD_RELEASES"); n != c.asked {
				t.Errorf("the resolver was asked %d times for the channel's secret, want %d", n, c.asked)
			}
			dh.assertUnpublished(t, c.never...)
		})
	}
}

func TestOnlyADiscordWebhookURLPasses(t *testing.T) {
	for value, want := range map[string]bool{
		"https://discord.com/api/webhooks/1/abc":           true,
		"https://discordapp.com/api/webhooks/1/abc":        true,
		"https://ptb.discord.com/api/webhooks/1/abc":       true,
		"https://canary.discord.com/api/webhooks/1/abc":    true,
		"https://Discord.com/api/webhooks/1/abc":           true,
		"https://discord.com/api/webhooks/1/abc?wait=true": true,
		"https://discord.com/api/webhooks/1/abc\n":         true, // a stored value often ends in a newline
		"http://discord.com/api/webhooks/1/abc":            false,
		"https://discord.com:8443/api/webhooks/1/abc":      false,
		"https://evil.test/api/webhooks/1/abc":             false,
		"https://discord.com.evil.test/api/webhooks/1/abc": false,
		"https://evil.test/discord.com/api/webhooks/1/abc": false,
		"https://user:pw@discord.com/api/webhooks/1/abc":   false,
		"https://discord.com@evil.test/api/webhooks/1/abc": false,
		"https://discord.com/api/channels/1/messages":      false,
		"https://discord.com/api/webhooks/":                false,
		"https://discord.com/api/webhooks/../../users/@me": false,
		"https://discord.com/api/webhooks/1/%2e%2e/x":      false,
		"https://discord.com/api/webhooks/1/a b":           false,
		"https://discord.com/api/webhooks/1/a\tb":          false,
		"discord.com/api/webhooks/1/abc":                   false,
		"https://example.test/hook":                        false,
		"":                                                 false,
	} {
		if got := isDiscordWebhookURL(value); got != want {
			t.Errorf("isDiscordWebhookURL(%q) = %v, want %v", value, got, want)
		}
	}
}

// The rule a channel's secret name is held to is the outbound worker's, which
// stageOutboundRequestToSecret's own @pattern spells: a name this stage let
// through and the mutation refused would fail every delivery naming it.
func TestAChannelsSecretIsHeldToTheOutboundWorkersNameRule(t *testing.T) {
	src, err := os.ReadFile("../../dsl/platform/mutations.memql")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)\nmutation outboundRequest stageOutboundRequestToSecret \{.*?\n\}`).Find(src)
	m := regexp.MustCompile(`(?m)^\s*targetSecret\s+string!\s+@pattern\("([^"]*)"\)`).FindSubmatch(block)
	if m == nil {
		t.Fatal("stageOutboundRequestToSecret's targetSecret carries no @pattern: this test reads the wrong file, or the rule moved")
	}
	if got := string(m[1]); got != channelSecretNamePattern {
		t.Errorf("the mutation holds a secret target's name to %q and the notify stage to %q", got, channelSecretNamePattern)
	}
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

func TestAnEmailChannelSendsOneRowPerRecipient(t *testing.T) {
	dh := newNotifyHarness(t, opsManifest)
	dh.addOps("ada@example.test", " bob@example.test ", "ADA@example.test", "")
	dh.deliverAt(1)
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	staged := dh.store.stagedNotifications()
	var targets, ids []string
	for _, row := range staged {
		targets = append(targets, row.Target)
		ids = append(ids, row.RequestID)
		if row.Medium != "email" || row.TargetSecret != "" || row.Subject != "shop · Push to main passed" ||
			!strings.Contains(row.Body, "Deployment details: "+pipelines.RunPageURL(testOSOrigin, bareID(run.ID))+"\n") ||
			!notifyRequestIDRe.MatchString(row.RequestID) || row.DedupeKey != row.RequestID || row.RequestedBy != "pipelines:notify:"+bareID(run.ID) {
			t.Errorf("an email row: %+v", row)
		}
	}
	if !slices.Equal(targets, []string{"ada@example.test", "bob@example.test"}) {
		t.Errorf("one row per distinct recipient, trimmed: %q", targets)
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		t.Errorf("each row has an id of its own: %q", ids)
	}
	args := dh.receiptOf(t, "notify.notify")
	result := argObject(args, "result")
	if argString(args, "status") != WorkStepDone || argString(args, "errorMessage") != "Delivered to ops (email, 2 recipients)." ||
		result["kind"] != "email" || !slices.Equal(argStrings(result, "requestIds"), ids) {
		t.Errorf("receipt = %v", args)
	}
	if asked := dh.asked.all(); slices.Contains(asked, "DISCORD_RELEASES") {
		t.Errorf("an email channel resolves no secret: %v", asked)
	}
}

func TestAnEmailChannelWithoutBareAddressesSendsNothing(t *testing.T) {
	for _, c := range []struct {
		name       string
		recipients []string
		message    string
		never      []string
	}{
		{"a display name", []string{"ada@example.test", "Bob <bob@example.test>"},
			"Recipient 2 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"bob@example.test", "Bob <"}},
		{"a display name with no space", []string{"Bob<bob@example.test>"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"bob@example.test"}},
		{"angle brackets", []string{"<bob@example.test>"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"bob@example.test"}},
		{"a comment", []string{"bob@example.test(ops)"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"bob@example.test"}},
		{"a quoted local part", []string{`"bob smith"@example.test`},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"bob smith"}},
		{"two addresses", []string{"ada@example.test,eve@evil.test"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"eve@evil.test"}},
		{"a header smuggled into an address", []string{"ada@example.test\r\nBcc: eve@evil.test"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"eve@evil.test"}},
		{"not an address at all", []string{"ops-team"},
			"Recipient 1 of channel ops is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.",
			[]string{"ops-team"}},
		{"no recipients", nil,
			"Channel ops delivers by email and lists no recipients, so nothing was sent. Add its recipients in Deployables > Settings > Channels, then re-run.", nil},
		{"only blanks", []string{" ", ""},
			"Channel ops delivers by email and lists no recipients, so nothing was sent. Add its recipients in Deployables > Settings > Channels, then re-run.", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, opsManifest)
			dh.addOps(c.recipients...)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			args := dh.receiptOf(t, "notify.notify")
			if argString(args, "errorCode") != pipelines.CodeChannelInvalid || argString(args, "errorMessage") != c.message {
				t.Errorf("receipt = %v\nwant %q", args, c.message)
			}
			if staged := dh.store.stagedNotifications(); len(staged) != 0 {
				t.Errorf("nothing is staged: %+v", staged)
			}
			dh.assertUnpublished(t, c.never...)
		})
	}
}

// ---------------------------------------------------------------------------
// Outcomes the rows report
// ---------------------------------------------------------------------------

// A delivery the outbound worker gave up on fails the step with the worker's
// own error -- masked, because a transport's error can carry the URL it
// failed on.
func TestAFailedDeliveryFailsTheStepWithTheWorkersErrorMasked(t *testing.T) {
	for _, c := range []struct {
		name      string
		attempts  int
		lastError string
		message   string
	}{
		{"after its attempts", 3, "webhook: POST " + discordHook + ": status 404",
			"Delivery to releases failed after 3 attempts: webhook: POST ***: status 404."},
		{"after one attempt", 1, "webhook: status 401.",
			"Delivery to releases failed after 1 attempt: webhook: status 401."},
		{"refused before any attempt", 0, "webhook: target not in allowlist",
			"Delivery to releases was refused by the outbound worker before any attempt: webhook: target not in allowlist."},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, notifyManifest)
			dh.moveAt(1, "failed", c.attempts, c.lastError)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			args := dh.receiptOf(t, "notify.notify")
			if argString(args, "status") != WorkStepFailed || argString(args, "errorCode") != pipelines.CodeNotifyFailed ||
				argString(args, "errorMessage") != c.message {
				t.Errorf("receipt = %v\nwant %q", args, c.message)
			}
			got, _ := dh.store.run(run.ID)
			if got.Conclusion != ConclusionFailure {
				t.Errorf("run = %s", got.Conclusion)
			}
			if last := dh.lastUpdate(t).Run; last.Output.Title != "Failed at notify: notify.notify" {
				t.Errorf("check run title %q", last.Output.Title)
			}
			dh.assertNowhere(t, discordHook, hookToken)
		})
	}
}

// A check run can be public. A failure names a recipient by its place in the
// channel and never by its address, and the worker's error is masked of every
// address the channel lists.
func TestAFailedEmailDeliveryNamesNoAddress(t *testing.T) {
	dh := newNotifyHarness(t, opsManifest)
	dh.addOps("ada@example.test", "bob@example.test")
	dh.store.mu.Lock()
	dh.store.onOutboundRead = func(read int, ids []string) {
		if read == 1 && len(ids) == 2 {
			dh.store.setOutbound(ids[0], "sent", 1, "")
			dh.store.setOutbound(ids[1], "failed", 1, `target "bob@example.test" not in email allowlist`)
		}
	}
	dh.store.mu.Unlock()
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	args := dh.receiptOf(t, "notify.notify")
	want := `Delivery to ops failed after 1 attempt: target "***" not in email allowlist (recipient 2 of 2; 1 of 2 delivered so far).`
	if argString(args, "errorCode") != pipelines.CodeNotifyFailed || argString(args, "errorMessage") != want {
		t.Errorf("receipt = %v\nwant %q", args, want)
	}
	dh.assertUnpublished(t, "ada@example.test", "bob@example.test")
}

// A delivery that has not arrived when the step's time runs out fails the step
// undelivered, saying where the delivery stood -- and never that the worker
// keeps trying when its row says otherwise.
func TestANotificationNotDeliveredInTimeSaysWhereItStood(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(dh *driveHarness)
		message string
	}{
		{"never attempted", func(*driveHarness) {},
			"Not delivered to releases within 1s; the outbound worker has not attempted it yet, and it stays queued after this step ends until a node configured to send webhooks takes it."},
		{"still retrying", func(dh *driveHarness) { dh.moveAt(1, "retrying", 2, "webhook: status 502 from "+discordHook) },
			"Not delivered to releases within 1s; the outbound worker is still retrying (attempt 2: webhook: status 502 from ***) and keeps trying after this step ends."},
		{"stranded mid-send", func(dh *driveHarness) { dh.moveAt(1, "sending", 0, "") },
			"Not delivered to releases within 1s; the delivery is still marked as being sent, and the worker that claimed it has not concluded."},
		{"never read", func(dh *driveHarness) {
			dh.store.mu.Lock()
			dh.store.failOutboundRead = errors.New("engine unavailable")
			dh.store.mu.Unlock()
		}, "Not delivered to releases within 1s; the state of its delivery could not be read (engine unavailable), though it was handed to the outbound worker, which may still deliver it."},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, notifyManifest)
			dh.integ.Configure(func(d *Deps) { d.notifyTimeout = time.Second })
			c.arrange(dh)
			run := dh.openRun(t, pushOpening())
			began := time.Now()
			deliver(t, dh.integ, run)
			if waited := time.Since(began); waited < time.Second {
				t.Errorf("the stage stopped waiting after %v, before its timeout", waited)
			}

			args := dh.receiptOf(t, "notify.notify")
			if argString(args, "status") != WorkStepFailed || argString(args, "errorCode") != pipelines.CodeNotifyUndelivered ||
				argString(args, "errorMessage") != c.message {
				t.Errorf("receipt = %v\nwant %q", args, c.message)
			}
			if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionFailure {
				t.Errorf("run = %s", got.Conclusion)
			}
			dh.assertNowhere(t, discordHook, hookToken)
		})
	}
}

// RECOVERED is a pass after the newest completed run of the same pipeline and
// event, queued before this one, did not pass.
func TestANotificationSaysTheRunRecoveredAfterAFailure(t *testing.T) {
	prior := func(id string, event pipelines.Event, status, conclusion string, queued time.Duration) Run {
		return Run{
			ID: id, OwnerUserID: testPipeline("").OwnerUserID, PipelineID: PipelineIDFor(packageID), Event: event,
			Status: status, Conclusion: conclusion, QueuedAt: testNow.Add(queued),
		}
	}
	for _, c := range []struct {
		name    string
		before  []Run
		failErr error
		want    string
	}{
		{"no run before it", nil, nil, "passed"},
		{"the previous push failed", []Run{prior("p1", pipelines.EventPush, StatusCompleted, ConclusionFailure, -time.Hour)}, nil, "recovered"},
		{"the previous push was cancelled", []Run{prior("p1", pipelines.EventPush, StatusCompleted, ConclusionCancelled, -time.Hour)}, nil, "recovered"},
		{"the previous push passed", []Run{prior("p1", pipelines.EventPush, StatusCompleted, ConclusionSuccess, -time.Hour)}, nil, "passed"},
		{"the newest previous push passed, an older one failed", []Run{
			prior("p1", pipelines.EventPush, StatusCompleted, ConclusionSuccess, -time.Hour),
			prior("p2", pipelines.EventPush, StatusCompleted, ConclusionFailure, -2*time.Hour),
		}, nil, "passed"},
		{"a push still running is looked past to a failure", []Run{
			prior("p1", pipelines.EventPush, StatusInProgress, "", -time.Hour),
			prior("p2", pipelines.EventPush, StatusCompleted, ConclusionFailure, -2*time.Hour),
		}, nil, "recovered"},
		// A run with no conclusion yet is no failure: what came before it
		// passed, and so did this one.
		{"a push still running is looked past to a pass", []Run{
			prior("p1", pipelines.EventPush, StatusInProgress, "", -time.Hour),
			prior("p2", pipelines.EventPush, StatusCompleted, ConclusionSuccess, -2*time.Hour),
		}, nil, "passed"},
		{"a failed push queued after this one", []Run{prior("p1", pipelines.EventPush, StatusCompleted, ConclusionFailure, time.Hour)}, nil, "passed"},
		{"a failed pull request", []Run{prior("p1", pipelines.EventPullRequest, StatusCompleted, ConclusionFailure, -time.Hour)}, nil, "passed"},
		{"the history cannot be read", []Run{prior("p1", pipelines.EventPush, StatusCompleted, ConclusionFailure, -time.Hour)},
			errors.New("engine unavailable"), "passed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, notifyManifest)
			for _, r := range c.before {
				dh.store.addRun(r)
			}
			dh.store.mu.Lock()
			dh.store.failPreviousRuns = c.failErr
			dh.store.mu.Unlock()
			dh.deliverAt(1)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)

			staged := dh.store.stagedNotifications()
			if len(staged) != 1 {
				t.Fatalf("staged %d", len(staged))
			}
			e := decodeDiscord(t, staged[0].Body).Embeds[0]
			if want := "shop · Push to main " + c.want; e.Title != want {
				t.Errorf("title %q, want %q", e.Title, want)
			}
			if c.want == "recovered" && !strings.HasSuffix(e.Description, "\nPassed after the previous run failed. All 2 stages passed.") {
				t.Errorf("description %q", e.Description)
			}
			if c.failErr != nil && !strings.Contains(dh.logs.String(), "could not read the pipeline's previous runs") {
				t.Errorf("a history the stage could not read is said in the log:\n%s", dh.logs.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Across replicas
// ---------------------------------------------------------------------------

// fakeOutbox is the outbound worker on ANOTHER replica. It shares nothing with
// the driver but the store -- no hook, no channel, no memory -- and moves each
// row it finds as the real worker does: claimed (sending), then delivered.
type fakeOutbox struct {
	store     *memStore
	stop      chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	delivered []string
}

func startFakeOutbox(store *memStore) *fakeOutbox {
	o := &fakeOutbox{store: store, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-o.stop:
				return
			case <-tick.C:
			}
			for _, row := range o.store.outboundRows() {
				switch row.State.Status {
				case "pending":
					o.store.setOutbound(row.State.ID, "sending", 0, "")
				case "sending":
					o.store.setOutbound(row.State.ID, "sent", 1, "")
					o.mu.Lock()
					o.delivered = append(o.delivered, row.State.ID)
					o.mu.Unlock()
				}
			}
		}
	}()
	return o
}

func (o *fakeOutbox) close() {
	close(o.stop)
	<-o.done
}

func (o *fakeOutbox) deliveredIDs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.delivered)
}

// THE HOP: the replica that staged a notification is not the one that
// delivers it, and the two share the row and nothing else. The driver
// concludes from the row alone.
func TestTheNotifyStageConcludesFromTheRowAlone(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	outbox := startFakeOutbox(dh.store)
	defer outbox.close()
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)

	staged := dh.store.stagedNotifications()
	if len(staged) != 1 {
		t.Fatalf("staged %d", len(staged))
	}
	if delivered := outbox.deliveredIDs(); !slices.Equal(delivered, []string{staged[0].RequestID}) {
		t.Errorf("the other replica's worker delivered %v, want the staged row", delivered)
	}
	args := dh.receiptOf(t, "notify.notify")
	if result := argObject(args, "result"); argString(args, "status") != WorkStepDone ||
		!slices.Equal(argStrings(result, "requestIds"), []string{staged[0].RequestID}) {
		t.Errorf("receipt = %v", args)
	}
	if reads := dh.store.reads(); reads < 1 {
		t.Errorf("the driver concluded without reading the row")
	}
}

// storeTap is a Store that records the ids each OutboundStatuses call asks
// for, passing every call through: one replica's reads, told apart from
// another's over the same rows.
type storeTap struct {
	Store
	mu    sync.Mutex
	reads [][]string
}

func (s *storeTap) OutboundStatuses(ctx context.Context, ids []string) ([]OutboundStatus, error) {
	s.mu.Lock()
	s.reads = append(s.reads, slices.Clone(ids))
	s.mu.Unlock()
	return s.Store.OutboundStatuses(ctx, ids)
}

func (s *storeTap) asked() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reads)
}

// A drive resumed after its predecessor lost the lease re-sends the notify
// step with the same attempt, stages rows of its OWN, and concludes from them
// alone: the row its predecessor staged reads `sent`, and is never taken for
// this drive's delivery. (A notification can then arrive twice after a driver
// stops mid-delivery -- at least once, never falsely confirmed.)
func TestAResumedNotifyStageConcludesFromItsOwnRowsAlone(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	run := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	waitFor(t, "agent-a to stage its notification", func() bool { return len(dh.store.stagedNotifications()) == 1 })
	first := dh.store.stagedNotifications()[0].RequestID

	// agent-a stops renewing, and agent-b takes the run over.
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()
	tap := &storeTap{Store: dh.store}
	other := New(Deps{Store: tap, GitHub: dh.github, Gate: dh.gate.run,
		OSOrigin: func() string { return testOSOrigin }, Now: func() time.Time { return testNow }})
	dh.configureDriver(other, "agent-b")
	if err := other.RecoverRuns(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	waitFor(t, "agent-b to stage a notification of its own", func() bool { return len(dh.store.stagedNotifications()) == 2 })
	second := dh.store.stagedNotifications()[1].RequestID
	// agent-a has lost the run, and its fence before each read of its row
	// says so: it stops waiting at once, not when its time runs out.
	tookOver := time.Now()
	waitDrives(t, dh.integ)
	if stopped := time.Since(tookOver); stopped > 5*time.Second {
		t.Errorf("the replica that lost the run kept waiting on its delivery for %v", stopped)
	}

	// The predecessor's row is delivered. It is not this drive's.
	dh.store.setOutbound(first, "sent", 1, "")
	waitFor(t, "agent-b to read its row ten times more, or to conclude", func() bool {
		if len(dh.work.receiptsOf("notify.notify")) > 0 {
			return true
		}
		n := 0
		for _, ids := range tap.asked() {
			if slices.Contains(ids, second) {
				n++
			}
		}
		return n >= 10
	})
	if receipts := dh.work.receiptsOf("notify.notify"); len(receipts) != 0 {
		t.Fatalf("a drive concluded the notify step from a row it did not stage: %+v", receipts)
	}
	dh.store.setOutbound(second, "failed", 2, "webhook: status 404")
	waitDrives(t, other)

	if first == second || !notifyRequestIDRe.MatchString(second) {
		t.Errorf("the resumed drive stages under a fresh id: %q then %q", first, second)
	}
	for _, ids := range tap.asked() {
		if slices.Contains(ids, first) {
			t.Errorf("the resumed drive read its predecessor's row: %v", ids)
		}
	}
	args := dh.receiptOf(t, "notify.notify")
	if argString(args, "errorCode") != pipelines.CodeNotifyFailed ||
		argString(args, "errorMessage") != "Delivery to releases failed after 2 attempts: webhook: status 404." {
		t.Errorf("the resumed drive reports its own row: %v", args)
	}
	got, _ := dh.store.run(run.ID)
	if got.DriverNodeID != "agent-b" || got.Conclusion != ConclusionFailure {
		t.Errorf("run %s by %q", got.Conclusion, got.DriverNodeID)
	}
	if row := dh.work.stepRow("notify.notify"); fmt.Sprint(row["attempt"]) != "1" {
		t.Errorf("the notify step is re-sent with the same attempt: %v", row["attempt"])
	}
	// The predecessor ran the tests step; its artifact is on its row, and the
	// resumed drive's message lists it.
	if files := decodeDiscord(t, dh.store.stagedNotifications()[1].Body).fields()["Artifacts"]; !strings.Contains(files, "file-art-tests.unit") {
		t.Errorf("the resumed drive's message lists the predecessor's artifacts: %q", files)
	}
}

// A failed-only re-run sends its notification again: what the first attempt
// delivered announced the first attempt, and this one has its own outcome --
// here, that the pipeline recovered.
func TestARerunOfFailedStepsAnnouncesItsOwnOutcome(t *testing.T) {
	dh := newNotifyHarness(t, `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: tests
      steps:
        - name: unit
          run: go test ./...
    - name: notify
      on: [push]
      channel: releases
`)
	var failUnit sync.Mutex
	failing := true
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		failUnit.Lock()
		defer failUnit.Unlock()
		if req.StepKey == "tests.unit" && failing {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
		}
		return res, nil
	}
	dh.deliverEvery()
	first := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, first)
	if staged := dh.store.stagedNotifications(); len(staged) != 1 ||
		decodeDiscord(t, staged[0].Body).Embeds[0].Title != "shop · Push to main failed at tests" {
		t.Fatalf("the first attempt announces its failure: %+v", staged)
	}

	failUnit.Lock()
	failing = false
	failUnit.Unlock()
	// An hour later, so the first attempt was queued before the re-run.
	dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(time.Hour) } })
	next, err := dh.integ.Rerun(personCtx(ownerID), first.ID, true)
	if err != nil {
		t.Fatalf("rerun failed steps: %v", err)
	}
	deliver(t, dh.integ, next)

	staged := dh.store.stagedNotifications()
	if len(staged) != 2 {
		t.Fatalf("the re-run sends a notification of its own: %d staged", len(staged))
	}
	e := decodeDiscord(t, staged[1].Body).Embeds[0]
	if e.Title != "shop · Push to main recovered" {
		t.Errorf("the re-run's title %q", e.Title)
	}
	// The checks stage was carried, not run: a pass it counts is one this
	// attempt ran.
	if !strings.HasSuffix(e.Description, "\nPassed after the previous run failed. All 1 stage passed.") {
		t.Errorf("the re-run's description %q", e.Description)
	}
	got, _ := dh.store.run(next.ID)
	for _, row := range dh.work.stepRows(bareID(got.WorkRunID)) {
		if row["key"] == "checks.vet" && row["errorCode"] != pipelines.CodePassedEarlier {
			t.Errorf("what passed is still carried: %v", row)
		}
	}
}

// ---------------------------------------------------------------------------
// The message's links, and the rows' ids
// ---------------------------------------------------------------------------

// With no MemQL OS domain there is no run page and no file to open: the
// message says so, and links nothing.
func TestWithNoOSDomainTheMessageLinksNothing(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	dh.integ.Configure(func(d *Deps) { d.OSOrigin = func() string { return "" } })
	dh.store.addFile(dh.p.OwnerUserID, "file-art-tests.unit", "coverage.html")
	dh.deliverAt(1)
	deliver(t, dh.integ, dh.openRun(t, pushOpening()))

	msg := decodeDiscord(t, dh.onlyStaged(t).Body)
	fields := msg.fields()
	if msg.Embeds[0].URL != "" || fields["Deployment details"] != "Not available: this cluster has no MemQL OS domain configured." {
		t.Errorf("url %q, details %q", msg.Embeds[0].URL, fields["Deployment details"])
	}
	if _, ok := fields["MemQL OS"]; ok {
		t.Errorf("no OS link without an OS domain: %v", fields)
	}
	if fields["Artifacts"] != "coverage.html\nartifact 2" {
		t.Errorf("artifacts %q", fields["Artifacts"])
	}
}

// A message lists five files and counts the rest, so the Library is asked for
// the names of five.
func TestOnlyTheFilesAMessageListsAreNamed(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "tests.unit" {
			res.ArtifactFileIDs = []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"}
		}
		return res, nil
	}
	tap := &namesTap{Store: dh.store}
	dh.integ.Configure(func(d *Deps) { d.Store = tap })
	dh.deliverAt(1)
	deliver(t, dh.integ, dh.openRun(t, pushOpening()))

	if asked := tap.asked(); len(asked) != 1 || !slices.Equal(asked[0], []string{"f1", "f2", "f3", "f4", "f5"}) {
		t.Errorf("the Library is asked for the names it will show: %v", asked)
	}
	files := decodeDiscord(t, dh.onlyStaged(t).Body).fields()["Artifacts"]
	if !strings.HasSuffix(files, "and 3 more on the run page") {
		t.Errorf("artifacts %q", files)
	}
}

// namesTap records the ids each LibraryFileNames call asks for.
type namesTap struct {
	Store
	mu    sync.Mutex
	calls [][]string
}

func (s *namesTap) LibraryFileNames(ctx context.Context, owner string, ids []string) (map[string]string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, slices.Clone(ids))
	s.mu.Unlock()
	return s.Store.LibraryFileNames(ctx, owner, ids)
}

func (s *namesTap) asked() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// A notification half handed over says so: what was staged may still arrive.
func TestAStagingThatFailsSaysWhatWasHandedOver(t *testing.T) {
	for _, c := range []struct {
		name    string
		failAt  int
		message string
	}{
		{"the first row", 1, "Nothing was sent to ops: handing the notification to the outbound worker failed (memQ). Re-run to try again."},
		{"the second row", 2, "Delivery to ops was cut short: 1 of 2 deliveries had been handed to the outbound worker when handing over the next failed (memQ); those may still arrive."},
	} {
		t.Run(c.name, func(t *testing.T) {
			dh := newNotifyHarness(t, opsManifest)
			dh.addOps("ada@example.test", "bob@example.test")
			dh.store.mu.Lock()
			dh.store.failStageAt = c.failAt
			dh.store.mu.Unlock()
			deliver(t, dh.integ, dh.openRun(t, pushOpening()))

			args := dh.receiptOf(t, "notify.notify")
			message := argString(args, "errorMessage")
			// The store's error names the row it failed on; the message
			// carries it as the store said it.
			message = regexp.MustCompile(`\(memStore: staging pn[0-9a-f]{32} failed\)`).ReplaceAllString(message, "(memQ)")
			if argString(args, "errorCode") != pipelines.CodeNotifyFailed || message != c.message {
				t.Errorf("receipt = %v\nwant %q", args, c.message)
			}
			if n := len(dh.store.stagedNotifications()); n != c.failAt-1 {
				t.Errorf("staged %d rows, want %d", n, c.failAt-1)
			}
			if reads := dh.store.reads(); reads != 0 {
				t.Errorf("a stage that failed waits on nothing: %d reads", reads)
			}
		})
	}
}

func TestNotifyRequestIDsAreRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newNotifyRequestID()
		if err != nil {
			t.Fatal(err)
		}
		if !notifyRequestIDRe.MatchString(id) || seen[id] {
			t.Fatalf("id %d: %q (seen before: %v)", i, id, seen[id])
		}
		seen[id] = true
	}
}

// The stage reads the outbox two seconds after it staged, then each wait half
// as long again as the last, up to fifteen seconds.
func TestTheNotifyStagePollsAtTwoSecondsGrowingToFifteen(t *testing.T) {
	want := []time.Duration{
		2 * time.Second, 3 * time.Second, 4500 * time.Millisecond, 6750 * time.Millisecond,
		10125 * time.Millisecond, 15 * time.Second, 15 * time.Second, 15 * time.Second,
	}
	for n, w := range want {
		if got := notifyPollAfter(n); got != w {
			t.Errorf("poll %d waits %v, want %v", n, got, w)
		}
	}
	if got := notifyPollAfter(1000); got != 15*time.Second {
		t.Errorf("a late poll waits %v", got)
	}
}

// A cancel that lands while the stage is still reading its channel stops it
// before anything is staged: the cancel is asked once more right before the
// first row is handed over.
func TestACancelThatLandsMidStepStagesNothing(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	run := dh.openRun(t, pushOpening())
	cancelling := &cancelOnChannelRead{Store: dh.store, cancel: func() {
		if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}}
	dh.integ.Configure(func(d *Deps) { d.Store = cancelling })
	deliver(t, dh.integ, run)

	if !cancelling.fired() {
		t.Fatalf("the channel was never read, so the cancel never landed mid-step")
	}
	if staged := dh.store.stagedNotifications(); len(staged) != 0 {
		t.Errorf("a run cancelled mid-step staged %d notifications", len(staged))
	}
	if args := dh.receiptOf(t, "notify.notify"); argString(args, "status") != WorkStepCancelled ||
		argString(args, "errorMessage") != "The run was cancelled before this step finished." {
		t.Errorf("receipt = %v", args)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// cancelOnChannelRead asks for the run's cancel as the notify stage reads its
// channel, and then answers the read.
type cancelOnChannelRead struct {
	Store
	cancel func()
	mu     sync.Mutex
	done   bool
}

func (s *cancelOnChannelRead) ChannelForOwnerByName(ctx context.Context, owner, name string) (*Channel, error) {
	s.mu.Lock()
	first := !s.done
	s.done = true
	s.mu.Unlock()
	if first {
		s.cancel()
	}
	return s.Store.ChannelForOwnerByName(ctx, owner, name)
}

func (s *cancelOnChannelRead) fired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// A read that answers somebody else's channel -- a store that broke its own
// owner conjunct -- is not a channel this pipeline delivers to: the stage
// checks the owner itself.
func TestAChannelOfAnotherOwnerIsNeverDeliveredTo(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	dh.setChannel(t, releasesID, func(c *Channel) { c.OwnerUserID = "v1:identity:user:" + otherID })
	dh.integ.Configure(func(d *Deps) { d.Store = &anyOwnersChannel{memStore: dh.store} })
	deliver(t, dh.integ, dh.openRun(t, pushOpening()))

	if args := dh.receiptOf(t, "notify.notify"); argString(args, "errorCode") != pipelines.CodeChannelMissing {
		t.Errorf("receipt = %v", args)
	}
	if staged, asked := dh.store.stagedNotifications(), dh.asked.count("DISCORD_RELEASES"); len(staged) != 0 || asked != 0 {
		t.Errorf("another owner's channel is neither delivered to nor resolved: %d staged, %d resolutions", len(staged), asked)
	}
}

// anyOwnersChannel answers a channel by its name whoever owns it.
type anyOwnersChannel struct{ *memStore }

func (s *anyOwnersChannel) ChannelForOwnerByName(_ context.Context, _, name string) (*Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.channels {
		if c.Name == name {
			return &c, nil
		}
	}
	return nil, nil
}

// A read of the rows decides a delivery only about the rows asked for: sent
// when every one reads sent, failed when one reads failed, and nothing at all
// from a read that is not of them.
func TestADeliveryIsJudgedOnlyOnTheRowsItStaged(t *testing.T) {
	ids := []string{"pn1", "pn2"}
	row := func(id, status string) OutboundStatus { return OutboundStatus{ID: id, Status: status} }
	for _, c := range []struct {
		name     string
		ids      []string
		statuses []OutboundStatus
		want     deliveryEnd
	}{
		{"every row sent", ids, []OutboundStatus{row("pn1", "sent"), row("pn2", "sent")}, deliverySent},
		{"one row sent, one sending", ids, []OutboundStatus{row("pn1", "sent"), row("pn2", "sending")}, deliveryWaiting},
		{"one row failed after one sent", ids, []OutboundStatus{row("pn1", "sent"), row("pn2", "failed")}, deliveryFailed},
		{"one row failed before one pending", ids, []OutboundStatus{row("pn1", "failed"), row("pn2", "pending")}, deliveryFailed},
		{"a row that is not there", ids, []OutboundStatus{row("pn1", "sent"), row("pn2", "")}, deliveryWaiting},
		{"a sent row under another id", ids, []OutboundStatus{row("pn1", "sent"), row("pn9", "sent")}, deliveryWaiting},
		{"a failed row under another id", ids, []OutboundStatus{row("pn9", "failed"), row("pn2", "sent")}, deliveryWaiting},
		{"fewer answers than rows", ids, []OutboundStatus{row("pn1", "sent")}, deliveryWaiting},
		{"no rows at all", nil, nil, deliveryWaiting},
		{"canonical and bare spellings of one row", []string{"v1:platform:outboundRequest:pn1"}, []OutboundStatus{row("pn1", "sent")}, deliverySent},
	} {
		if got := deliveryOf(c.ids, c.statuses); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A secret's value can be any word -- even one the message's JSON is made of.
// The message's parts are masked before they are composed, and the encoded
// body never is: a mask over it would cut a key out of every message.
func TestASecretThatSpellsAPartOfTheMessageLeavesItWhole(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	dh.secrets["SHOP_TOKEN"] = "value"
	dh.deliverAt(1)
	deliver(t, dh.integ, dh.openRun(t, pushOpening()))

	fields := decodeDiscord(t, dh.onlyStaged(t).Body).fields()
	if fields["Version"] != "1111111" || fields["Deployment details"] == "" {
		t.Errorf("the message lost its fields: %v", fields)
	}
	if args := dh.receiptOf(t, "notify.notify"); argString(args, "status") != WorkStepDone {
		t.Errorf("receipt = %v", args)
	}
}

// A cancel that lands while an email channel's rows are being handed over
// hands nothing more over: the recipients already staged may still be sent
// to, and the step says so.
func TestACancelBetweenTwoRowsHandsNothingMoreOver(t *testing.T) {
	dh := newNotifyHarness(t, opsManifest)
	dh.addOps("ada@example.test", "bob@example.test")
	run := dh.openRun(t, pushOpening())
	cancelling := &cancelOnStage{Store: dh.store, cancel: func() {
		if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}}
	dh.integ.Configure(func(d *Deps) { d.Store = cancelling })
	deliver(t, dh.integ, run)

	if staged := dh.store.stagedNotifications(); len(staged) != 1 || staged[0].Target != "ada@example.test" {
		t.Errorf("only the row handed over before the cancel is staged: %+v", staged)
	}
	if args := dh.receiptOf(t, "notify.notify"); argString(args, "status") != WorkStepCancelled ||
		!strings.HasSuffix(argString(args, "errorMessage"), "so it may still arrive.") {
		t.Errorf("receipt = %v", args)
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// cancelOnStage asks for the run's cancel as the first row is staged, once it
// is.
type cancelOnStage struct {
	Store
	cancel func()
	once   sync.Once
}

func (s *cancelOnStage) StageNotification(ctx context.Context, n NotificationRequest) error {
	err := s.Store.StageNotification(ctx, n)
	s.once.Do(s.cancel)
	return err
}
