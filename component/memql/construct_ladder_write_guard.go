package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// constructLadderFields are the fields of a v1:authoring:construct that only
// the certification ladder writes (epic memql#5408): the rung, the procedure a
// replay executes and the preconditions it starts on, the version a promotion
// approval pins, and the evidence the state machine keeps between events.
// Mirrors dsl/authoring/concepts.memql's certification-ladder block; the
// writers are recordProcedure and recordConstructLadder.
var constructLadderFields = []string{
	"ladder",
	"preconditions",
	"procedure",
	"procedureHash",
	"shadowMatches",
	"canaryMatches",
	"distinctBindings",
	"failures",
	"insufficient",
	"promotionApprovalId",
	"lastReplayAt",
	"ladderReason",
	"ladderChangedAt",
}

// constructLearnedFields are the construct's older catalog evidence (epic
// memql#4966), which become the LADDER'S on a learned procedure: goalSignature
// is what compile serves a goal from it by, and reliability, reinforceCount and
// lastReinforced rank it among the procedures that answer one. The writers are
// recordConstructGoalSignature and recordConstructReliability.
//
// Only on a row whose `ladder` is set. An authored construct is not on the
// ladder, and what those fields mean there is not this guard's to judge.
var constructLearnedFields = []string{
	"goalSignature",
	"reliability",
	"reinforceCount",
	"lastReinforced",
}

// constructLearnedSource is the MemQL source a person reads on a learned
// procedure's page and approves: recordProcedure writes it beside the
// procedure a replay executes, and procedureHash digests both. On an authored
// construct it is the construct itself, which its owner edits; on a learned
// one an owner edit would show a person one thing, approved under a hash that
// never changed, while the ladder served another.
const constructLearnedSource = "source"

// validateConstructLadderServerOnly refuses a write to a v1:authoring:construct
// that CHANGES a field the certification ladder owns without internal origin
// (epic memql#5408, issue #5409).
//
// The ladder is the one place a learned procedure earns being served: a
// trusted procedure answers a goal with no model and no app, so the rung, the
// procedure it runs and the version a person approved are the whole of the
// trust. Its writers are @serverOnly mutations, and integrations/procedure
// calls them under the owner's borrowed actor with internal origin stamped. But
// a construct is @rowAuthz(owner="ownerUserId"): the row-authz write guard
// admits its owner, and the raw insert(...) literal -- like the update() body
// of a mutation the owner authored themselves -- never consults a mutation's
// @serverOnly (the memql#5623 pattern). Probed against a real database, an
// ordinary writer put `ladder: "trusted"`, a procedure, its hash and a goal
// signature onto their own construct.
//
// FIELD-LEVEL AND KEYED ON A CHANGE, not on the concept: an owner renaming or
// retiring their construct from the OS writes the same row, and the read-merge
// carries every ladder field through that write unchanged. So a field is
// judged only when the final row differs from the stored one (prior is nil on
// a create, where a field that is set is a change). Absent, JSON null and the
// empty string are one unset value, as they are in the language's `==`.
//
// Keyed on ORIGIN, like validateGithubConnectStateServerOnly: no caller -- the
// owner, a cluster owner and a system actor included -- has a reason to move
// the ladder any other way than through integrations/procedure.
func validateConstructLadderServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	for _, field := range constructLadderFields {
		if constructFieldChanged(prior, final, field) {
			return errConstructLadderWrite(field,
				"where a learned procedure stands on its certification ladder, what it runs and the version a person approved")
		}
	}
	if !constructIsLearned(prior) && !constructIsLearned(final) {
		return nil
	}
	for _, field := range constructLearnedFields {
		if constructFieldChanged(prior, final, field) {
			return errConstructLadderWrite(field,
				"on a learned procedure, the signature compile serves a goal from it by and the reliability that ranks it")
		}
	}
	if constructFieldChanged(prior, final, constructLearnedSource) {
		return errConstructLadderWrite(constructLearnedSource,
			"on a learned procedure, the source a person reads and approves beside the procedure a replay runs")
	}
	return nil
}

func errConstructLadderWrite(field, what string) error {
	return fmt.Errorf(
		"%s: write to `%s` refused -- it is %s, and only integrations/procedure writes it, "+
			"through recordProcedure, recordConstructLadder, recordConstructGoalSignature and "+
			"recordConstructReliability under internal origin. A raw insert() or update() never "+
			"consults their @serverOnly, and a trusted procedure serves a goal with no model and no "+
			"app, so a write from anywhere else would let a person promote code nobody reviewed. "+
			"See component/memql/construct_ladder_write_guard.go and epic memql#5408.",
		memorynodes.ConceptAuthoringConstruct, field, what,
	)
}

// constructIsLearned reports whether a construct row is on the ladder.
func constructIsLearned(row map[string]any) bool {
	s, _ := row["ladder"].(string)
	return strings.TrimSpace(s) != ""
}

// constructFieldChanged reports whether field differs between the stored row
// (nil on a create) and the row about to be written.
func constructFieldChanged(prior, final map[string]any, field string) bool {
	before, after := prior[field], final[field]
	beforeUnset, afterUnset := constructFieldUnset(before), constructFieldUnset(after)
	if beforeUnset || afterUnset {
		return beforeUnset != afterUnset
	}
	return !constructFieldEqual(before, after)
}

// constructFieldUnset is the language's one unset value: absent, JSON null,
// or the empty string.
func constructFieldUnset(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// constructFieldEqual compares two payload values by their canonical JSON, so
// a number decoded as float64 on one side and written as an int by Go on the
// other is one value, and a nested object compares by content rather than by
// key order. A value that will not encode is never equal to anything, which
// refuses rather than admits.
func constructFieldEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ja, jb)
}
