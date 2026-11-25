package logstore

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// accessHoldComponent and accessHoldMessage are what an administrator
// searches Logs for to see why a person has no Ask. The message says what
// MemQL OS does on this answer since 2026-11-25 (design record
// 2026-11-25-who-gets-into-memql-os): it keeps nobody out, and shows the
// person no Ask. "Hold" in the names here is from the rule before it.
const (
	accessHoldComponent = "desktopAccess"
	accessHoldMessage   = "No model access is set up for this person: MemQL OS shows them no Ask"
)

// RecordAccessHold is the memql.SetAccessHoldRecorder writer: it stores the
// hold line on this node's sink and returns only once the insert answered, or
// failed at InsertNow's per-insert limit (5 s) when the database hangs.
//
// It does not go through the queue because the reference is returned to the
// caller the moment this returns, and a reference whose line was dropped for a
// full queue or the rate bucket would send an administrator to a search that
// finds nothing (desktop-access design, section 3). A node with no log store
// returns ErrNoStore, which desktopAccess answers as held with NO reference:
// no reference is better than one that names a line nobody can find. No
// MemQL OS surface shows the reference since 2026-11-25; the rule is kept for
// any caller that does.
//
// The person and any lender in the reasons reach here already bare (the
// engine spells them once, in component/memql/desktop_access.go); the line's
// Subject, the Stamp's UserId and those reasons therefore agree.
func RecordAccessHold(ctx context.Context, line memql.AccessHoldLine) error {
	return recordAccessHoldOn(ctx, Current(), line)
}

func recordAccessHoldOn(ctx context.Context, s *Sink, line memql.AccessHoldLine) error {
	if s == nil {
		return ErrNoStore
	}
	reasons := append([]string(nil), line.Reasons...)
	return s.InsertNow(ctx, logger.Line{
		Level:          slog.LevelWarn,
		Component:      accessHoldComponent,
		Message:        accessHoldMessage,
		Subject:        line.UserId,
		SubjectConcept: "v1:identity:user",
		Attributes:     map[string]any{"reference": line.Reference, "reasons": reasons},
	}, Stamp{NodeType: s.NodeType(), Node: s.Node(), UserId: line.UserId})
}
