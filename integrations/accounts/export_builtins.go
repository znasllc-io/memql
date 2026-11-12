package accounts

// export_builtins.go -- the seven export calls, the three methods the download
// route asks of this package, and the audit trail (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections F, G
// and H).
//
// # Who may call
//
// Every call declares @requiresCapability("execute", "app:accounts/export")
// (dsl/accounts/builtins.memql), seeded on owner and developer only, and the
// engine refuses every other caller before a handler here runs
// (component/memql/export_capability_gate_test.go). The reads
// (accountExportReadiness, accountExports, accountExportStatus) need nothing
// more. Every act -- prepare, start, cancel, remove -- is a person's decision,
// so it also asks auth.RequirePersonSession FIRST, before it reads anything:
// a personal access token, a machine credential, delegated access, borrowed
// authority and a call made inside a run are all refused
// (person_session_required).
//
// # Why a second type beside Integration
//
// The inventory's Integration is another task's file. The plug-in registers a
// Provider (plugin.go) that embeds both and answers the union of their
// capabilities under the one name "accounts", so a builtin's executor
// `integration.accounts.<name>` reaches either half. The download route and
// the agent node's runner look the provider up by that name and reach the
// export half through it.
//
// # The passphrase
//
// Prepare generates it, seals it on the draft and returns it on that one
// answer row and nowhere else. It is never put in a log line, an audit detail
// or an error; export_db_test.go captures all three and searches them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/accountexport"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// The audit vocabulary. `accountExport` is declared on v1:identity:auditEvent's
// targetType enum; `data` is the category the design names.
const (
	auditCategoryData        = "data"
	auditTargetAccountExport = "accountExport"

	auditExportPrepared   = "account_export_prepared"
	auditExportStarted    = "account_export_started"
	auditExportCompleted  = "account_export_completed"
	auditExportFailed     = "account_export_failed"
	auditExportCancelled  = "account_export_cancelled"
	auditExportDownloaded = "account_export_downloaded"
	auditExportRemoved    = "account_export_removed"

	// auditWriteTimeout bounds an audit write that outlives its request.
	auditWriteTimeout = 5 * time.Second
)

// The list's page bounds.
const (
	exportListDefault = 20
	exportListMax     = 100
)

// downloadRangeMax is how much of a request's Range header the download's
// audit event keeps.
const downloadRangeMax = 200

// primaryAccountName names the installation's own account when it has no row.
const primaryAccountName = "Primary account"

// ExportWork is what a claimed export runs. The run task supplies the real one;
// a node with none set claims nothing. x.Attempt is part of what it receives
// because each attempt writes under a storage key of its own.
//
// WHO REMOVES WHAT. The work cleans up an attempt that FAILS, or is cancelled,
// before it returns: it returns an error and leaves nothing behind. A result it
// returns (err == nil, ObjectKey set) is the runner's from then on. The runner
// records it, or -- when it cannot, because the lease was lost, a cancel landed
// after the work finished, the node is stopping, or the final write matched
// nothing -- removes the object through the blob store, so no file is left that
// no record names (export_runner.go, discard).
type ExportWork func(ctx context.Context, x Export, passphrase string, progress func(ExportProgress)) (ExportResult, error)

// ExportProgress is where a running attempt stands.
type ExportProgress struct {
	Phase                        string
	KindsTotal, KindsDone        int
	RowsWritten, VersionsWritten int64
	BytesTotal, BytesDone        int64
}

// ExportResult is a finished file.
type ExportResult struct {
	ObjectKey, FileName, SHA256 string
	SizeBytes                   int64
	Report                      json.RawMessage
}

// Executor is the one engine method this half needs: the audit write and the
// owners' addresses go through the engine's ordinary mutation and query.
type Executor interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// ExportBlobs is the export blob store (integrations/azureblob's
// AccountExportStore).
type ExportBlobs interface {
	Delete(ctx context.Context, key string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	OpenRange(ctx context.Context, key string, offset, count int64) (io.ReadCloser, error)
}

// Exports is the export half of the accounts plug-in.
type Exports struct {
	inv    *Integration
	engine Executor
	logger *slog.Logger

	// The readiness seams. nil is "condition not met": readiness fails closed
	// on a node where one was not wired, rather than calling an account ready
	// on a question it never asked.
	assignmentStates func(context.Context, memql.AccountQuerier) ([]memql.AccountAssignmentState, error)
	undeclared       func([]*memorynodes.Concept) []string
	followPending    func(context.Context, memql.AccountQuerier, string) (int64, error)

	// notifyOwners mails every active owner when an export starts
	// (export_notice.go). nil is skipped.
	notifyOwners func(ctx context.Context, x Export, startedBy string)
	// blobs is the export blob store, built on first use: a node with no
	// storage configured still serves every read here.
	blobs func() (ExportBlobs, error)

	// newPassphrase, seal and open are accountexport.GeneratePassphrase and
	// component/secret; fields so nothing else can stand in for them by
	// accident and a test needs no other key handling.
	newPassphrase func() (string, error)
	seal          func(string) (string, error)
	open          func(string) (string, error)

	// work is what a claimed export runs, set by the run task's wiring.
	work atomic.Pointer[ExportWork]

	// notices counts the owners' emails still being sent, off the request; a
	// test waits on it.
	notices sync.WaitGroup
	// markRemovedHook stands in for markRemoved in a test of its failure.
	markRemovedHook func(ctx context.Context, id, by string) (Export, bool, error)
	// getHook stands in for get in notStartable, in a test of a failed lookup.
	getHook func(ctx context.Context, id string) (Export, bool, error)
	// names resolves people's display names for the answer rows; nil leaves
	// them empty. Defaults to displayNames below.
	names func(ctx context.Context, ids []string) (map[string]string, error)
}

// NewExports builds the export half over the inventory's integration.
func NewExports(inv *Integration, engine Executor, logger *slog.Logger) *Exports {
	if logger == nil {
		logger = slog.Default()
	}
	return &Exports{
		inv: inv, engine: engine, logger: logger,
		newPassphrase: accountexport.GeneratePassphrase,
		// The fingerprint secret.Encrypt also answers ends in the passphrase's
		// last four characters, so it is dropped here and never kept.
		seal: func(p string) (string, error) {
			ct, _, err := secret.Encrypt(p)
			return ct, err
		},
		open: secret.Decrypt,
	}
}

// SetExportWork installs what a claimed export runs.
func (s *Exports) SetExportWork(w ExportWork) {
	if w == nil {
		s.work.Store(nil)
		return
	}
	s.work.Store(&w)
}

func (s *Exports) exportWork() ExportWork {
	if p := s.work.Load(); p != nil {
		return *p
	}
	return nil
}

// Capabilities are the export half's builtins.
func (s *Exports) exportCapabilities() []memql.IntegrationCapability {
	account := map[string]string{"accountId": "string (required) -- the account, bare or canonical."}
	export := map[string]string{"exportId": "string (required) -- the export."}
	return []memql.IntegrationCapability{
		{Name: "exportReadiness", Description: "Whether an account can be exported: the five conditions, the rows in no account, and the verdict.", Handler: s.handleReadiness, ArgsSchema: account, PreserveOrder: true},
		{Name: "exportPrepare", Description: "Prepare an export of an account as a draft; a protected one returns its passphrase once.", Handler: s.handlePrepare,
			ArgsSchema: map[string]string{"accountId": account["accountId"], "protect": "bool -- encrypt the file with a passphrase shown once."}, PreserveOrder: true},
		{Name: "exportStart", Description: "Queue a prepared export.", Handler: s.handleStart, ArgsSchema: export, PreserveOrder: true},
		{Name: "exports", Description: "An account's exports, newest first, paged.", Handler: s.handleList,
			ArgsSchema: map[string]string{"accountId": account["accountId"], "after": "string -- the cursor of the previous page.", "limit": "int -- at most 100, default 20."}, PreserveOrder: true},
		{Name: "exportStatus", Description: "One export's state, phase, counts and report.", Handler: s.handleStatus, ArgsSchema: export, PreserveOrder: true},
		{Name: "exportCancel", Description: "Cancel an export that is prepared, queued or running.", Handler: s.handleCancel, ArgsSchema: export, PreserveOrder: true},
		{Name: "exportRemove", Description: "Delete a complete export's file and keep its record.", Handler: s.handleRemove, ArgsSchema: export, PreserveOrder: true},
	}
}

// ---------------------------------------------------------------------------
// Arguments and answers

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func boolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

// intArg reads a whole-number argument, or def when the call does not carry it.
//
// narrowing: SATURATE -- both callers read a page size and bound it themselves
// (a size above the page's maximum is the maximum; one below 1 is the default
// in the export list and a refusal in the rows list). A number too large for
// an int has to stay "too large" for those bounds to see, and must never come
// back negative.
func intArg(args map[string]any, key string, def int) (int, error) {
	switch v := args[key].(type) {
	case nil:
		return def, nil
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return num.ClampInt64(v), nil
	case float64:
		// NaN is not equal to its own truncation, so it is refused here too.
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("%s must be a whole number", key)
		}
		return num.ClampFloat64(v), nil
	default:
		return 0, fmt.Errorf("%s must be a number", key)
	}
}

func exportIDArg(args map[string]any, call string) (string, error) {
	id := stringArg(args, "exportId")
	if id == "" {
		return "", fmt.Errorf("%s needs an export: exportId is empty", call)
	}
	return id, nil
}

func accountArg(args map[string]any, call string) (string, error) {
	raw := stringArg(args, "accountId")
	if raw == "" {
		return "", fmt.Errorf("%s needs an account: accountId is empty", call)
	}
	return memql.BareAccountID(raw)
}

// exportPayload is the answer row of one export. withReport adds the report.
func exportPayload(x Export, withReport bool) (map[string]any, error) {
	if !withReport {
		x.Report = nil
	}
	encoded, err := json.Marshal(x)
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err := json.Unmarshal(encoded, &p); err != nil {
		return nil, err
	}
	p["kind"] = "export"
	return p, nil
}

func exportNode(x Export, withReport bool, extra map[string]any) ([]memorynodes.MemoryNode, error) {
	p, err := exportPayload(x, withReport)
	if err != nil {
		return nil, err
	}
	for k, v := range extra {
		p[k] = v
	}
	n, err := node("export:"+x.ID, p)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{n}, nil
}

func notFound(id string) error { return fmt.Errorf("there is no export %q", id) }

// ---------------------------------------------------------------------------
// The calls

func (s *Exports) handleReadiness(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	account, err := accountArg(args, "accountExportReadiness")
	if err != nil {
		return nil, err
	}
	r, err := s.Readiness(ctx, account)
	if err != nil {
		return nil, err
	}
	return r.nodes()
}

// accountNameSQL reads the account row's name, newest version first.
//
// staged-data: MUST-NOT-GATE -- naming the account an export is of; a staged account is still the account the export holds, and refusing it would refuse an export the readiness conditions allow.
const accountNameSQL = `SELECT COALESCE(payload->>'name', '') FROM "MemoryNodes"
	WHERE concept = $1 AND id IN ($2, $3) ORDER BY "createdAt" DESC LIMIT 1`

// accountName answers the account's name, refusing an unknown account. The
// primary account is one by definition, whether or not its row is written.
func (s *Exports) accountName(ctx context.Context, bare string) (string, error) {
	db, err := s.inv.database()
	if err != nil {
		return "", err
	}
	var name string
	err = db.DB.QueryRowContext(ctx, accountNameSQL, accountConcept, canonicalPrefix+bare, bare).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows) && bare == primaryAccount:
		return primaryAccountName, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("there is no account %q", bare)
	case err != nil:
		return "", fmt.Errorf("look up the account: %w", err)
	}
	if strings.TrimSpace(name) == "" && bare == primaryAccount {
		name = primaryAccountName
	}
	return name, nil
}

func (s *Exports) handlePrepare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userID, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	account, err := accountArg(args, "accountExportPrepare")
	if err != nil {
		return nil, err
	}
	name, err := s.accountName(ctx, account)
	if err != nil {
		return nil, err
	}
	ready, err := s.Readiness(ctx, account)
	if err != nil {
		return nil, err
	}
	if unmet := ready.unmet(); len(unmet) > 0 {
		return nil, fmt.Errorf("%s: This account cannot be exported yet. Not met: %s.", FailureNotReady, strings.Join(unmet, ", "))
	}

	protect := boolArg(args, "protect")
	var pass string
	var sealed sql.NullString
	if protect {
		pass, err = s.newPassphrase()
		if err != nil {
			return nil, errors.New("accountExportPrepare: a passphrase could not be generated")
		}
		ct, err := s.seal(pass)
		if err != nil {
			// The sealing error names the key's state, never the plaintext.
			return nil, fmt.Errorf("accountExportPrepare: the passphrase could not be sealed: %w", err)
		}
		sealed = sql.NullString{String: ct, Valid: true}
	}
	x, err := s.insertDraft(ctx, Export{
		ID: "aex-" + id.NewShortId(), AccountID: account, AccountName: name, RequestedBy: userID, Protected: protect,
	}, sealed)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, auditEntry{Action: auditExportPrepared, Export: x})
	var extra map[string]any
	if protect {
		extra = map[string]any{"passphrase": pass}
	}
	return s.exportNodes(ctx, []Export{x}, false, extra)
}

func (s *Exports) handleStart(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := auth.RequirePersonSession(ctx); err != nil {
		return nil, err
	}
	id, err := exportIDArg(args, "accountExportStart")
	if err != nil {
		return nil, err
	}
	x, ok, err := s.queue(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("accountExportStart: %w", err)
	}
	if !ok {
		return nil, s.notStartable(ctx, id)
	}
	s.audit(ctx, auditEntry{Action: auditExportStarted, Export: x})
	s.mailOwners(ctx, x)
	return s.exportNodes(ctx, []Export{x}, false, nil)
}

// startedBy is how the owners' email names who started an export: their
// address when the session carries one, else their id.
func startedBy(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		if strings.TrimSpace(ac.PrimaryEmail) != "" {
			return ac.PrimaryEmail
		}
		return ac.UserId
	}
	return ""
}

func (s *Exports) handleList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	account, err := accountArg(args, "accountExports")
	if err != nil {
		return nil, err
	}
	limit, err := intArg(args, "limit", exportListDefault)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = exportListDefault
	}
	if limit > exportListMax {
		limit = exportListMax
	}
	exports, more, err := s.list(ctx, account, stringArg(args, "after"), limit)
	if err != nil {
		return nil, err
	}
	out, err := s.exportNodes(ctx, exports, false, nil)
	if err != nil {
		return nil, err
	}
	next := ""
	if more && len(exports) > 0 {
		next = encodeExportCursor(exports[len(exports)-1])
	}
	n, err := node("page", map[string]any{"kind": "page", "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return append(out, n), nil
}

func (s *Exports) handleStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	id, err := exportIDArg(args, "accountExportStatus")
	if err != nil {
		return nil, err
	}
	x, ok, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound(id)
	}
	return s.exportNodes(ctx, []Export{x}, true, nil)
}

func (s *Exports) handleCancel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := auth.RequirePersonSession(ctx); err != nil {
		return nil, err
	}
	id, err := exportIDArg(args, "accountExportCancel")
	if err != nil {
		return nil, err
	}
	x, ok, err := s.cancel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("accountExportCancel: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("export %q is not prepared, queued or running, so there is nothing to cancel", id)
	}
	// A running export is audited and counted when its runner writes the
	// final state; one that ended here is ended now.
	if x.State == exportCancelled {
		s.audit(ctx, auditEntry{Action: auditExportCancelled, Export: x})
		metrics.AccountExportFinished(metrics.AccountExportCancelled)
	}
	return s.exportNodes(ctx, []Export{x}, false, nil)
}

func (s *Exports) handleRemove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userID, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	id, err := exportIDArg(args, "accountExportRemove")
	if err != nil {
		return nil, err
	}
	x, ok, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound(id)
	}
	if x.State != exportComplete || x.RemovedAt != nil {
		return nil, fmt.Errorf("only a complete file that is still kept can be removed; export %q is %s", id, removedOr(x))
	}
	// The object goes first: a record that says "removed" over a file that is
	// still there would be the worse half to leave behind.
	if x.ObjectKey != "" {
		blobs, err := s.blobStore()
		if err != nil {
			return nil, err
		}
		if err := blobs.Delete(ctx, x.ObjectKey); err != nil {
			return nil, fmt.Errorf("accountExportRemove: delete the file: %w", err)
		}
	}
	mark := s.markRemoved
	if s.markRemovedHook != nil {
		mark = s.markRemovedHook
	}
	x, ok, err = mark(ctx, id, userID)
	if err != nil {
		// The object is already gone and Delete answers a missing object as
		// done, so a second removal finishes the record.
		return nil, fmt.Errorf("the file was deleted, but its record could not be updated; try the removal again: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("export %q was removed by someone else", id)
	}
	s.audit(ctx, auditEntry{Action: auditExportRemoved, Export: x})
	return s.exportNodes(ctx, []Export{x}, false, nil)
}

func removedOr(x Export) string {
	if x.RemovedAt != nil {
		return "removed"
	}
	return x.State
}

func (s *Exports) blobStore() (ExportBlobs, error) {
	if s.blobs == nil {
		return nil, errors.New("this node has no storage for export files")
	}
	return s.blobs()
}

// ---------------------------------------------------------------------------
// The download route's three methods (component/server/account_export_handler.go).

// ResolveExportForDownload answers ok only for a complete export whose file was
// not removed.
func (s *Exports) ResolveExportForDownload(ctx context.Context, exportID string) (Export, bool, error) {
	x, ok, err := s.get(ctx, strings.TrimSpace(exportID))
	if err != nil || !ok {
		return Export{}, false, err
	}
	if x.State != exportComplete || x.RemovedAt != nil || x.ObjectKey == "" {
		return Export{}, false, nil
	}
	return x, true, nil
}

// OpenExport streams the file from offset; length < 0 means to the end.
func (s *Exports) OpenExport(ctx context.Context, x Export, offset, length int64) (io.ReadCloser, error) {
	blobs, err := s.blobStore()
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("invalid offset %d", offset)
	}
	if length < 0 {
		if offset == 0 {
			return blobs.Open(ctx, x.ObjectKey)
		}
		length = x.SizeBytes - offset
		if length <= 0 {
			return io.NopCloser(strings.NewReader("")), nil
		}
	}
	return blobs.OpenRange(ctx, x.ObjectKey, offset, length)
}

// RecordExportDownload writes the audit event for one download request, with
// the range as sent. Unlike the other events its failure is returned: the
// route serves no byte without its record.
func (s *Exports) RecordExportDownload(ctx context.Context, x Export, userID, rangeAsked string) error {
	// As sent, to the length a real header has: a Range is a few dozen
	// characters, and the trail keeps no more of one that is not.
	if len(rangeAsked) > downloadRangeMax {
		rangeAsked = strings.ToValidUTF8(rangeAsked[:downloadRangeMax], "")
	}
	return s.writeAudit(ctx, auditEntry{Action: auditExportDownloaded, Export: x, ActorID: userID,
		Detail: map[string]any{"range": rangeAsked}})
}

// ---------------------------------------------------------------------------
// The audit trail

type auditEntry struct {
	Action  string
	Export  Export
	ActorID string // defaults to the session's person
	// System marks an event the cluster wrote (a run that ended), with no
	// person acting.
	System bool
	Detail map[string]any
}

// auditDetail is what every event carries. Never the passphrase, never the
// storage key, never a failure's free text: a code is enough to read the
// trail by, and the record holds the rest.
func auditDetail(e auditEntry) map[string]any {
	d := map[string]any{
		"accountId": e.Export.AccountID, "accountName": e.Export.AccountName,
		"state": e.Export.State, "protected": e.Export.Protected, "attempt": e.Export.Attempt,
	}
	if e.Export.FailureCode != "" {
		d["failureCode"] = e.Export.FailureCode
	}
	if e.Export.State == exportComplete {
		d["sizeBytes"], d["sha256"] = e.Export.SizeBytes, e.Export.SHA256
	}
	for k, v := range e.Detail {
		d[k] = v
	}
	return d
}

// audit writes an event for an act that landed. A FAILED AUDIT WRITE DOES NOT
// FAIL THE ACT: the record is already written, and returning the error would
// report a failure that did not happen and invite a retry. It is logged.
func (s *Exports) audit(ctx context.Context, e auditEntry) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := s.writeAudit(detached, e); err != nil {
		s.logger.Warn("account export: the audit event did not land", "action", e.Action, "exportId", e.Export.ID, "error", err)
	}
}

func (s *Exports) writeAudit(ctx context.Context, e auditEntry) error {
	if s.engine == nil {
		return errors.New("account export: no engine to write the audit event")
	}
	actorID, actorEmail, actorRole := e.ActorID, "", ""
	if e.System {
		actorRole = "system"
	} else if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		if actorID == "" {
			actorID = ac.UserId
		}
		actorEmail, actorRole = ac.PrimaryEmail, string(ac.Role)
	}
	call, err := langparser.RenderCall("createAuditEvent", map[string]any{
		"eventId":     "v1:identity:auditEvent:" + id.NewShortId(),
		"occurredAt":  time.Now().UTC().Format(time.RFC3339Nano),
		"category":    auditCategoryData,
		"action":      e.Action,
		"actorUserId": actorID,
		"actorEmail":  actorEmail,
		"actorRole":   actorRole,
		"targetType":  auditTargetAccountExport,
		"targetId":    e.Export.ID,
		"outcome":     auditOutcome(e.Export),
		"detail":      auditDetail(e),
	})
	if err != nil {
		return err
	}
	// Internal origin for this one write: auditEvent's create admits a cluster
	// owner or server code, and a developer exporting is neither. The actor it
	// records is still the caller.
	_, err = s.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

func auditOutcome(x Export) string {
	if x.State == exportFailed {
		return "failure"
	}
	return "success"
}

// finished audits and counts an export the cluster ended (a run's last state,
// or a take-over that failed or cancelled it).
func (s *Exports) finished(ctx context.Context, x Export) {
	action, outcome := "", ""
	switch x.State {
	case exportComplete:
		action, outcome = auditExportCompleted, metrics.AccountExportComplete
	case exportFailed:
		action, outcome = auditExportFailed, metrics.AccountExportFailed
	case exportCancelled:
		action, outcome = auditExportCancelled, metrics.AccountExportCancelled
	default:
		return
	}
	metrics.AccountExportFinished(outcome)
	s.audit(auth.ContextWithSystemActor(ctx, "accounts"), auditEntry{Action: action, Export: x, System: true})
}

// lazyBlobs builds the blob store on first use, and again on the next use
// after a build that failed.
func lazyBlobs(build func() (ExportBlobs, error)) func() (ExportBlobs, error) {
	var mu sync.Mutex
	var store ExportBlobs
	return func() (ExportBlobs, error) {
		mu.Lock()
		defer mu.Unlock()
		if store != nil {
			return store, nil
		}
		// A failure is not kept: one transient error at first use must not
		// leave this node unable to remove or serve a file until it restarts.
		built, err := build()
		if err != nil {
			return nil, err
		}
		store = built
		return store, nil
	}
}

// ---------------------------------------------------------------------------
// The people on an answer row, the expired draft, and the owners' email

// userConcept and userPrefix name a person's row.
const (
	userConcept = "v1:identity:user"
	userPrefix  = "v1:identity:user:"
)

// exportNodes answers export rows with the people's display names beside their
// ids (`requestedByName`, `removedByName`), resolved once per distinct id in
// the answer. A person who cannot be resolved has an empty name; a failed
// lookup answers empty names rather than failing a read of the record.
func (s *Exports) exportNodes(ctx context.Context, xs []Export, withReport bool, extra map[string]any) ([]memorynodes.MemoryNode, error) {
	seen := map[string]bool{}
	var ids []string
	for _, x := range xs {
		for _, who := range []string{x.RequestedBy, x.RemovedBy} {
			if who = strings.TrimSpace(who); who != "" && !seen[who] {
				seen[who] = true
				ids = append(ids, who)
			}
		}
	}
	names := map[string]string{}
	if len(ids) > 0 {
		resolve := s.names
		if resolve == nil {
			resolve = s.displayNames
		}
		got, err := resolve(ctx, ids)
		if err != nil {
			s.logger.Warn("account export: the people's names could not be read; the answer carries their ids alone", "error", err)
		} else {
			names = got
		}
	}
	out := make([]memorynodes.MemoryNode, 0, len(xs))
	for _, x := range xs {
		row := map[string]any{"requestedByName": names[x.RequestedBy], "removedByName": names[x.RemovedBy]}
		for k, v := range extra {
			row[k] = v
		}
		n, err := exportNode(x, withReport, row)
		if err != nil {
			return nil, err
		}
		out = append(out, n...)
	}
	return out, nil
}

// displayNames reads the newest displayName of each person. An id is matched
// in both spellings, bare and v1:identity:user:<bare>, and answered under the
// spelling it was asked in.
//
// staged-data: MUST-NOT-GATE -- naming the person who started or removed an export, for an owner or developer who holds the export part; the identity concept is the cluster's own and never staged, and a missing name would only blank the label.
func (s *Exports) displayNames(ctx context.Context, ids []string) (map[string]string, error) {
	db, err := s.inv.database()
	if err != nil {
		return nil, err
	}
	asked := map[string]string{} // stored spelling -> asked spelling
	args := []any{userConcept}
	marks := make([]string, 0, 2*len(ids))
	for _, who := range ids {
		bare := strings.TrimPrefix(who, userPrefix)
		for _, spelling := range []string{userPrefix + bare, bare} {
			if _, dup := asked[spelling]; dup {
				continue
			}
			asked[spelling] = who
			args = append(args, spelling)
			marks = append(marks, fmt.Sprintf("$%d", len(args)))
		}
	}
	rows, err := db.DB.QueryContext(ctx, `SELECT DISTINCT ON (id) id, COALESCE(payload->>'displayName', '')
		  FROM "MemoryNodes" WHERE concept = $1 AND id IN (`+strings.Join(marks, ", ")+`)
		 ORDER BY id, "createdAt" DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var stored, name string
		if err := rows.Scan(&stored, &name); err != nil {
			return nil, err
		}
		if who, ok := asked[stored]; ok && strings.TrimSpace(name) != "" {
			out[who] = name
		}
	}
	return out, rows.Err()
}

// codeDraftExpired is the refusal the screen keys on when a draft can no longer
// be started.
const codeDraftExpired = "export_draft_expired"

// notStartable says why an export could not be queued: one that is no longer
// a draft says what it is; a draft known to be gone, or older than its
// lifetime and waiting for the sweep, is export_draft_expired; a lookup that
// failed is its own error.
func (s *Exports) notStartable(ctx context.Context, id string) error {
	get := s.get
	if s.getHook != nil {
		get = s.getHook
	}
	x, ok, err := get(ctx, id)
	if err != nil {
		// A failed read says nothing about the draft, so it is not answered as
		// expired: the person tries again rather than preparing a second one.
		return fmt.Errorf("export %q could not be read, so it was not started; try again: %w", id, err)
	}
	if ok && x.State != exportDraft {
		return fmt.Errorf("export %q is already %s, so it cannot be started again", id, x.State)
	}
	return fmt.Errorf("%s: This prepared export is no longer waiting to be started; prepare it again.", codeDraftExpired)
}

// ownerNoticeTimeout bounds the whole owners' email, every recipient included.
const ownerNoticeTimeout = 30 * time.Second

// mailOwners sends the owners' email after the start is committed, off the
// request: a slow or hanging mail provider must not hold a person's start.
// It runs on a context detached from the request, so the request ending does
// not cut it short, under one bound for the whole notice; its failures are
// logged by the notice itself.
func (s *Exports) mailOwners(ctx context.Context, x Export) {
	if s.notifyOwners == nil {
		return
	}
	by := startedBy(ctx)
	s.notices.Add(1)
	go func() {
		defer s.notices.Done()
		// The notice runs outside the request's handler, so a panic in it (the
		// owner lookup, the template, the sender) would take the node down
		// where it used to fail one request. It is recovered and logged with
		// the panic's type and message only: the value may be anything.
		defer func() {
			if v := recover(); v != nil {
				s.logger.Error("account export: the owners' email panicked and was not sent",
					"exportId", x.ID, "panicType", fmt.Sprintf("%T", v), "panic", panicMessage(v))
			}
		}()
		noticeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ownerNoticeTimeout)
		defer cancel()
		s.notifyOwners(noticeCtx, x, by)
	}()
}

// panicMessage is a recovered value's message: an error's text, a string as
// it is, and nothing for any other value.
func panicMessage(v any) string {
	switch p := v.(type) {
	case error:
		return p.Error()
	case string:
		return p
	}
	return ""
}
