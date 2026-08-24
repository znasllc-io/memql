package release

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// store.go -- the graph writes a cut makes, and the one place this package
// stamps internal origin.
//
// ===========================================================================
// WHY THE STAMP IS HERE AND NOWHERE ELSE
// ===========================================================================
// auth.CallOrigin's zero value is OriginClient, so a context that does not SAY
// it is internal is treated as a client call whatever actor it carries -- and
// component/memql/engine.go refuses every @serverOnly construct on a client
// origin. Both writes below are @serverOnly, so without the stamp every one of
// them is refused, and the failure is one WARN per call with an empty table
// behind it. That is the shape memql#2989 and the recovery-key entry in
// call_origin_conformance_test.go both record.
//
// The stamp is applied INLINE, as the argument to one Execute, on a context
// this function constructs. It is never returned. That is what bounds it: no
// later frame can inherit the mark and use it to open a different @serverOnly
// construct, which is the escalation memql#2989 found.
//
// ===========================================================================
// THE ALLOWLIST ENTRY THIS PACKAGE HOLDS IS "REQUEST-DERIVED"
// ===========================================================================
// Stated honestly rather than borrowed: the caller IS a request, an owner
// clicking a button in the portal, which is the shape call_origin.go warns
// about in as many words. What earns the exception is a precondition that is
// ASSERTED rather than asserted-about:
//
//   every path into this file is downstream of the Go owner wall, in the same
//   function, before any network call -- and owner_wall_test.go drives a
//   non-owner actor through the handler and proves the engine is never reached.
//
// It is also narrow in a way the identity entries are not. The stamp reaches
// exactly three constructs -- two release-cut writes and createAuditEvent --
// none of which returns a row to the caller, and the widest thing a leaked
// context could do here is append to an append-only release history that an
// owner may already append to.

// conceptId is the row set this package owns.
const conceptId = "v1:cluster:releaseCut"

// Store writes the release-cut row and its audit event.
type Store struct {
	engine memql.IntegrationEngineAccess
	now    func() time.Time
}

// NewStore builds a store over the engine.
func NewStore(engine memql.IntegrationEngineAccess) *Store {
	return &Store{engine: engine, now: func() time.Time { return time.Now().UTC() }}
}

// executeServerOnly runs one @serverOnly call under internal origin.
//
// The ONE seam, deliberately -- the identity store's executeAndExtractInternal
// carries the same note. A stamp hand-rolled at a call site is a stamp the
// next call site copies slightly wrong, and the conformance gate polices
// packages rather than call sites, so it would not notice.
//
// It returns a RESULT and never a context, which is the structural half of the
// bound described above.
func (s *Store) executeServerOnly(ctx context.Context, call string) (*memql.ExecuteResult, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("release: no engine is wired; the row cannot be written")
	}
	return s.engine.Execute(auth.ContextWithInternalOrigin(ctx), call)
}

// Record is the row a cut writes.
type Record struct {
	Version          string
	Bump             string
	BaseSha          string
	RequestedBy      string
	RequestedByEmail string
	Status           string
	ReleaseURL       string
	TagName          string
	Error            string
	PinBumpPrURL     string
	PinBumpNote      string
}

// WriteCut appends the release-cut row.
//
// A FAILURE HERE DOES NOT UNDO THE RELEASE, and the caller must not treat it
// as though it did. By the time this runs the tag exists and the Release is
// published; the cascade has already fired. Reporting the cut as failed
// because its bookkeeping row did not land would tell an operator to cut
// again, which is the one thing they must not do.
func (s *Store) WriteCut(ctx context.Context, rec Record) error {
	call := renderCall("createReleaseCut", map[string]any{
		"version":          rec.Version,
		"bump":             rec.Bump,
		"baseSha":          rec.BaseSha,
		"requestedBy":      rec.RequestedBy,
		"requestedByEmail": rec.RequestedByEmail,
		"status":           rec.Status,
		"releaseUrl":       rec.ReleaseURL,
		"tagName":          rec.TagName,
		"error":            rec.Error,
		"pinBumpPrUrl":     rec.PinBumpPrURL,
		"pinBumpNote":      rec.PinBumpNote,
		"dispatchedAt":     s.now().Format(time.RFC3339),
	})
	if _, err := s.executeServerOnly(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("release: record the cut of %s: %w", rec.Version, err)
	}
	return nil
}

// UpdateStatus moves a row to what a check verified.
func (s *Store) UpdateStatus(ctx context.Context, version, status, errText string) error {
	call := renderCall("updateReleaseCutStatus", map[string]any{
		"version":   version,
		"status":    status,
		"error":     errText,
		"checkedAt": s.now().Format(time.RFC3339),
	})
	if _, err := s.executeServerOnly(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("release: update the status of %s: %w", version, err)
	}
	return nil
}

// CutByVersion reads one release-cut row, or reports false when this cluster
// has no row for that version.
//
// A BY-ID READ, NOT A SCAN OF THE HISTORY LIST. releaseCuts paginates 50 --
// correct for a portal list, wrong for a lookup: an installation past its
// fiftieth release would miss every older version here and the caller would
// answer version_not_cut, which MEANS "cut by hand, or on another
// installation". That is a confident wrong answer produced by a page boundary
// nobody could see from the message, which is exactly the class of thing the
// rest of this package refuses to do.
//
// The read goes through the SAME internal-origin seam as the writes. It runs on
// behalf of a caller the Go wall has already admitted as an owner, and the row
// it fetches is the one the status check is about to write rather than a
// listing anyone sees.
func (s *Store) CutByVersion(ctx context.Context, version string) (map[string]any, bool, error) {
	call := renderCall("releaseCutByVersion", map[string]any{"version": version})
	res, err := s.executeServerOnly(ctx, "query "+call)
	if err != nil {
		return nil, false, fmt.Errorf("release: read the cut %s: %w", version, err)
	}
	if res == nil || res.Bundle == nil {
		return nil, false, nil
	}
	for _, node := range res.Bundle.GetNodes() {
		if node == nil {
			continue
		}
		payload := map[string]any{}
		if p := node.GetPayload(); p != nil {
			payload = p.AsMap()
		}
		// The by-id filter has already selected; the version check stays
		// as a belt-and-braces guard against an id collision between
		// concepts, which resolveFullId makes unlikely rather than
		// impossible.
		if asString(payload["version"]) == version {
			payload["id"] = node.GetId()
			return payload, true, nil
		}
	}
	return nil, false, nil
}

// WriteAudit appends a `release_cut` event to the DECISIONS log.
//
// v1:identity:auditEvent AND NOT v1:identity:authActivity, and the split is
// memql#4328's: this log records decisions and security signals, the other
// records routine mechanics. Shipping a version of the platform is a decision
// if anything is -- it is the single most consequential button in the portal --
// and it happens a handful of times a month, which is exactly the volume the
// Audit Trail was split to keep readable.
//
// A FAILURE HERE IS LOGGED BY THE CALLER AND NEVER FAILS THE CUT, for
// WriteCut's reason. The row and the event are two writes and there is no
// transaction spanning them; the alternative -- refusing the cut because the
// audit line did not land -- reports a release that shipped as one that did
// not.
func (s *Store) WriteAudit(ctx context.Context, rec Record, actorRole string) error {
	detail := map[string]any{
		"version": rec.Version,
		"bump":    rec.Bump,
		"baseSha": rec.BaseSha,
		"status":  rec.Status,
	}
	if rec.ReleaseURL != "" {
		detail["releaseUrl"] = rec.ReleaseURL
	}
	if rec.Error != "" {
		detail["error"] = rec.Error
	}
	outcome := "success"
	if rec.Status == "failed" || rec.Status == "tag_created_release_failed" {
		outcome = "failure"
	}
	now := s.now()
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "v1:identity:auditEvent:" + id.NewShortId(),
		"occurredAt":  now.Format(time.RFC3339),
		"category":    "admin",
		"action":      "release_cut",
		"actorUserId": rec.RequestedBy,
		"actorEmail":  rec.RequestedByEmail,
		"actorRole":   actorRole,
		"targetType":  "releaseCut",
		"targetId":    rec.Version,
		"detail":      detail,
		"outcome":     outcome,
	})
	if _, err := s.executeServerOnly(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("release: audit the cut of %s: %w", rec.Version, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Call rendering
// ---------------------------------------------------------------------------

// renderCall composes `name(arg: value, ...)` with arguments in a stable
// order.
//
// SORTED, so the call string a test asserts on is the call string that ships.
// Argument order carries no meaning to the engine, and an unordered map would
// make every assertion here flaky in a way that reads as a real failure.
//
// A BLANK-STRING ARGUMENT IS OMITTED rather than sent. The mutations spell
// their optional fields `args.X ?? ""`, and `??` is blank-coalescing, so
// sending "" reaches the same value -- but omitting it keeps the call short
// and, more importantly, keeps an OPTIONAL argument optional in the one place
// it matters: `update{}` is a read-merge, and a blank sent for a field the
// caller did not mean to change would overwrite it.
func renderCall(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return name + "()"
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+renderValue(args[k]))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// renderValue emits one value as a MemQL literal.
//
// OBJECT KEYS ARE QUOTED, matching integrations/shopify/render.go's rule and
// for a weaker version of its reason: the only object rendered here is the
// audit detail, whose keys this file writes -- but a quoted key is correct for
// every key rather than for most of them, and a renderer that asks whether a
// key happens to be safe is a renderer that is eventually asked about one that
// is not.
func renderValue(v any) string {
	switch value := v.(type) {
	case string:
		return languageParser.QuoteString(value)
	case bool:
		if value {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", value)
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, languageParser.QuoteString(k)+": "+renderValue(value[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		// Never a bare Go rendering: %v on an unexpected type emits a
		// fragment that corrupts the statement around it. A quoted
		// string is wrong-but-parseable, which fails visibly.
		return languageParser.QuoteString(fmt.Sprintf("%v", v))
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
