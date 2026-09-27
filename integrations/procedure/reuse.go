package procedure

// reuse.go -- a person's reuse label on one of their constructs (epic
// memql#5414, #5418; design D24).
//
// The evidence decides a construct's label (reuse_sweep.go) and a person may
// override it. The override is a VERSION: every call writes the next one --
// the server's count, never the caller's -- and never rewrites an earlier
// one, and the evidence keeps counting underneath it, so a surface can show
// both. `evidence` writes an override whose label is empty, which hands the
// label back to the sweep and is itself a version like any other.
//
// CALLER-SCOPED IN THE HANDLER. @serverOnly is refused on a builtin at parse,
// so the gate lives here: the construct is read through authoringConstructById
// under the CALLER's own actor, which filters on the construct's owner -- a
// caller who does not own it reads nothing and is refused
// construct_not_found, with nothing written. The write goes through the one
// internal-origin stamp (store.go writeInternal), because
// recordConstructReuseOverride is @serverOnly: the version it records is the
// server's count.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

// reuseLabelEvidence is the label that clears a person's override.
const reuseLabelEvidence = "evidence"

func (i *Integration) handleSetReuse(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	constructId := strings.TrimSpace(argString(args, "constructId"))
	if constructId == "" {
		return nil, fmt.Errorf("procedure.setReuse: constructId is required")
	}
	requested := strings.TrimSpace(argString(args, "label"))
	label := work.ParseReuseLabel(requested)
	if label == "" && requested != reuseLabelEvidence {
		return nil, fmt.Errorf("procedure.setReuse: label %q is not reusable, goalSpecific, accountSpecific or evidence", requested)
	}
	ac, present := auth.AccessFromContext(ctx)
	if !present || ac == nil || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("procedure.setReuse: an authenticated owner is required")
	}
	rows, err := i.store.query(ctx, "query "+call("authoringConstructById", map[string]any{"constructId": constructId}))
	if err != nil {
		return nil, fmt.Errorf("procedure.setReuse: %w", err)
	}
	// Belt and braces over the owned read, for learnFromRun's reason: the
	// read admits only the owner today, and an override is a claim about THE
	// OWNER'S catalog that nobody else may make.
	if len(rows) == 0 || !sameUser(str(rows[0], "ownerUserId"), ac.UserId) {
		return nil, fmt.Errorf("construct_not_found: procedure.setReuse: construct %s is not one of your constructs", constructId)
	}
	row := rows[0]
	override := map[string]any{
		"label":   string(label),
		"by":      ac.UserId,
		"at":      i.clock().UTC().Format(timeLayout),
		"version": intOf(obj(row, "reuseOverride"), "version") + 1,
	}
	id := firstNonEmpty(str(row, "id"), constructId)
	if err := i.store.writeInternal(ctx, "mutation "+call("recordConstructReuseOverride",
		map[string]any{"constructId": id, "reuseOverride": override})); err != nil {
		return nil, fmt.Errorf("procedure.setReuse: record the override: %w", err)
	}
	effective := work.EffectiveReuse(work.ParseReuseLabel(str(row, "reuse")), label)
	return i.reply(map[string]any{
		"constructId": id,
		"reuse":       string(effective),
		"override":    override,
	}), nil
}
