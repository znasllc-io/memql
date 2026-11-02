package packages

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
)

// Auditor records the outcome of a deploy or a rollback on v1:identity:auditEvent.
//
// SUCCESS AND REFUSAL BOTH (design section E), which is the property worth
// stating: an audit log that only records failures answers "what went wrong"
// and not "who deployed this", and deploying somebody's code onto a hostname
// this cluster serves is exactly the second question.
type Auditor interface {
	Deploy(ctx context.Context, ev DeployAuditEvent)
	Rollback(ctx context.Context, ev RollbackAuditEvent)
}

// DeployAuditEvent is one recorded attempt.
type DeployAuditEvent struct {
	PackageId     string
	DeploymentId  string
	SourceVersion string
	Status        string
	// FailureReason is the stable refusal code when there is one. A failure
	// with no code is recorded as an internal error rather than as a refusal,
	// so a real fault is never filed as somebody's mistake.
	FailureReason string
	Deployables   []DeployableOutcome
}

// RollbackAuditEvent is how one rollback ended. A deploy's row carries its real
// error and how far it got, and a rollback has no row, so this carries both.
type RollbackAuditEvent struct {
	PackageId string
	// DeploymentId is the deployment it returned to, which is not changed by this.
	DeploymentId string
	DslVersion   string
	// Outcome is success, failure, or blocked for a refusal before anything moved.
	Outcome string
	// FailureReason is the real error, whichever stage stopped it. A deploy's is a
	// stable code because its row has the sentence. This is what the Audit section
	// shows as why it failed, and a refusal's sentence starts with its code.
	FailureReason string
	// Sites are the ones it put back, which on a failure can be some of them.
	Sites        []string
	PointerMoved bool
	Rolled       bool
}

// engineAuditor writes through the same createAuditEvent mutation every other
// Go writer in this tree uses.
type engineAuditor struct {
	engine Engine
	logger *slog.Logger
}

// AuditAction is the action string this epic writes. Named rather than
// inlined, because the OS and any operator query filtering the trail key on it.
const AuditAction = "package_deploy"

// AuditActionRollback is the same trail's word for a rollback ending.
const AuditActionRollback = "package_rollback"

func (a *engineAuditor) Deploy(ctx context.Context, ev DeployAuditEvent) {
	if a == nil || a.engine == nil {
		return
	}

	outcome := "success"
	switch ev.Status {
	case StatusRefused:
		outcome = "blocked"
	case StatusFailed:
		outcome = "failure"
	}

	detail := map[string]any{
		"deploymentId":  ev.DeploymentId,
		"sourceVersion": ev.SourceVersion,
		"status":        ev.Status,
	}
	if len(ev.Deployables) > 0 {
		sites := make([]string, 0, len(ev.Deployables))
		for _, d := range ev.Deployables {
			if d.SiteId != "" {
				sites = append(sites, d.SiteId)
			}
		}
		detail["sites"] = sites
	}
	a.write(ctx, AuditAction, ev.PackageId, ev.DeploymentId, outcome, ev.FailureReason, detail)
}

func (a *engineAuditor) Rollback(ctx context.Context, ev RollbackAuditEvent) {
	if a == nil || a.engine == nil {
		return
	}
	detail := map[string]any{
		"deploymentId": ev.DeploymentId,
		"dslVersion":   ev.DslVersion,
		"pointerMoved": ev.PointerMoved,
		"rolled":       ev.Rolled,
	}
	if len(ev.Sites) > 0 {
		detail["sites"] = ev.Sites
	}
	a.write(ctx, AuditActionRollback, ev.PackageId, ev.DeploymentId, ev.Outcome, ev.FailureReason, detail)
}

func (a *engineAuditor) write(ctx context.Context, action, packageId, deploymentId, outcome, failureReason string, detail map[string]any) {
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		detailJSON = []byte("{}")
	}

	var b strings.Builder
	b.WriteString("mutation createAuditEvent(eventId: ")
	b.WriteString(langparser.QuoteString("audit-" + id.NewShortId()))
	b.WriteString(", occurredAt: ")
	b.WriteString(langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)))
	b.WriteString(", category: \"configuration\", action: ")
	b.WriteString(langparser.QuoteString(action))
	if ac, ok := auth.AccessFromContext(ctx); ok {
		b.WriteString(", actorUserId: ")
		b.WriteString(langparser.QuoteString(ac.UserId))
		if ac.PrimaryEmail != "" {
			b.WriteString(", actorEmail: ")
			b.WriteString(langparser.QuoteString(ac.PrimaryEmail))
		}
		if ac.Role != "" {
			b.WriteString(", actorRole: ")
			b.WriteString(langparser.QuoteString(string(ac.Role)))
		}
		if ac.IdentityId != "" {
			b.WriteString(", actorIdentityId: ")
			b.WriteString(langparser.QuoteString(ac.IdentityId))
		}
	}
	// targetId names the PACKAGE and targetType is left at its default, for
	// the reason sitePublishFromArtifact leaves it too: adding an enum value
	// is a DSL change with a conformance gate behind it, and the trail reads
	// perfectly well with the action naming what kind of thing this was.
	b.WriteString(", targetId: ")
	b.WriteString(langparser.QuoteString(packageId))
	b.WriteString(", outcome: ")
	b.WriteString(langparser.QuoteString(outcome))
	if failureReason != "" {
		b.WriteString(", failureReason: ")
		b.WriteString(langparser.QuoteString(failureReason))
	}
	b.WriteString(", detail: ")
	b.Write(detailJSON)
	b.WriteString(")")

	// Internal origin for this one write (memql#5624): auditEvent's create
	// admits a cluster owner or server code, and the person deploying need
	// be neither. Through the store's one stamping site rather than a second
	// one (internal_origin_test.go counts them); the statement is composed
	// above field by field, so the stamp reaches this createAuditEvent and
	// nothing else.
	if err := (&store{engine: a.engine}).writeInternal(ctx, b.String()); err != nil {
		a.logger.Warn("packages: the audit event could not be written",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, deploymentId),
			"action", action, "deployment", deploymentId, "package", packageId,
			"outcome", outcome, "err", err)
	}
}

// audit records one attempt. A missing Auditor is a no-op rather than an
// error: a test drives the state machine without one, and an audit failure
// must never change what the deploy did.
func (d *Deps) audit(ctx context.Context, req DeployRequest, out *DeployOutcome, err error) {
	if d.Auditor == nil {
		return
	}
	ev := DeployAuditEvent{
		PackageId:    req.PackageId,
		DeploymentId: out.DeploymentId,
		Status:       out.Status,
		Deployables:  out.Deployables,
	}
	if out.Report != nil {
		ev.SourceVersion = out.Report.SourceVersion
	}
	if err != nil {
		ev.FailureReason = RefusalCode(err)
		if ev.FailureReason == "" {
			ev.FailureReason = "internal_error"
		}
	}
	d.Auditor.Deploy(ctx, ev)
}

// newRowId mints a canonical row id for a concept.
func newRowId(concept string) string {
	return concept + ":" + id.NewShortId()
}
