package pipelinerun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
	"github.com/znasllc-io/memql/component/pipelines"
)

// notify.go -- a notify stage announces its run (epic memql#5480, design
// record D16).
//
// THE DRIVER DELIVERS, NEVER THE STEP RUNNER. A notify step is the driver's
// own: it reads the channel the stage names, composes the message
// (pipelinenotify.DiscordMessage, EmailMessage) with the run page as its report
// link, and stages it on the platform's outbound path -- one
// v1:platform:outboundRequest per destination -- for the outbound worker
// (component/outbound) to deliver. The step's receipt is written after the
// delivery's REAL result: every row sent, one failed, or not there yet when
// the step's time ran out.
//
// A DELIVERY'S STATE IS ON ITS ROW, AND ONLY THERE. The worker that takes a
// row may run on any replica, and has no way to say how a delivery went but
// the row: the stage reads the rows it staged until they say (the read is
// fresh), so it concludes the same whichever replica delivered.
//
// A ROW IS THIS DRIVE'S OWN. Each is staged under a random 128-bit id, never
// one derived from the run, and no existing row is ever adopted. A row keeps
// its delivery state at its id (@createOnly): a derivable id would let anyone
// who can stage a plain row pre-create it `sent`, and would hand a drive
// resumed after a lost lease the row its predecessor staged, read as this
// drive's delivery. So a resumed drive stages rows of its own, and a
// notification can arrive twice when a driver stops mid-delivery -- at least
// once, and never falsely confirmed. And once staged, a row is the server's:
// both stagings mark it serverStaged, and the engine refuses every write to it
// without internal origin (component/memql's protected-row write guard), so no
// client can stamp it `sent`, requeue it, or re-stage it somewhere else, an
// email as much as a Discord webhook. Its state is the worker's alone.
//
// A WEBHOOK'S URL IS A CREDENTIAL. A Discord channel names the globalSecret
// holding it; the row names the same secret, and the worker resolves it at
// send time. The stage resolves it once, only to check that it IS a Discord
// webhook's URL, remembers it for masking and drops it: no row, receipt, log
// line or message carries it. And an outbound row is readable by any
// signed-in reader (memql#5804), so a message says nothing the run page does
// not.

const (
	// notifyRequestPrefix opens every notify row's id.
	notifyRequestPrefix = "pn"
	// notifyRequestedByPrefix is a notify row's provenance, before the run's
	// id.
	notifyRequestedByPrefix = "pipelines:notify:"

	// notifyPollFirst is the wait before the stage first reads its rows; each
	// wait after is half as long again, up to notifyPollMax.
	notifyPollFirst = 2 * time.Second
	notifyPollMax   = 15 * time.Second

	// notifyListedFiles is how many of the run's files a message names
	// (component/pipelines' composers list five and count the rest), and so
	// how many names the Library is asked for.
	notifyListedFiles = 5

	// maxChannelRecipients is the most addresses an email channel delivers
	// to. A channel's row carries whatever list was written to it, so the
	// stage holds the list to this itself, at delivery.
	maxChannelRecipients = 20

	// channelSecretNamePattern is the rule a Discord channel's secret NAME is
	// held to before anything is resolved: the outbound worker's
	// (component/outbound.SecretNamePattern), which
	// stageOutboundRequestToSecret's own @pattern spells. The resolver
	// interpolates the name into its lookup, so a name outside it is never
	// resolved, and one the mutation would refuse would fail every delivery.
	channelSecretNamePattern = `^[A-Z][A-Z0-9_]{0,63}$`

	// discordWebhookPath is where on its host a Discord webhook's URL lives.
	discordWebhookPath = "/api/webhooks/"

	// emailMedium is an email row's medium; a webhook's is secretTargetMedium.
	emailMedium = "email"
)

// A channel's kinds and lifecycle (dsl/pipelines' channel concept).
const (
	channelDiscord  = "discord"
	channelEmail    = "email"
	channelActive   = "active"
	channelArchived = "archived"
)

// An outbound row's delivery states (dsl/platform's outboundRequest.status).
const (
	outboundPending  = "pending"
	outboundSending  = "sending"
	outboundSent     = "sent"
	outboundRetrying = "retrying"
	outboundFailed   = "failed"
)

var channelSecretNameRe = regexp.MustCompile(channelSecretNamePattern)

// discordWebhookHosts are the hosts a Discord webhook's URL is served on.
var discordWebhookHosts = map[string]bool{
	"discord.com":        true,
	"discordapp.com":     true,
	"ptb.discord.com":    true,
	"canary.discord.com": true,
}

// ---------------------------------------------------------------------------
// The stage
// ---------------------------------------------------------------------------

// runNotify takes a notify step from pending to its receipt: the channel read
// and held to what a delivery needs, the message composed, one row staged per
// destination, and the rows read until they say how the delivery went. On a
// lost lease it writes nothing more; on a cancel it stages nothing, or says
// what it had already handed over.
func (dr *runDriver) runNotify(ctx context.Context, t *stepTrack) {
	step := t.step
	// The intent, before anything the step does.
	if !dr.stillHolds(ctx) {
		return
	}
	var journalErr error
	t.handle, journalErr = dr.work().Step(ctx, step.Key)
	if journalErr != nil {
		dr.journalUnavailable(step.Key, journalErr)
		return
	}
	running := t.snapshot()
	running.Status = StepRunning
	t.set(running)
	began := time.Now()
	finish := func(rec receipt) {
		rec.durationMs = max(time.Since(began).Milliseconds(), 1)
		dr.settle(ctx, t, rec)
	}
	fail := func(f *pipelines.Failure) { finish(failReceipt(f.Code, f.Message)) }

	target, failure := dr.targetOf(ctx, step)
	if failure != nil {
		fail(failure)
		return
	}
	requests, failure := dr.notifyRequests(ctx, step, target)
	if failure != nil {
		fail(failure)
		return
	}

	// Nothing leaves the cluster for a run that was asked to stop, or that
	// is no longer this replica's to drive: the cancel and the lease are
	// asked once more, the lease against its row, right before the first
	// row is staged.
	switch {
	case dr.lease.isLost():
		return
	case dr.lease.isCancelled():
		finish(cancelReceipt())
		return
	}
	if !dr.stillHolds(ctx) {
		return
	}
	mask := dr.maskFor(target)
	ids := make([]string, 0, len(requests))
	for _, req := range requests {
		// A cancel or a lost lease between two rows hands nothing more over;
		// the wait below, stopped at once, writes what is true of the rows
		// already handed over.
		if len(ids) > 0 && (dr.lease.isLost() || dr.lease.isCancelled()) {
			break
		}
		if err := dr.d.Store.StageNotification(ctx, req); err != nil {
			fail(stagingFailed(target.name, len(ids), len(requests), oneLine(mask(err.Error()))))
			return
		}
		ids = append(ids, req.RequestID)
	}
	dr.log.Info("pipelines: a notification was handed to the outbound worker",
		"step", step.Key, "channel", target.name, "kind", target.channel.Kind, "requests", ids)

	timeout := dr.deliveryTimeout(step)
	end, statuses, readErr := dr.notifyWait(ctx, ids, timeout).await(ctx)
	switch end {
	case deliveryStopped:
		// A cancel: the rows are the worker's now, and may still be
		// delivered. A lost lease: the replica taking over writes the
		// receipt, so this one writes nothing.
		if dr.lease.isCancelled() {
			finish(notifyCancelReceipt(target.name))
		}
	case deliverySent:
		finish(deliveredReceipt(target, ids, statuses))
	case deliveryFailed:
		fail(&pipelines.Failure{Code: pipelines.CodeNotifyFailed, Message: failedDelivery(target.name, statuses, mask)})
	default:
		fail(&pipelines.Failure{Code: pipelines.CodeNotifyUndelivered,
			Message: undeliveredNotice(target, timeout, statuses, readErr, mask)})
	}
}

// deliveryTimeout is how long the stage waits on its delivery: the step's own
// timeout, which is the default step timeout for every notify stage, or a
// test's (Deps.notifyTimeout).
func (dr *runDriver) deliveryTimeout(step pipelines.Step) time.Duration {
	if dr.d.notifyTimeout > 0 {
		return dr.d.notifyTimeout
	}
	if step.TimeoutSeconds > 0 {
		return time.Duration(step.TimeoutSeconds) * time.Second
	}
	return pipelines.DefaultStepTimeout
}

// notifyWait is the wait on the rows ids name, stopped by the drive's stop and
// fenced before each read by a fresh read of the run: a replica that lost the
// run stops reading at once rather than at its next heartbeat.
func (dr *runDriver) notifyWait(ctx context.Context, ids []string, timeout time.Duration) deliveryWait {
	pace := notifyPollAfter
	if every := dr.d.notifyPoll; every > 0 {
		pace = func(int) time.Duration { return every }
	}
	logged := false
	return deliveryWait{
		store: dr.d.Store, ids: ids, timeout: timeout, pace: pace,
		stop:  dr.lease.stop,
		holds: func() bool { return dr.stillHolds(ctx) },
		readFailed: func(err error) {
			if !logged {
				logged = true
				dr.log.Warn("pipelines: a notification's delivery state could not be read; the stage keeps reading it",
					"requests", ids, "error", dr.mask(err.Error()))
			}
		},
	}
}

// notifyPollAfter is the wait before read n (from 0) of a notification's rows:
// two seconds, then each wait half as long again, up to fifteen.
func notifyPollAfter(n int) time.Duration {
	d := notifyPollFirst
	for i := 0; i < n && d < notifyPollMax; i++ {
		d = d * 3 / 2
	}
	return min(d, notifyPollMax)
}

// newNotifyRequestID is a notify row's id: 128 bits from crypto/rand, which
// nobody can guess before the row exists.
func newNotifyRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return notifyRequestPrefix + hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// The channel
// ---------------------------------------------------------------------------

// notifyTarget is where a notification goes: the channel, by the name the
// stage gave it, and what its rows name -- a Discord webhook's secret, or an
// email channel's addresses.
type notifyTarget struct {
	channel   Channel
	name      string
	secret    string
	addresses []string
}

// targetOf reads the channel the step names, under the pipeline owner's
// borrowed authority and FRESH -- it may have been archived on another
// replica a moment ago -- and holds it to what a delivery needs: the owner's,
// active, accepting this pipeline, and able to deliver as it is set up. It
// answers the failure that fails the step when it is not.
func (dr *runDriver) targetOf(ctx context.Context, step pipelines.Step) (notifyTarget, *pipelines.Failure) {
	owner := dr.p.OwnerUserID
	name := strings.TrimSpace(step.Channel)
	ch, err := dr.d.Store.ChannelForOwnerByName(memql.ContextWithFreshRead(ctx), owner, name)
	if err != nil {
		return notifyTarget{}, notifyFailure(pipelines.CodeNotifyFailed,
			"Nothing was sent to %s: the channel could not be read (%s). Re-run to try again.", name, oneLine(dr.mask(err.Error())))
	}
	// The read is the owner's own; a channel it answers that is somebody
	// else's is not this pipeline's to deliver to, however it got there.
	if ch == nil || !sameID(ch.OwnerUserID, owner) {
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelMissing,
			"No channel named %s belongs to this pipeline's owner. Create it in Deployables > Settings > Channels, then re-run.", name)
	}
	switch {
	case ch.Status == channelArchived:
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelArchived,
			"Channel %s is archived, so it delivers nothing. Name an active channel in the notify stage, or create one in Deployables > Settings > Channels, then re-run.", name)
	case ch.Status != channelActive:
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s is neither active nor archived, so nothing was sent. Save it again in Deployables > Settings > Channels, then re-run.", name)
	case !slices.ContainsFunc(dr.p.ChannelIDs, func(id string) bool { return sameID(id, ch.ID) }):
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelNotAllowed,
			"Channel %s exists, but pipeline %s is not one it accepts. Allow the pipeline on the channel in Deployables > Settings > Channels, then re-run.", name, dr.p.Name)
	}
	switch ch.Kind {
	case channelDiscord:
		return dr.discordTarget(ctx, name, *ch)
	case channelEmail:
		return emailTarget(name, *ch)
	}
	return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
		"Channel %s is of a kind this cluster cannot deliver to, so nothing was sent. Save it again in Deployables > Settings > Channels as a Discord or an email channel, then re-run.", name)
}

// discordTarget holds a Discord channel's secret to the outbound worker's name
// rule and out of the platform's namespace BEFORE anything is resolved, then
// resolves it under the pipeline owner's borrowed actor -- once, only to check
// that the value is a Discord webhook's URL. The value is remembered for
// masking and dropped; no failure repeats it.
func (dr *runDriver) discordTarget(ctx context.Context, name string, ch Channel) (notifyTarget, *pipelines.Failure) {
	secret := strings.TrimSpace(ch.SecretRef)
	switch {
	case secret == "":
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s delivers to Discord and names no secret holding its webhook URL, so nothing was sent. Name the globalSecret holding the URL on the channel, then re-run.", name)
	case !channelSecretNameRe.MatchString(secret):
		// The name is not repeated: a string that breaks the rule may be a
		// value pasted where its name belongs -- the webhook's URL itself.
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s names a secret whose name does not match %s, so it was not resolved and nothing was sent. Name the globalSecret holding the webhook URL on the channel, then re-run.", name, channelSecretNamePattern)
	case strings.HasPrefix(secret, reservedSecretPrefix):
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s names %s, in the platform's own MEMQL_ namespace, which a channel never reads; it was not resolved and nothing was sent. Name the globalSecret holding the webhook URL on the channel, then re-run.", name, secret)
	}
	value := ""
	if dr.d.Secrets != nil {
		v, err := dr.d.Secrets(auth.ContextWithUserActor(ctx, dr.p.OwnerUserID), secret)
		if err == nil {
			value = v
		} else {
			// The resolver's error is not repeated: what it says about a
			// value it could not read is not this package's to pass on.
			dr.log.Info("pipelines: a channel's secret did not resolve", "channel", name, "secret", secret)
		}
	}
	if strings.TrimSpace(value) == "" {
		return notifyTarget{}, notifyFailure(pipelines.CodeSecretMissing,
			"No value is stored on this cluster for %s, which channel %s names. Store the channel's Discord webhook URL under that name, then re-run.", secret, name)
	}
	// A credential whatever it turns out to be: masked wherever the drive
	// writes from here on.
	dr.remember(value)
	if !isDiscordWebhookURL(value) {
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"The value stored for %s, which channel %s names, is not a Discord webhook URL, so nothing was sent. Store the channel's webhook URL under that name, then re-run.", secret, name)
	}
	return notifyTarget{channel: ch, name: name, secret: secret}, nil
}

// isDiscordWebhookURL reports whether value is a Discord webhook's URL:
// https, on one of Discord's hosts with no port and no user, at a path under
// /api/webhooks/ that names a webhook and climbs nowhere. Surrounding white
// space -- a stored value's trailing newline -- is not part of it, and any
// inside it is refused. The deployment's webhook allowlist still decides
// whether the worker delivers to it.
func isDiscordWebhookURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsFunc(value, isSpaceOrControl) {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || !discordWebhookHosts[strings.ToLower(u.Host)] {
		return false
	}
	rest, ok := strings.CutPrefix(u.Path, discordWebhookPath)
	if !ok || rest == "" {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// emailTarget is an email channel's addresses, each a bare mailbox, trimmed
// and listed once. An address that is not one -- a display name, a second
// address, a line break that would start a header -- fails the stage by its
// place in the list: the address itself is not repeated, because a check run
// can be public.
func emailTarget(name string, ch Channel) (notifyTarget, *pipelines.Failure) {
	var addresses []string
	seen := map[string]bool{}
	for i, raw := range ch.Recipients {
		address := strings.TrimSpace(raw)
		if address == "" {
			continue
		}
		if !bareMailbox(address) {
			return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
				"Recipient %d of channel %s is not a bare email address, so nothing was sent. Correct the channel's recipients in Deployables > Settings > Channels, then re-run.", i+1, name)
		}
		if key := strings.ToLower(address); !seen[key] {
			seen[key] = true
			addresses = append(addresses, address)
		}
	}
	switch {
	case len(addresses) == 0:
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s delivers by email and lists no recipients, so nothing was sent. Add its recipients in Deployables > Settings > Channels, then re-run.", name)
	case len(addresses) > maxChannelRecipients:
		return notifyTarget{}, notifyFailure(pipelines.CodeChannelInvalid,
			"Channel %s lists %d recipients, and a channel delivers to at most %d, so nothing was sent. Shorten its list in Deployables > Settings > Channels, then re-run.", name, len(addresses), maxChannelRecipients)
	}
	return notifyTarget{channel: ch, name: name, addresses: addresses}, nil
}

// bareMailbox reports whether s is one address and nothing else: it parses,
// and parsing gives back exactly s. That round trip is what refuses a display
// name, a comment (read as a name), angle brackets, a quoted local part (given
// back unquoted) and a second address; and the parser refuses a line break
// anywhere, so no address can carry a header.
func bareMailbox(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// maskFor masks a text the stage writes about its delivery: every value the
// drive resolved and, for an email channel, every address it lists -- a check
// run can be public, and a recipient's address is not the run's to publish.
func (dr *runDriver) maskFor(target notifyTarget) func(string) string {
	masks := append(dr.maskValues(), target.addresses...)
	return func(s string) string { return pipelines.MaskSecrets(s, masks) }
}

// ---------------------------------------------------------------------------
// The message
// ---------------------------------------------------------------------------

// notifyRequests composes the message and the rows that carry it: one webhook
// row naming a Discord channel's secret, or one email row per address. Each
// row has an id of its own, drawn here, so nothing staged can be a row that
// existed before this drive.
func (dr *runDriver) notifyRequests(ctx context.Context, step pipelines.Step, target notifyTarget) ([]NotificationRequest, *pipelines.Failure) {
	n, err := dr.notification(ctx, step)
	if err != nil {
		return nil, notifyFailure(pipelines.CodeNotifyFailed, "The notification workflow failed: %s", dr.mask(err.Error()))
	}
	request := func() (NotificationRequest, *pipelines.Failure) {
		id, err := newNotifyRequestID()
		if err != nil {
			return NotificationRequest{}, notifyFailure(pipelines.CodeNotifyFailed,
				"Nothing was sent to %s: no request id could be drawn (%s).", target.name, oneLine(err.Error()))
		}
		return NotificationRequest{RequestID: id, DedupeKey: id, RequestedBy: notifyRequestedByPrefix + dr.facts.runID}, nil
	}
	composeFailed := func(err error) *pipelines.Failure {
		return notifyFailure(pipelines.CodeNotifyFailed,
			"Nothing was sent to %s: the message could not be composed (%s).", target.name, oneLine(dr.mask(err.Error())))
	}

	if target.secret != "" {
		body, err := pipelinenotify.DiscordMessage(n)
		if err != nil {
			return nil, composeFailed(err)
		}
		req, failure := request()
		if failure != nil {
			return nil, failure
		}
		// The body is NOT masked again: its parts were (notification), and a
		// mask over the encoded JSON could cut its structure -- a secret
		// whose value is "name" would turn every message into a 400.
		req.Medium, req.TargetSecret, req.Body = secretTargetMedium, target.secret, string(body)
		return []NotificationRequest{req}, nil
	}
	subject, body, err := pipelinenotify.EmailMessage(n)
	if err != nil {
		return nil, composeFailed(err)
	}
	requests := make([]NotificationRequest, 0, len(target.addresses))
	for _, address := range target.addresses {
		req, failure := request()
		if failure != nil {
			return nil, failure
		}
		req.Medium, req.Target, req.Subject, req.Body = emailMedium, address, subject, body
		requests = append(requests, req)
	}
	return requests, nil
}

// notification is what the message says of the run so far.
//
// The outcome is FAILED when a step of an earlier stage failed, was refused,
// or was cancelled by its runner (the run itself was not, or the stage would
// not be running): the first such step, in the order declared, is the one it
// names. Otherwise it is RECOVERED when the run before this one did not pass,
// and PASSED. Stages counts the earlier stages that passed, in this attempt or
// in an earlier one a failed-only re-run carried them from; a stage skipped
// whole for any other reason -- nothing it covers changed -- did not pass.
//
// EVERY TEXT IN IT IS MASKED with every value the drive resolved, here, before
// a composer sees it: the composers escape what they are given, and a value
// escaped is no longer the string a mask looks for.
func (dr *runDriver) notification(ctx context.Context, step pipelines.Step) (pipelinenotify.Notification, error) {
	f := dr.facts
	masks := dr.maskValues()
	mask := func(s string) string { return pipelines.MaskSecrets(s, masks) }
	origin := strings.TrimSpace(dr.d.OSOrigin())
	now := dr.d.now()
	n := pipelinenotify.Notification{
		Pipeline: mask(dr.p.Name), Event: f.event, Version: mask(f.version), SHA: f.sha,
		Branch: mask(f.branch), PullRequest: f.pullRequest, Title: mask(f.title),
		OSOrigin: origin, At: now,
	}
	if !f.started.IsZero() {
		n.DurationMs = max(now.Sub(f.started).Milliseconds(), 0)
	}
	if origin != "" {
		n.RunPageURL = pipelines.RunPageURL(origin, f.runID)
	}
	for _, link := range step.Links {
		n.Links = append(n.Links, pipelines.Link{Label: mask(link.Label), URL: mask(link.URL)})
	}
	var files []string
	for _, st := range dr.stagesBefore(step.Stage) {
		states := make([]StepState, 0, len(st.tracks))
		for _, t := range st.tracks {
			s := t.snapshot()
			states = append(states, s)
			files = append(files, s.ArtifactFileIDs...)
			if n.Outcome != "" {
				continue
			}
			switch s.Status {
			case StepFailed, StepRefused, StepCancelled:
				name := s.Name
				if name == "" {
					name = s.Key
				}
				n.Outcome = pipelinenotify.NotifyFailed
				n.FailedStep, n.FailedCode, n.FailedMessage = mask(s.Stage+"/"+name), mask(s.Code), mask(s.Message)
			}
		}
		if passedOrCarried(states) {
			n.Stages++
		}
	}
	n.Artifacts = dr.notifyFiles(ctx, files, origin, mask)
	value, err := workflowhost.Run(ctx, "pipelineNotificationOutcome", map[string]any{"failed": n.Outcome == pipelinenotify.NotifyFailed, "previousFailed": dr.previousRunFailed(ctx)}, workflowhost.Options{})
	if err != nil {
		return n, err
	}
	outcome, _ := value.(string)
	n.Outcome = pipelinenotify.NotifyOutcome(outcome)
	if !n.Outcome.Valid() {
		return n, fmt.Errorf("notification workflow returned invalid outcome %q", outcome)
	}
	return n, nil
}

// passedOrCarried reports a stage that passed: one of its steps passed in this
// attempt and none did worse, or every step was skipped and one of them was
// carried as passed in an earlier attempt (pipeline_passed_earlier).
func passedOrCarried(states []StepState) bool {
	switch stageStatus(states) {
	case StagePassed:
		return true
	case StageSkipped:
		return slices.ContainsFunc(states, func(s StepState) bool { return s.Code == pipelines.CodePassedEarlier })
	}
	return false
}

// stagesBefore is the planned stages before the one named stage.
func (dr *runDriver) stagesBefore(stage string) []stageTracks {
	for i, st := range dr.stages {
		if st.name == stage {
			return dr.stages[:i]
		}
	}
	return dr.stages
}

// notifyFiles are the run's files as a message lists them, each once: named
// by the Library -- which is asked only for the names a message shows -- or,
// unnamed, by their place among the run's files, and each opening in the
// Library when the cluster has an OS domain. The names are cosmetic: a read
// that fails numbers every file rather than failing the stage.
func (dr *runDriver) notifyFiles(ctx context.Context, ids []string, origin string, mask func(string) string) []pipelines.Link {
	seen := map[string]bool{}
	var files []string
	for _, id := range ids {
		if bare := bareID(id); bare != "" && !seen[bare] {
			seen[bare] = true
			files = append(files, strings.TrimSpace(id))
		}
	}
	if len(files) == 0 {
		return nil
	}
	names, err := dr.d.Store.LibraryFileNames(ctx, dr.p.OwnerUserID, files[:min(len(files), notifyListedFiles)])
	if err != nil {
		dr.log.Warn("pipelines: the run's file names could not be read; the notification numbers its files", "error", dr.mask(err.Error()))
		names = nil
	}
	links := make([]pipelines.Link, 0, len(files))
	for i, id := range files {
		label := strings.TrimSpace(names[id])
		if label == "" {
			label = fmt.Sprintf("artifact %d", i+1)
		}
		link := pipelines.Link{Label: mask(label)}
		if origin != "" {
			link.URL = strings.TrimRight(origin, "/") + "/?libraryFile=" + url.QueryEscape(bareID(id))
		}
		links = append(links, link)
	}
	return links
}

// previousRunFailed reports whether the newest completed run of this pipeline
// and event, queued before this one, ended any way but success: what makes a
// pass RECOVERED. Read fresh: the run before may have concluded on another
// replica a moment ago. A history that cannot be read is no evidence of a
// failure, and the message says passed, which is true.
func (dr *runDriver) previousRunFailed(ctx context.Context) bool {
	f := dr.facts
	runs, err := dr.d.Store.PreviousRuns(memql.ContextWithFreshRead(ctx), f.pipelineID, f.event)
	if err != nil {
		dr.log.Warn("pipelines: the notification could not read the pipeline's previous runs, so it says passed rather than whether it recovered",
			"error", dr.mask(err.Error()))
		return false
	}
	var newest *Run
	for i := range runs {
		r := &runs[i]
		if sameID(r.ID, f.runID) || r.Status != StatusCompleted || !r.QueuedAt.Before(f.queuedAt) {
			continue
		}
		if newest == nil || r.QueuedAt.After(newest.QueuedAt) {
			newest = r
		}
	}
	return newest != nil && newest.Conclusion != ConclusionSuccess
}

// ---------------------------------------------------------------------------
// Waiting on the rows
// ---------------------------------------------------------------------------

// deliveryEnd is how waiting on a delivery's rows ended.
type deliveryEnd int

const (
	// deliveryWaiting: no verdict yet -- a read's answer, never an ending.
	deliveryWaiting deliveryEnd = iota
	// deliverySent: every row reads sent.
	deliverySent
	// deliveryFailed: a row reads failed.
	deliveryFailed
	// deliveryTimedOut: the time ran out first.
	deliveryTimedOut
	// deliveryStopped: the waiter was stopped -- its stop closed, or its fence
	// failed.
	deliveryStopped
)

// deliveryWait waits on outbound rows by reading them, and by nothing else:
// the worker delivering them may be on any replica, and the rows are where it
// says how a delivery went. Everything it waits by is a field -- the rows, the
// pace, the stop and the fence -- and nothing ties it to the notify stage's
// drive.
type deliveryWait struct {
	store Store
	ids   []string
	// timeout is how long to wait in all.
	timeout time.Duration
	// pace is the wait before read n (from 0).
	pace func(n int) time.Duration
	// stop ends the wait when it closes; nil never does.
	stop <-chan struct{}
	// holds is asked before each read, and false ends the wait; nil always
	// holds.
	holds func() bool
	// readFailed hears each read that failed; nil hears nothing.
	readFailed func(error)
}

// await reads the rows at the pace until every one is sent, one has failed,
// the time runs out or the wait is stopped, and answers how it ended with the
// rows as last read (nil when no read answered) and the error of the latest
// read, when it failed. The last read is made at the deadline itself, so a
// row sent in the last interval is not reported missing.
func (w deliveryWait) await(ctx context.Context) (deliveryEnd, []OutboundStatus, error) {
	deadline := time.Now().Add(w.timeout)
	var (
		last    []OutboundStatus
		lastErr error
	)
	for n := 0; ; n++ {
		if !w.sleep(ctx, min(w.pace(n), max(time.Until(deadline), 0))) || (w.holds != nil && !w.holds()) {
			return deliveryStopped, last, lastErr
		}
		statuses, err := w.store.OutboundStatuses(ctx, w.ids)
		if err != nil {
			lastErr = err
			if w.readFailed != nil {
				w.readFailed(err)
			}
		} else {
			last, lastErr = statuses, nil
			if end := deliveryOf(w.ids, statuses); end != deliveryWaiting {
				return end, statuses, nil
			}
		}
		if !time.Now().Before(deadline) {
			return deliveryTimedOut, last, lastErr
		}
	}
}

// sleep waits d, and answers false when the wait was stopped first.
func (w deliveryWait) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-w.stop:
		return false
	case <-ctx.Done():
		return false
	}
}

// deliveryOf is how one read of the rows stands: sent when EVERY row asked
// for reads sent, failed when one reads failed, and waiting otherwise. A read
// that is not of these rows -- an entry missing, or under another id --
// decides nothing: a row this wait did not ask for is no delivery of its own.
func deliveryOf(ids []string, statuses []OutboundStatus) deliveryEnd {
	if len(ids) == 0 || len(statuses) != len(ids) {
		return deliveryWaiting
	}
	sent := 0
	for i, s := range statuses {
		if !sameID(s.ID, ids[i]) {
			return deliveryWaiting
		}
		switch s.Status {
		case outboundFailed:
			return deliveryFailed
		case outboundSent:
			sent++
		}
	}
	if sent == len(ids) {
		return deliverySent
	}
	return deliveryWaiting
}

// ---------------------------------------------------------------------------
// Receipts
// ---------------------------------------------------------------------------

// deliveredReceipt is a delivery every row confirmed: where it went, the rows
// that carried it, and when the last of them was accepted.
func deliveredReceipt(target notifyTarget, ids []string, statuses []OutboundStatus) receipt {
	var sentAt time.Time
	for _, s := range statuses {
		if s.SentAt.After(sentAt) {
			sentAt = s.SentAt
		}
	}
	result := map[string]any{"channel": target.name, "kind": target.channel.Kind, "requestIds": slices.Clone(ids)}
	if !sentAt.IsZero() {
		result["sentAt"] = sentAt.UTC().Format(time.RFC3339)
	}
	how := "Discord"
	if target.secret == "" {
		how = "email, " + countOf(len(ids), "recipient", "recipients")
	}
	return receipt{status: WorkStepDone, report: StepSucceeded, message: "Delivered to " + target.name + " (" + how + ").", result: result}
}

// notifyCancelReceipt is a cancel that came while the stage waited: the run's
// cancel receipt, saying what is true of a notification already handed over.
func notifyCancelReceipt(name string) receipt {
	why := "The run was cancelled while this step waited on its delivery to " + name +
		". The notification had already been handed to the outbound worker, so it may still arrive."
	return receipt{status: WorkStepCancelled, report: StepCancelled, message: why, result: map[string]any{"reason": why}}
}

// stagingFailed is a stage that could not hand every row over. A staging that
// answered an error may still have written its row, so the one it failed on
// MAY NOT have been handed over -- never "nothing was sent" -- and what was
// handed over before it is the worker's, and may still arrive.
func stagingFailed(name string, handed, total int, why string) *pipelines.Failure {
	if handed == 0 {
		return notifyFailure(pipelines.CodeNotifyFailed,
			"The notification to %s may not have been handed to the outbound worker: handing it over failed (%s). Re-run to send it.", name, why)
	}
	return notifyFailure(pipelines.CodeNotifyFailed,
		"Delivery to %s was cut short: handing delivery %d of %d to the outbound worker failed (%s), so it may not have been handed over; the %d handed over before it may still arrive.",
		name, handed+1, total, why, handed)
}

// failedDelivery names the delivery the outbound worker gave up on, after how
// many attempts, with the worker's own error, masked: a transport's error can
// carry what it failed to reach.
func failedDelivery(name string, statuses []OutboundStatus, mask func(string) string) string {
	i := max(slices.IndexFunc(statuses, func(s OutboundStatus) bool { return s.Status == outboundFailed }), 0)
	s := statuses[i]
	why := strings.TrimRight(oneLine(mask(s.LastError)), ".")
	if why == "" {
		why = "the outbound worker recorded no error"
	}
	var b strings.Builder
	if s.Attempts > 0 {
		fmt.Fprintf(&b, "Delivery to %s failed after %s: %s", name, countOf(s.Attempts, "attempt", "attempts"), why)
	} else {
		fmt.Fprintf(&b, "Delivery to %s was refused by the outbound worker before any attempt: %s", name, why)
	}
	if n := len(statuses); n > 1 {
		fmt.Fprintf(&b, " (recipient %d of %d; %d of %d delivered so far)", i+1, n, sentCount(statuses), n)
	}
	b.WriteString(".")
	return b.String()
}

// undeliveredNotice says where a delivery stood when the stage stopped waiting,
// as its row says it -- and never that the worker keeps trying when the row
// says otherwise: a row left `sending` by a replica that died mid-send is not
// taken up again.
func undeliveredNotice(target notifyTarget, timeout time.Duration, statuses []OutboundStatus, readErr error, mask func(string) string) string {
	lead := fmt.Sprintf("Not delivered to %s within %s", target.name, pipelines.FormatDuration(timeout))
	if statuses == nil {
		why := "no read answered"
		if readErr != nil {
			why = oneLine(mask(readErr.Error()))
		}
		return lead + "; the state of its delivery could not be read (" + why +
			"), though it was handed to the outbound worker, which may still deliver it."
	}
	n := len(statuses)
	i := max(slices.IndexFunc(statuses, func(s OutboundStatus) bool { return s.Status != outboundSent }), 0)
	s := statuses[i]
	which, delivery, retrying := "it", "the delivery", "retrying"
	if n > 1 {
		lead += fmt.Sprintf(" (%d of %d recipients so far)", sentCount(statuses), n)
		which = fmt.Sprintf("recipient %d of %d", i+1, n)
		delivery = "the delivery to " + which
		retrying += " " + which
	}
	var clause string
	switch s.Status {
	case outboundRetrying:
		clause = fmt.Sprintf("the outbound worker is still %s (%s) and keeps trying after this step ends", retrying, attemptNote(s, mask))
	case outboundPending:
		yet := "yet"
		if s.Attempts > 0 {
			yet = "again yet (" + attemptNote(s, mask) + ")"
		}
		medium := "webhooks"
		if target.secret == "" {
			medium = "email"
		}
		clause = fmt.Sprintf("the outbound worker has not attempted %s %s, and it stays queued after this step ends until a node configured to send %s takes it",
			which, yet, medium)
	case outboundSending:
		clause = delivery + " is still marked as being sent, and the worker that claimed it has not concluded"
	case "":
		clause = "the outbound row this step staged"
		if n > 1 {
			clause += " for " + which
		}
		clause += " could not be found"
	default:
		clause = fmt.Sprintf("%s reads %q, a state this step does not know", delivery, s.Status)
	}
	msg := lead + "; " + clause
	if readErr != nil {
		msg += " (as last read; reading it again failed: " + oneLine(mask(readErr.Error())) + ")"
	}
	return msg + "."
}

// attemptNote is a row's attempts and its last error, masked: "attempt 2:
// webhook: status 502".
func attemptNote(s OutboundStatus, mask func(string) string) string {
	note := fmt.Sprintf("attempt %d", s.Attempts)
	if why := strings.TrimRight(oneLine(mask(s.LastError)), "."); why != "" {
		note += ": " + why
	}
	return note
}

func sentCount(statuses []OutboundStatus) int {
	n := 0
	for _, s := range statuses {
		if s.Status == outboundSent {
			n++
		}
	}
	return n
}

func notifyFailure(code, format string, args ...any) *pipelines.Failure {
	return &pipelines.Failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// oneLine is s with every run of white space or control characters made one
// space: what a worker or an engine said, as one line of a sentence.
func oneLine(s string) string { return strings.Join(strings.FieldsFunc(s, isSpaceOrControl), " ") }

// countOf is n and the noun that agrees with it.
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
