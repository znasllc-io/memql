package campaigns

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// store.go -- the engine seam.
//
// Everything the sending engine reads or writes goes through a NAMED DSL
// construct, never a hand-built query string. That is not stylistic: a
// named construct carries its `filter` (including the caller conjunct)
// and its declared row-authz binding, whereas a raw string is enforced
// only by the per-row admission gate and has no binding to inject a tier
// from. Reaching for a raw string here would quietly opt the engine's own
// reads out of the narrower of the two enforcement mechanisms.
//
// # The actor is the caller's business, not the store's
//
// Not one method here builds an actor context. Each takes the ctx it is
// given and issues the call under it, and the worker is explicit at every
// call site about WHICH identity is in scope:
//
//   - the engine's own operator identity, for the two clusterOwner-tier
//     engine concepts (send jobs, suppression);
//   - the CAMPAIGN OWNER'S identity, for everything owned (the campaign,
//     its template, its audience's recipients, the delivery ledger).
//
// Keeping that out of the store means a reader can see, at the call site,
// whose authority a given read runs under -- which is the one question
// this design turns on. A store that picked the actor itself would make
// that invisible exactly where it matters most.

// Engine is the narrow engine surface the sending engine needs. Kept to
// one method so tests fake it with flat row envelopes (the outbound
// worker's precedent).
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Store issues the domain's named constructs.
type Store struct{ engine Engine }

// NewStore wraps an engine.
func NewStore(engine Engine) *Store { return &Store{engine: engine} }

// SendJob is one row of v1:campaigns:sendJob.
type SendJob struct {
	ID                  string
	CampaignID          string
	CampaignOwnerUserID string
	AudienceID          string
	TemplateID          string
	Status              string
	SentCount           int
	SkippedCount        int
	FailedCount         int
	RecipientCount      int
	ThrottledUntil      time.Time
	StartedAt           time.Time

	// ScheduledAt is the time this job was enqueued for, copied off the
	// campaign at schedule time (memql#3459). A HINT, never the authority:
	// the worker re-reads the campaign's own scheduledAt at fire time, so an
	// operator who moves the date is obeyed rather than raced by this copy.
	ScheduledAt time.Time

	// RosterCursor is where the roster walk resumes (memql#3460) -- an
	// opaque engine keyset cursor. An OPTIMIZATION only: losing it costs one
	// re-scan, because the ledger is what decides who gets mailed.
	RosterCursor string

	// RosterOutstanding records whether the pass in progress has seen
	// anything still to deal with. It is what makes completion decidable
	// without holding the whole roster in memory.
	RosterOutstanding bool
}

// Progress is the count of terminal outcomes so far. It doubles as the
// cross-replica claim key's discriminator: a batch that made progress
// changes it, so the next batch claims fresh, while a batch that made
// none re-claims the same key and is correctly blocked until the lease
// expires.
func (j SendJob) Progress() int { return j.SentCount + j.SkippedCount + j.FailedCount }

// Campaign is the subset of v1:campaigns:campaign the sender reads.
type Campaign struct {
	ID          string
	OwnerUserID string
	Name        string
	AudienceID  string
	TemplateID  string
	FromName    string
	ReplyTo     string
	Status      string

	// ScheduledAt is THE authority on when a scheduled send fires
	// (memql#3459). The job row carries a copy for the operator to look at;
	// this is the one the worker compares against the clock.
	ScheduledAt time.Time
}

// Template is the authored content.
type Template struct {
	ID       string
	Subject  string
	TextBody string
	HTMLBody string
	Status   string
}

// Recipient is one address in the audience roster.
type Recipient struct {
	ID string
	// CanonicalID is the stored `v1:campaigns:recipient:<short>` form. Kept
	// alongside the bare id because the ledger page read compares a LIST of
	// recipient ids, and the engine's relationship-canonicalizing pre-walk
	// rewrites a bare id only on a scalar comparison -- so a bare id in that
	// list would match nothing, which reads as "nobody has been mailed"
	// (memql#3460).
	CanonicalID        string
	Email              string
	DisplayName        string
	SubscriptionStatus string
}

// LedgerEntry is the slim delivery projection the resume diff runs over.
type LedgerEntry struct {
	RecipientID   string
	Status        string
	Attempts      int
	NextAttemptAt time.Time
}

// Suppression is a row of the cluster-wide do-not-mail list.
type Suppression struct {
	Digest       string
	Reason       string
	SuppressedAt time.Time
}

// --- reads --------------------------------------------------------------

// DrainableJobs returns queued + running send jobs, oldest first.
// clusterOwner-tier: issue under the engine's operator identity.
func (s *Store) DrainableJobs(ctx context.Context) ([]SendJob, error) {
	rows, err := s.rows(ctx, "query drainableSendJobs()")
	if err != nil {
		return nil, err
	}
	jobs := make([]SendJob, 0, len(rows))
	for _, r := range rows {
		jobs = append(jobs, sendJobFromRow(r))
	}
	return jobs, nil
}

// AnyCampaignEverSent reports whether this cluster has ever put a
// campaign message in front of a recipient -- which is the same question
// as "does an unsubscribe link signed by the current secret exist in
// somebody's mailbox" (memql#3458). clusterOwner-tier: it deliberately
// spans owners, so it is issued under the engine's operator identity.
//
// The signal is sentCount, not the presence of a job: a queued job that
// never drained mailed nobody, and warning about it would train an
// operator to ignore the warning.
//
// It reads the newest recentSendJobs page rather than counting the whole
// history, and that bound is honest about what this is: a boot-time
// nudge, not an audit. A cluster whose newest 200 send jobs all delivered
// zero messages while an older one delivered some is a cluster the
// warning misses -- and the cost of that miss is one missing log line,
// not a wrong action.
func (s *Store) AnyCampaignEverSent(ctx context.Context) (bool, error) {
	rows, err := s.rows(ctx, "query recentSendJobs()")
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if integer(r, "sentCount") > 0 {
			return true, nil
		}
	}
	return false, nil
}

// JobByID re-reads one job after the claim, so the worker acts on the row
// as it stands now rather than as the scan saw it -- an operator may have
// paused the campaign in between.
func (s *Store) JobByID(ctx context.Context, sendJobID string) (SendJob, bool, error) {
	rows, err := s.rows(ctx, call("query", "sendJobById", arg{"sendJobId", sendJobID}))
	if err != nil || len(rows) == 0 {
		return SendJob{}, false, err
	}
	return sendJobFromRow(rows[0]), true, nil
}

// CampaignByID reads a campaign. OWNED tier: issue under the campaign
// owner's identity. A `false` here is not only "no such row" -- it is
// also what a wrong actor looks like, which is the correct answer for
// both.
func (s *Store) CampaignByID(ctx context.Context, campaignID string) (Campaign, bool, error) {
	rows, err := s.rows(ctx, call("query", "campaignById", arg{"campaignId", campaignID}))
	if err != nil || len(rows) == 0 {
		return Campaign{}, false, err
	}
	r := rows[0]
	return Campaign{
		ID:          bare(str(r, "id")),
		OwnerUserID: bare(str(r, "ownerUserId")),
		Name:        str(r, "name"),
		AudienceID:  bare(str(r, "audienceId")),
		TemplateID:  bare(str(r, "templateId")),
		FromName:    str(r, "fromName"),
		ReplyTo:     str(r, "replyTo"),
		Status:      str(r, "status"),
		ScheduledAt: parseTime(str(r, "scheduledAt")),
	}, true, nil
}

// ScheduledJobs returns every send job still waiting for its time, oldest
// first. clusterOwner-tier: issue under the engine's operator identity.
//
// Unfiltered by due-ness on purpose (memql#3459). The row carries a COPY of
// the campaign's scheduledAt, and an operator moving the date with
// updateCampaign never touches it -- so filtering here would either fire a
// send the operator had postponed or sit on one they had brought forward.
// The worker reads them all and asks the campaign, which is affordable
// because the set is one row per pending scheduled campaign.
func (s *Store) ScheduledJobs(ctx context.Context) ([]SendJob, error) {
	rows, err := s.rows(ctx, "query scheduledSendJobs()")
	if err != nil {
		return nil, err
	}
	jobs := make([]SendJob, 0, len(rows))
	for _, r := range rows {
		jobs = append(jobs, sendJobFromRow(r))
	}
	return jobs, nil
}

// TemplateByID reads a template. OWNED tier.
func (s *Store) TemplateByID(ctx context.Context, templateID string) (Template, bool, error) {
	rows, err := s.rows(ctx, call("query", "templateById", arg{"templateId", templateID}))
	if err != nil || len(rows) == 0 {
		return Template{}, false, err
	}
	r := rows[0]
	return Template{
		ID:       bare(str(r, "id")),
		Subject:  str(r, "subject"),
		TextBody: str(r, "textBody"),
		HTMLBody: str(r, "htmlBody"),
		Status:   str(r, "status"),
	}, true, nil
}

// RosterPage reads ONE PAGE of the audience, suppressed members included --
// a `skipped` delivery row is an outcome the operator is owed rather than a
// silence. OWNED tier.
//
// cursor is the engine's opaque keyset token from the previous page, or ""
// to start. The returned cursor is empty when the audience is exhausted,
// which is the signal the walk terminates on (memql#3460).
func (s *Store) RosterPage(ctx context.Context, audienceID, cursor string) ([]Recipient, string, error) {
	rows, next, err := s.rowsPage(ctx, cursor, call("query", "audienceRosterForSend", arg{"audienceId", audienceID}))
	if err != nil {
		return nil, "", err
	}
	out := make([]Recipient, 0, len(rows))
	for _, r := range rows {
		canonical := str(r, "id")
		out = append(out, Recipient{
			ID:                 bare(canonical),
			CanonicalID:        canonical,
			Email:              str(r, "email"),
			DisplayName:        str(r, "displayName"),
			SubscriptionStatus: str(r, "subscriptionStatus"),
		})
	}
	return out, next, nil
}

// Roster reads the whole audience by walking every page. OWNED tier.
//
// Retained for the callers that genuinely want the set rather than a batch:
// the unsubscribe endpoint resolving one recipient, and tests. THE SEND PATH
// DOES NOT USE IT -- it walks RosterPage, because materializing a
// hundred-thousand-address audience once per batch is the shape memql#3460
// removed. maxRows bounds it so "the whole audience" cannot become an
// unbounded allocation in a caller that only needed one row.
func (s *Store) Roster(ctx context.Context, audienceID string) ([]Recipient, error) {
	var out []Recipient
	cursor := ""
	for {
		page, next, err := s.RosterPage(ctx, audienceID, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" || len(out) >= rosterWalkCap {
			return out, nil
		}
		cursor = next
	}
}

// rosterWalkCap bounds the whole-audience read above. Deliberately far
// larger than the old 5000 ceiling and deliberately not a product limit:
// it exists so a bug in the cursor loop is a bounded allocation rather
// than an OOM.
const rosterWalkCap = 100000

// RosterSize returns how many recipients an audience holds, as a
// server-side count. OWNED tier.
//
// A count query rather than the length of a read, and that distinction is
// the memql#3460 lesson in one line: measuring a bounded page and calling it
// a total is how the 5000 ceiling came to be load-bearing.
func (s *Store) RosterSize(ctx context.Context, audienceID string) (int, error) {
	res, err := s.engine.Execute(ctx, call("query", "audienceRosterSize", arg{"audienceId", audienceID}))
	if err != nil {
		return 0, fmt.Errorf("campaigns: query audienceRosterSize: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if _, ok := row["count"]; ok {
			return integer(row, "count"), nil
		}
	}
	return 0, nil
}

// LedgerFor reads the delivery ledger for exactly the recipients named,
// keyed by bare recipient id. THE read that makes a resumed send safe, and
// the one that keeps a batch's cost proportional to the batch rather than to
// the campaign (memql#3460). OWNED tier.
//
// canonicalIDs must be the stored canonical form -- see Recipient.CanonicalID
// for why a bare id here would silently match nothing.
func (s *Store) LedgerFor(ctx context.Context, campaignID string, canonicalIDs []string) (map[string]LedgerEntry, error) {
	if len(canonicalIDs) == 0 {
		return map[string]LedgerEntry{}, nil
	}
	rows, err := s.rows(ctx, call("query", "deliveriesForRecipients",
		arg{"campaignId", campaignID},
		arg{"recipientIds", canonicalIDs},
	))
	if err != nil {
		return nil, err
	}
	// The query's own paginate is the only thing that could truncate this
	// read, and a truncated ledger reads as "these recipients have never been
	// mailed" -- which would re-mail them. Reads collapse to one row per id,
	// so a result at or over the bound means the page size and the bound have
	// drifted apart, and refusing is the only safe answer.
	if len(rows) >= ledgerPageBound {
		return nil, fmt.Errorf(
			"campaigns: the delivery-ledger page returned %d rows, at the query's %d bound; refusing to diff a possibly-truncated ledger",
			len(rows), ledgerPageBound)
	}
	return ledgerFromRows(rows), nil
}

// ledgerPageBound mirrors the `paginate` on deliveriesForRecipients. It is
// headroom over the roster page size, not a page in its own right: the read
// returns at most one row per named recipient.
const ledgerPageBound = 1000

// Ledger reads the delivery ledger for one campaign, keyed by bare
// recipient id. THE read that makes a resumed send safe. OWNED tier.
func (s *Store) Ledger(ctx context.Context, campaignID string) (map[string]LedgerEntry, error) {
	rows, err := s.rows(ctx, call("query", "deliveryLedgerForCampaign", arg{"campaignId", campaignID}))
	if err != nil {
		return nil, err
	}
	return ledgerFromRows(rows), nil
}

// ledgerFromRows keys a ledger projection by bare recipient id.
//
// LAST WRITE WINS on a duplicate. The query sorts oldest-first and a
// re-attempt appends a new version under the SAME row id, so the later row is
// the current state. Reading the first would resume against a stale outcome
// and re-mail somebody.
func ledgerFromRows(rows []map[string]any) map[string]LedgerEntry {
	out := make(map[string]LedgerEntry, len(rows))
	for _, r := range rows {
		recipientID := bare(str(r, "recipientId"))
		if recipientID == "" {
			continue
		}
		out[recipientID] = LedgerEntry{
			RecipientID:   recipientID,
			Status:        str(r, "status"),
			Attempts:      integer(r, "attempts"),
			NextAttemptAt: parseTime(str(r, "nextAttemptAt")),
		}
	}
	return out
}

// SuppressionByDigest asks the cluster-wide list about one address.
// clusterOwner-tier: issue under the engine's operator identity, NEVER
// the campaign owner's -- no operator's authority may decide whether the
// suppression list applies to them.
func (s *Store) SuppressionByDigest(ctx context.Context, digest string) (Suppression, bool, error) {
	if digest == "" {
		return Suppression{}, false, nil
	}
	rows, err := s.rows(ctx, call("query", "suppressionByDigest", arg{"emailDigest", digest}))
	if err != nil || len(rows) == 0 {
		return Suppression{}, false, err
	}
	r := rows[len(rows)-1]
	return Suppression{
		Digest:       str(r, "id"),
		Reason:       str(r, "reason"),
		SuppressedAt: parseTime(str(r, "suppressedAt")),
	}, true, nil
}

// --- writes -------------------------------------------------------------

// EnqueueSend creates (or re-creates) the send job for a campaign.
// clusterOwner-tier.
//
// job.Status selects between the two ways a job comes into existence
// (memql#3459): empty or "queued" for a send starting now, "scheduled" for
// one committed to a time. A scheduled job is inert -- isDrainableSendJob
// does not match it -- so writing one can never by itself mail anybody.
func (s *Store) EnqueueSend(ctx context.Context, job SendJob) error {
	args := []arg{
		{"campaignId", job.CampaignID},
		{"campaignOwnerUserId", job.CampaignOwnerUserID},
		{"audienceId", job.AudienceID},
		{"templateId", job.TemplateID},
	}
	if job.Status != "" {
		args = append(args, arg{"status", job.Status})
	}
	if !job.ScheduledAt.IsZero() {
		args = append(args, arg{"scheduledAt", job.ScheduledAt.UTC().Format(time.RFC3339)})
	}
	return s.exec(ctx, call("mutation", "enqueueCampaignSend", args...))
}

// SendJobPatch is the set of send-job fields a caller means to change.
// Pointer-valued so "leave it alone" and "set it to zero" are different
// requests -- `updateSendJob` accepts every field it is handed, so a
// zero-valued struct would blank the counters on every progress stamp.
type SendJobPatch struct {
	Status         *string
	RecipientCount *int
	SentCount      *int
	SkippedCount   *int
	FailedCount    *int
	LastError      *string
	StartedAt      *time.Time
	CompletedAt    *time.Time
	ThrottledUntil *time.Time

	// RosterCursor and RosterOutstanding carry the walk's position and
	// whether the current pass has seen outstanding work (memql#3460).
	RosterCursor      *string
	RosterOutstanding *bool
}

// UpdateJob stamps a patch onto a send job. clusterOwner-tier.
func (s *Store) UpdateJob(ctx context.Context, sendJobID string, patch SendJobPatch) error {
	args := []arg{{"sendJobId", sendJobID}}
	args = appendStr(args, "status", patch.Status)
	args = appendInt(args, "recipientCount", patch.RecipientCount)
	args = appendInt(args, "sentCount", patch.SentCount)
	args = appendInt(args, "skippedCount", patch.SkippedCount)
	args = appendInt(args, "failedCount", patch.FailedCount)
	args = appendStr(args, "lastError", patch.LastError)
	args = appendTime(args, "startedAt", patch.StartedAt)
	args = appendTime(args, "completedAt", patch.CompletedAt)
	args = appendTime(args, "throttledUntil", patch.ThrottledUntil)
	args = appendStr(args, "rosterCursor", patch.RosterCursor)
	args = appendBool(args, "rosterOutstanding", patch.RosterOutstanding)
	return s.exec(ctx, call("mutation", "updateSendJob", args...))
}

// Delivery is one per-recipient outcome to record.
type Delivery struct {
	CampaignID    string
	RecipientID   string
	Email         string
	Status        string
	SkipReason    string
	LastError     string
	SentAt        time.Time
	Attempts      int
	NextAttemptAt time.Time
}

// RecordDelivery writes the ledger row. OWNED tier: issue under the
// CAMPAIGN OWNER'S identity -- that is what stamps ownerUserId from the
// actor and makes v1:campaigns:delivery's declared owner tier true
// (memql#3348).
func (s *Store) RecordDelivery(ctx context.Context, d Delivery) error {
	args := []arg{
		{"campaignId", d.CampaignID},
		{"recipientId", d.RecipientID},
		{"email", d.Email},
		{"status", d.Status},
		{"attempts", d.Attempts},
	}
	if d.SkipReason != "" {
		args = append(args, arg{"skipReason", d.SkipReason})
	}
	if d.LastError != "" {
		args = append(args, arg{"lastError", truncateError(d.LastError)})
	}
	if !d.SentAt.IsZero() {
		args = append(args, arg{"sentAt", d.SentAt.UTC().Format(time.RFC3339)})
	}
	if !d.NextAttemptAt.IsZero() {
		args = append(args, arg{"nextAttemptAt", d.NextAttemptAt.UTC().Format(time.RFC3339)})
	}
	return s.exec(ctx, call("mutation", "recordCampaignDelivery", args...))
}

// CampaignProgress is the counter set stamped onto the campaign row.
type CampaignProgress struct {
	Status         *string
	RecipientCount *int
	SentCount      *int
	FailedCount    *int
	LastError      *string
	CompletedAt    *time.Time
}

// UpdateCampaignProgress stamps live counters onto the campaign. OWNED
// tier: campaign owner's identity.
func (s *Store) UpdateCampaignProgress(ctx context.Context, campaignID string, p CampaignProgress) error {
	args := []arg{{"campaignId", campaignID}}
	args = appendStr(args, "status", p.Status)
	args = appendInt(args, "recipientCount", p.RecipientCount)
	args = appendInt(args, "sentCount", p.SentCount)
	args = appendInt(args, "failedCount", p.FailedCount)
	args = appendStr(args, "lastError", p.LastError)
	args = appendTime(args, "completedAt", p.CompletedAt)
	return s.exec(ctx, call("mutation", "updateCampaignProgress", args...))
}

// ScheduleCampaign commits a campaign to a time. OWNED tier: the caller's
// own identity, because this is the operator's row.
//
// Separate from SetCampaignStatus because it carries a value rather than
// only a transition -- and the value it carries is the one the drain
// worker treats as authoritative when it decides whether a send is due.
func (s *Store) ScheduleCampaign(ctx context.Context, campaignID string, at time.Time) error {
	return s.exec(ctx, call("mutation", "scheduleCampaign",
		arg{"campaignId", campaignID},
		arg{"scheduledAt", at.UTC().Format(time.RFC3339)},
	))
}

// SetCampaignStatus drives the operator-visible lifecycle transitions.
// OWNED tier.
func (s *Store) SetCampaignStatus(ctx context.Context, mutationName, campaignID string) error {
	return s.exec(ctx, call("mutation", mutationName, arg{"campaignId", campaignID}))
}

// RecordSuppression adds an address's digest to the cluster-wide list.
// clusterOwner-tier. The plaintext address never reaches the engine.
func (s *Store) RecordSuppression(ctx context.Context, digest, reason, domain, sourceCampaignID, note string) error {
	args := []arg{
		{"emailDigest", digest},
		{"reason", reason},
	}
	if domain != "" {
		args = append(args, arg{"domain", domain})
	}
	if sourceCampaignID != "" {
		args = append(args, arg{"sourceCampaignId", sourceCampaignID})
	}
	if note != "" {
		args = append(args, arg{"note", note})
	}
	return s.exec(ctx, call("mutation", "recordSuppression", args...))
}

// SetRecipientSubscription converges one operator's recipient row onto
// the cluster list's verdict. OWNED tier: campaign owner's identity.
func (s *Store) SetRecipientSubscription(ctx context.Context, recipientID, status string, at time.Time) error {
	args := []arg{
		{"recipientId", recipientID},
		{"subscriptionStatus", status},
	}
	if !at.IsZero() {
		args = append(args, arg{"unsubscribedAt", at.UTC().Format(time.RFC3339)})
	}
	return s.exec(ctx, call("mutation", "setRecipientSubscription", args...))
}

// RecipientByID reads one recipient. OWNED tier -- the unsubscribe
// endpoint uses it to resolve the address the signed token names.
func (s *Store) RecipientByID(ctx context.Context, audienceID, recipientID string) (Recipient, bool, error) {
	roster, err := s.Roster(ctx, audienceID)
	if err != nil {
		return Recipient{}, false, err
	}
	for _, r := range roster {
		if r.ID == recipientID {
			return r, true, nil
		}
	}
	return Recipient{}, false, nil
}

// --- plumbing -----------------------------------------------------------

type arg struct {
	name  string
	value any
}

// call renders a named-construct invocation. Every string value goes
// through langparser.QuoteString rather than %q (memql#3035): these
// values include a provider's own error text, and %q emits escapes the
// MemQL lexer refuses outright -- which would make the statement
// unparseable and silently drop the write that was recording the failure.
func call(kind, name string, args ...arg) string {
	rendered := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.value.(type) {
		case string:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(v))
		case int:
			rendered = append(rendered, fmt.Sprintf("%s: %d", a.name, v))
		case bool:
			rendered = append(rendered, fmt.Sprintf("%s: %t", a.name, v))
		case []string:
			quoted := make([]string, 0, len(v))
			for _, item := range v {
				quoted = append(quoted, langparser.QuoteString(item))
			}
			rendered = append(rendered, a.name+": ["+strings.Join(quoted, ", ")+"]")
		default:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}
	return fmt.Sprintf("%s %s(%s)", kind, name, strings.Join(rendered, ", "))
}

func appendStr(args []arg, name string, v *string) []arg {
	if v == nil {
		return args
	}
	return append(args, arg{name, *v})
}

func appendBool(args []arg, name string, v *bool) []arg {
	if v == nil {
		return args
	}
	return append(args, arg{name, *v})
}

func appendInt(args []arg, name string, v *int) []arg {
	if v == nil {
		return args
	}
	return append(args, arg{name, *v})
}

func appendTime(args []arg, name string, v *time.Time) []arg {
	if v == nil {
		return args
	}
	if v.IsZero() {
		return append(args, arg{name, ""})
	}
	return append(args, arg{name, v.UTC().Format(time.RFC3339)})
}

func (s *Store) rows(ctx context.Context, q string) ([]map[string]any, error) {
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("campaigns: %s: %w", firstWords(q), err)
	}
	return memql.MaterializeRows(res), nil
}

// rowsPage issues a paginated read at the given continuation point and
// returns the rows plus the engine's next cursor (memql#3460).
//
// The cursor rides the CONTEXT rather than the query text, because that is
// the engine's own contract: `paginate` in the DSL declares a page size, and
// the continuation is a runtime value the executor lifts off the context and
// compiles into a `(createdAt, id)` keyset predicate. There is no grammar for
// it and there should not be -- a cursor spelled into a query string would be
// a caller-supplied scan position on an owned-tier read.
func (s *Store) rowsPage(ctx context.Context, cursor, q string) ([]map[string]any, string, error) {
	if cursor != "" {
		ctx = memql.ContextWithCursor(ctx, cursor)
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("campaigns: %s: %w", firstWords(q), err)
	}
	return memql.MaterializeRows(res), cursorFromResult(res), nil
}

// cursorFromResult lifts the continuation token off whatever the engine seam
// handed back. Two shapes, and both are real: the in-process engine returns
// an *ExecuteResult carrying ResultMeta, and the package's tests fake flat row
// envelopes (the seam is `Execute(ctx, string) (any, error)` precisely so they
// can). A result with neither is an exhausted page, which is the safe reading
// -- it ends the walk rather than continuing from a position nobody supplied.
func cursorFromResult(res any) string {
	type metaCarrier interface{ GetMeta() *memql.ResultMeta }
	if carrier, ok := res.(metaCarrier); ok {
		if meta := carrier.GetMeta(); meta != nil {
			return strings.TrimSpace(meta.Cursor)
		}
	}
	if envelope, ok := res.(map[string]any); ok {
		if raw, ok := envelope["cursor"].(string); ok {
			return strings.TrimSpace(raw)
		}
		if meta, ok := envelope["meta"].(map[string]any); ok {
			if raw, ok := meta["cursor"].(string); ok {
				return strings.TrimSpace(raw)
			}
		}
	}
	return ""
}

func (s *Store) exec(ctx context.Context, q string) error {
	if _, err := s.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("campaigns: %s: %w", firstWords(q), err)
	}
	return nil
}

// firstWords names the construct in an error without echoing its
// arguments, which can carry a recipient's address.
func firstWords(q string) string {
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	return q
}

func sendJobFromRow(r map[string]any) SendJob {
	return SendJob{
		ID:                  bare(str(r, "id")),
		CampaignID:          bare(str(r, "campaignId")),
		CampaignOwnerUserID: bare(str(r, "campaignOwnerUserId")),
		AudienceID:          bare(str(r, "audienceId")),
		TemplateID:          bare(str(r, "templateId")),
		Status:              str(r, "status"),
		SentCount:           integer(r, "sentCount"),
		SkippedCount:        integer(r, "skippedCount"),
		FailedCount:         integer(r, "failedCount"),
		RecipientCount:      integer(r, "recipientCount"),
		ThrottledUntil:      parseTime(str(r, "throttledUntil")),
		StartedAt:           parseTime(str(r, "startedAt")),
		ScheduledAt:         parseTime(str(r, "scheduledAt")),
		RosterCursor:        str(r, "rosterCursor"),
		RosterOutstanding:   boolean(r, "rosterOutstanding"),
	}
}

func boolean(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// bare normalizes a stored canonical id to the short form.
//
// Every id field on these concepts is an @relationship, so the engine
// canonicalizes it at insert and hands it back as `v1:ns:concept:short`.
// A named query argument is the BARE-ids contract
// (docs/public/concepts/identifiers.md), and an impersonated actor's
// userId has to be the same shape the JWT subject would have been -- so
// the conversion happens once, here, on the way out of a row.
func bare(v string) string { return memql.BareShortId(v) }

func str(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// integer reads a campaign row's numeric field.
//
// SATURATES out of range (memql#4779). Every value read through here is a
// count or a rate -- sentCount, attempts, ratePerMinute, the deliverability
// tallies -- and two of the readings depend on the order: `sentCount > 0` is
// the guard that stops a campaign being sent twice, and a wrapped negative
// would answer "nothing sent yet".
func integer(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// lastErrorCap bounds a persisted error, mirroring the outbound worker's.
const lastErrorCap = 4096

// truncateError renders an error for storage: NUL-free, then capped.
// The NUL substitution is not optional -- PostgreSQL's jsonb cannot
// represent U+0000, so a single such byte from a remote server makes the
// insert fail, and the write that fails is the one recording the failure
// (the memql#3035 shape, reached through Postgres instead of the lexer).
func truncateError(msg string) string {
	msg = strings.ReplaceAll(msg, "\x00", "�")
	if len(msg) > lastErrorCap {
		msg = msg[:lastErrorCap]
	}
	return msg
}

// sortedRecipientIDs is a deterministic iteration helper for tests and
// logs; map order would make a batch's composition unreproducible.
func sortedRecipientIDs(m map[string]LedgerEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- reputation + warming (memql#3462) ----------------------------------

// ReputationWindow is one (identity, domain, day, node) counter row.
type ReputationWindow struct {
	SendingIdentity string
	Domain          string
	WindowStart     string
	NodeID          string
	Accepted        int
	HardBounce      int
	SoftBounce      int
	Complaint       int
}

// WarmupState is the ramp's persisted position.
type WarmupState struct {
	Step          int
	RatePerMinute int
	Decision      string
	Reason        string
	EvaluatedAt   time.Time
	StepEnteredAt time.Time
	// AcceptedAtStepStart is the deployment-wide accepted total as it stood
	// when the current step began. Stored as a WATERMARK rather than as a
	// running per-step counter because the counters themselves live in
	// per-replica rows: subtracting a watermark from the current total gives
	// the step's volume without any replica having to know what the others
	// sent.
	AcceptedAtStepStart int
}

// ReputationSince reads every counter row from a UTC day onward.
// clusterOwner-tier.
func (s *Store) ReputationSince(ctx context.Context, since string) ([]ReputationWindow, error) {
	rows, err := s.rows(ctx, call("query", "reputationWindowsSince", arg{"since", since}))
	if err != nil {
		return nil, err
	}
	out := make([]ReputationWindow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReputationWindow{
			SendingIdentity: str(r, "sendingIdentity"),
			Domain:          str(r, "domain"),
			WindowStart:     str(r, "windowStart"),
			NodeID:          str(r, "nodeId"),
			Accepted:        integer(r, "accepted"),
			HardBounce:      integer(r, "hardBounce"),
			SoftBounce:      integer(r, "softBounce"),
			Complaint:       integer(r, "complaint"),
		})
	}
	return out, nil
}

// ReputationForBucket reads what THIS node already wrote for one bucket, so
// a restart mid-day seeds from it instead of clobbering it with a total that
// began again at zero.
func (s *Store) ReputationForBucket(ctx context.Context, key reputationKey, nodeID string) (reputationCounts, error) {
	rows, err := s.ReputationSince(ctx, key.day)
	if err != nil {
		return reputationCounts{}, err
	}
	for _, r := range rows {
		if r.WindowStart == key.day && r.Domain == key.domain && r.NodeID == nodeID && r.SendingIdentity == key.identity {
			return reputationCounts{
				accepted:   r.Accepted,
				hardBounce: r.HardBounce,
				softBounce: r.SoftBounce,
				complaint:  r.Complaint,
			}, nil
		}
	}
	return reputationCounts{}, nil
}

// RecordReputationWindow writes this node's running total for one bucket.
// clusterOwner-tier. Absolute values -- see the mutation's doc for why that
// is the concurrency design rather than a shortcut.
func (s *Store) RecordReputationWindow(ctx context.Context, key reputationKey, nodeID string, counts reputationCounts) error {
	return s.exec(ctx, call("mutation", "recordReputationWindow",
		arg{"sendingIdentity", key.identity},
		arg{"domain", key.domain},
		arg{"windowStart", key.day},
		arg{"nodeId", nodeID},
		arg{"accepted", counts.accepted},
		arg{"hardBounce", counts.hardBounce},
		arg{"softBounce", counts.softBounce},
		arg{"complaint", counts.complaint},
	))
}

// WarmupState reads the ramp's position for one sending identity.
// clusterOwner-tier.
func (s *Store) WarmupState(ctx context.Context, identity string) (WarmupState, bool, error) {
	rows, err := s.rows(ctx, call("query", "warmupStateForIdentity", arg{"sendingIdentity", identity}))
	if err != nil || len(rows) == 0 {
		return WarmupState{}, false, err
	}
	r := rows[len(rows)-1]
	return WarmupState{
		Step:                integer(r, "step"),
		RatePerMinute:       integer(r, "ratePerMinute"),
		Decision:            str(r, "decision"),
		Reason:              str(r, "reason"),
		EvaluatedAt:         parseTime(str(r, "evaluatedAt")),
		StepEnteredAt:       parseTime(str(r, "stepEnteredAt")),
		AcceptedAtStepStart: integer(r, "acceptedInStep"),
	}, true, nil
}

// RecordWarmupState persists a ramp decision. clusterOwner-tier.
func (s *Store) RecordWarmupState(ctx context.Context, identity string, d warmupDecision) error {
	return s.exec(ctx, call("mutation", "recordWarmupState",
		arg{"sendingIdentity", identity},
		arg{"step", d.Step},
		arg{"ratePerMinute", d.RatePerMinute},
		arg{"decision", d.Decision},
		arg{"reason", d.Reason},
		arg{"evaluatedAt", time.Now().UTC().Format(time.RFC3339)},
		arg{"stepEnteredAt", d.StepEnteredAt.UTC().Format(time.RFC3339)},
		arg{"acceptedInStep", d.AcceptedInStep},
	))
}
