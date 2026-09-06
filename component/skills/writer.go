package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// writer.go -- the engine-backed writes over the capability graph.
//
// ===========================================================================
// WHY THE WRITES LIVE HERE AND NOT AT THEIR CALL SITES
// ===========================================================================
// `setSkillScripts`, `createSkillEdge` and `commitSkillEdge` are all
// `@serverOnly`, and origin defaults to CLIENT -- so a caller that does not
// stamp internal origin has each write refused with a WARN in a log and
// nothing else. The stamp is allowlisted per PACKAGE
// (component/auth/call_origin.go), and that list's own standard is that an
// entry should be "small, exists for one operation family, and every call
// site in it is downstream of one gate".
//
// The two callers are not that. `integrations/planner` is a large package
// full of request-derived paths, and `integrations/skills` serves the
// caller-scoped Library reads runScript makes. An entry for either would put
// the stamp within reach of code that has nothing to do with these three
// writes. This package is the operation family, and `selection.go` beside it
// stays pure -- it is a different file with no engine in it, which is what
// lets the selection rules be tested with no engine at all.
//
// THE GATE every call site here is downstream of: the writes are refused
// without a skill id, and both callers reach them only after their own gate
// -- the mint's three gates for an edge, and capture's read of the bytes off
// a surface the caller already reached for a script list.

// Executor is the engine seam. `any` on the result for the reason
// component/workjournal's is: nothing here reads a result, so taking the
// engine's concrete type would buy a dependency and nothing else.
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Writer writes the capability graph.
type Writer struct{ engine Executor }

// NewWriter builds one. A nil engine yields a nil writer, whose methods all
// refuse by name rather than panicking.
func NewWriter(engine Executor) *Writer {
	if engine == nil {
		return nil
	}
	return &Writer{engine: engine}
}

// SetScripts rewrites a skill's `scripts[]`.
//
// It takes the ENCODED list rather than a []Script so this package does not
// have to own the script shape as well as the graph -- the caller that
// merges the list is the caller that knows what an entry is.
func (w *Writer) SetScripts(ctx context.Context, skillID string, scriptsJSON []byte) error {
	if w == nil || w.engine == nil {
		return fmt.Errorf("skills: no engine is wired, so a skill's scripts cannot be recorded")
	}
	if strings.TrimSpace(skillID) == "" {
		return fmt.Errorf("skills: skillId is required")
	}
	if len(scriptsJSON) == 0 {
		scriptsJSON = []byte("[]")
	}
	_, err := w.engine.Execute(
		auth.ContextWithInternalOrigin(ctx),
		fmt.Sprintf("mutation setSkillScripts(skillId: %s, scripts: %s)",
			langparser.QuoteString(skillID), string(scriptsJSON)),
	)
	return err
}

// EdgeWrite is one edge to record.
type EdgeWrite struct {
	EdgeID   string
	From     string
	To       string
	Type     EdgeType
	Evidence []Evidence
	// ProposedBy is `system` or `user`.
	ProposedBy string
}

// Propose writes edges at `proposed`.
func (w *Writer) Propose(ctx context.Context, edges []EdgeWrite) error {
	return w.write(ctx, edges, false)
}

// Commit writes edges and promotes them in the same pass -- what a
// successful run does with what its compile proposed, and what a bundle mint
// does with its declared composition.
func (w *Writer) Commit(ctx context.Context, edges []EdgeWrite) error {
	return w.write(ctx, edges, true)
}

func (w *Writer) write(ctx context.Context, edges []EdgeWrite, commit bool) error {
	if w == nil || w.engine == nil {
		return fmt.Errorf("skills: no engine is wired, so a skill edge cannot be recorded")
	}
	for _, edge := range edges {
		if edge.EdgeID == "" || edge.From == "" || edge.To == "" || edge.From == edge.To {
			continue
		}
		evidence := edge.Evidence
		if evidence == nil {
			evidence = []Evidence{}
		}
		create, err := json.Marshal(map[string]any{
			"edgeId":      edge.EdgeID,
			"fromSkillId": edge.From,
			"toSkillId":   edge.To,
			"edgeType":    string(edge.Type),
			"evidence":    evidence,
			"proposedBy":  firstNonBlank(edge.ProposedBy, "system"),
		})
		if err != nil {
			return err
		}
		if _, err := w.engine.Execute(auth.ContextWithInternalOrigin(ctx),
			fmt.Sprintf("mutation createSkillEdge(%s)", string(create))); err != nil {
			return err
		}
		if !commit {
			continue
		}
		promote, err := json.Marshal(map[string]any{"edgeId": edge.EdgeID, "evidence": evidence})
		if err != nil {
			return err
		}
		if _, err := w.engine.Execute(auth.ContextWithInternalOrigin(ctx),
			fmt.Sprintf("mutation commitSkillEdge(%s)", string(promote))); err != nil {
			return err
		}
	}
	return nil
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
