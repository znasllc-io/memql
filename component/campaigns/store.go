package campaigns

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
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
	ID                 string
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
	}, true, nil
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

// Roster reads the whole audience, suppressed members included -- a
// `skipped` delivery row is an outcome the operator is owed rather than a
// silence. OWNED tier.
func (s *Store) Roster(ctx context.Context, audienceID string) ([]Recipient, error) {
	rows, err := s.rows(ctx, call("query", "audienceRosterForSend", arg{"audienceId", audienceID}))
	if err != nil {
		return nil, err
	}
	out := make([]Recipient, 0, len(rows))
	for _, r := range rows {
		out = append(out, Recipient{
			ID:                 bare(str(r, "id")),
			Email:              str(r, "email"),
			DisplayName:        str(r, "displayName"),
			SubscriptionStatus: str(r, "subscriptionStatus"),
		})
	}
	return out, nil
}

// Ledger reads the delivery ledger for one campaign, keyed by bare
// recipient id. THE read that makes a resumed send safe. OWNED tier.
func (s *Store) Ledger(ctx context.Context, campaignID string) (map[string]LedgerEntry, error) {
	rows, err := s.rows(ctx, call("query", "deliveryLedgerForCampaign", arg{"campaignId", campaignID}))
	if err != nil {
		return nil, err
	}
	out := make(map[string]LedgerEntry, len(rows))
	for _, r := range rows {
		recipientID := bare(str(r, "recipientId"))
		if recipientID == "" {
			continue
		}
		entry := LedgerEntry{
			RecipientID:   recipientID,
			Status:        str(r, "status"),
			Attempts:      integer(r, "attempts"),
			NextAttemptAt: parseTime(str(r, "nextAttemptAt")),
		}
		// LAST WRITE WINS on a duplicate. The query sorts oldest-first and
		// a re-attempt appends a new version under the SAME row id, so the
		// later row is the current state. Reading the first would resume
		// against a stale outcome and re-mail somebody.
		out[recipientID] = entry
	}
	return out, nil
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
func (s *Store) EnqueueSend(ctx context.Context, job SendJob) error {
	return s.exec(ctx, call("mutation", "enqueueCampaignSend",
		arg{"campaignId", job.CampaignID},
		arg{"campaignOwnerUserId", job.CampaignOwnerUserID},
		arg{"audienceId", job.AudienceID},
		arg{"templateId", job.TemplateID},
	))
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

func integer(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
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
