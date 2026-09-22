package memql

// The `fleetModelPull` ACT (epic memql#5103, design D3 and D5).
//
// ===========================================================================
// WHY THIS IS AN ACT THAT RETURNS AT ONCE, AND A ROW YOU WATCH
// ===========================================================================
// Its siblings in this package -- fleetModels, inferenceStatus, dataOrigins --
// are READS that produce virtual rows. This is the first fleet builtin that
// makes something happen on somebody's hardware, and it is shaped by two facts
// that no read has to deal with.
//
// It takes minutes to hours. A 70B model is tens of gigabytes and the download
// is bounded by a domestic connection, so blocking the caller's gRPC stream on
// it is not an option: the act starts the pull and returns, and the
// v1:worker:modelPull row is how anyone watches. That row is also what makes
// the pull survive the person closing the tab, and what answers "why is this
// model here" long after.
//
// It cannot run on the node that serves it. Everything that can reach a
// machine's stream lives under the `agent` build tag; the bff serving this call
// has no worker registry at all. So the act's whole job is to DECIDE and
// RECORD, and an agent replica picks the row up. The claim is
// `targetNodeId`: the replica whose own MEMQL_NODE_ID it names is the one that
// acts, which is deterministic and needs no lock, because exactly one replica
// holds a given machine's stream.
//
// ===========================================================================
// EVERY REFUSAL HAPPENS BEFORE A ROW IS WRITTEN
// ===========================================================================
// A pull names ONE machine, because a person pressed Pull on that machine's
// page; there is no routing here and no second machine to fall through to.
// That makes an accepted-then-failed pull strictly worse than a refusal: it is
// minutes of a spinner ending in a cause that was knowable at the moment of the
// press. So ownership, revocation and connectedness are all decided here, and
// each has its OWN code, because the remedies differ -- "this machine was
// revoked" and "wake it up" are different things for a person to go and do.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// ModelPullResultConcept is the canonical id of the act's reply row.
const ModelPullResultConcept = "v1:worker:modelPullResult"

// ModelPullConcept is the canonical id of the persisted pull record.
const ModelPullConcept = "v1:worker:modelPull"

// modelPullMachine is the slice of a v1:worker:registration row this act
// decides on. A struct rather than the raw row so the decision is a function
// of values and is testable without an engine, a database or a cluster.
type modelPullMachine struct {
	RegistrationId string
	OwnerUserId    string
	// IdentityId is the worker credential this registration is bound to (epic
	// memql#5327, design D2). Only fleetRevokeMachine reads it, and it lives
	// here rather than behind a lookup of its own because this struct is the
	// answer to "is this machine the caller's", which is the same question
	// that has to be settled before anybody may touch its credential.
	IdentityId      string
	DisplayName     string
	Name            string
	ConnectedNodeId string
	LastSeenAt      string
	RevokedAt       string
}

// modelPullPlan is what an accepted act will write.
type modelPullPlan struct {
	PullId       string
	WorkerId     string
	Model        string
	TargetNodeId string
	RequestedAt  string
}

// modelPullRefusal is a refusal carrying the code a surface acts on.
type modelPullRefusal struct {
	Code    string
	Message string
}

func (r modelPullRefusal) Error() string { return r.Message }

// modelPullErrorCode reads the code off a refusal, or "" for any other error.
func modelPullErrorCode(err error) string {
	if r, ok := err.(modelPullRefusal); ok {
		return r.Code
	}
	return ""
}

// planModelPull decides whether this caller may pull this model onto this
// machine, and what the row would say.
//
// PURE. Every branch is a function of the registration's own fields and the
// clock, which is what lets the refusals be asserted without standing up a
// fleet -- and the refusals are the part of this feature a person actually
// meets.
func planModelPull(m modelPullMachine, actingUserId, model string, now time.Time) (modelPullPlan, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return modelPullPlan{}, modelPullRefusal{
			Code:    "model_required",
			Message: "fleetModelPull needs a model id, in the runtime's own vocabulary (llama3.1:8b, hf.co/owner/repo:Q4_K_M)",
		}
	}

	// Ownership first, and it answers with the SAME sentence a machine that
	// does not exist gets -- a caller must not be able to tell somebody else's
	// registration id from a made-up one.
	acting := strings.TrimSpace(actingUserId)
	if acting == "" || m.RegistrationId == "" || !strings.EqualFold(strings.TrimSpace(m.OwnerUserId), acting) {
		return modelPullPlan{}, modelPullRefusal{
			Code:    "not_your_machine",
			Message: "no machine of yours has that registration id",
		}
	}

	if strings.TrimSpace(m.RevokedAt) != "" {
		return modelPullPlan{}, modelPullRefusal{
			Code:    "machine_revoked",
			Message: machineLabel(m) + " was revoked, so its worker token no longer works. Pair it again to use it.",
		}
	}

	// ===================================================================
	// `connectedNodeId` IS THE LIVENESS TEST HERE, AND NOT lastSeenAt
	// ===================================================================
	// It is blanked the moment a stream closes (EngineStore.ClearConnectedNode)
	// and `lastSeenAt` is deliberately NOT advanced on the way out, so a
	// disconnected machine has an empty node id immediately while its
	// heartbeat stays as stale as it truly is. For a PULL that is exactly the
	// right question: a download runs on the replica holding the stream, so
	// "which replica holds it" is the fact that decides, and a machine with a
	// fresh beat and no stream is one a pull cannot reach.
	//
	// It is also why this does not re-derive the online window from
	// lastSeenAt. There are exactly TWO implementations of that rule --
	// component/worker.IsOnline and clients/os/src/apps/fleet/online.ts, held
	// in step by TestFleetOnlineWindowMatchesTheClients -- and the count is
	// load-bearing. A third here could not even import the first
	// (component/worker reaches component/identity, which reaches this
	// package), so it would be a COPY of a constant, drifting silently.
	//
	// The residual case is a replica killed abruptly, which leaves the field
	// stamped at a node that is gone. That pull is picked up by nobody and
	// failed by workerModelPullStaleSweep, which is the one place in this
	// design where a person waits before learning -- and it is the case no
	// check performed here could have caught.
	if strings.TrimSpace(m.ConnectedNodeId) == "" {
		return modelPullPlan{}, modelPullRefusal{
			Code:    "machine_offline",
			Message: machineLabel(m) + " is not connected to the cluster right now, so there is no machine to pull to. Wake it, or check its cockpit is running, and try again.",
		}
	}

	return modelPullPlan{
		PullId:       id.NewShortId(),
		WorkerId:     m.RegistrationId,
		Model:        model,
		TargetNodeId: strings.TrimSpace(m.ConnectedNodeId),
		RequestedAt:  now.UTC().Format(time.RFC3339),
	}, nil
}

// machineLabel is what a person calls this machine: the name they gave it,
// falling back to what the cockpit reported.
func machineLabel(m modelPullMachine) string {
	if n := strings.TrimSpace(m.DisplayName); n != "" {
		return n
	}
	if n := strings.TrimSpace(m.Name); n != "" {
		return n
	}
	return m.RegistrationId
}

// renderCreateModelPullCall builds the mutation text that opens the row.
//
// `langparser.QuoteString`, NOT `strconv.Quote`, for the reason
// renderProviderConfigCall records at length: Go's quoting emits `\x00`, `\a`,
// `\v` and `\x7f`, all four of which the MemQL lexer REJECTS, so a value
// carrying one renders into a statement that fails to PARSE. It matters here
// because the model id is OPERATOR-TYPED -- pasted from a model card or a
// terminal -- which makes a stray control byte an ordinary event rather than an
// exotic one.
func renderCreateModelPullCall(p modelPullPlan) string {
	args := []string{
		"model: " + langparser.QuoteString(p.Model),
		"pullId: " + langparser.QuoteString(p.PullId),
		"requestedAt: " + langparser.QuoteString(p.RequestedAt),
		"targetNodeId: " + langparser.QuoteString(p.TargetNodeId),
		"workerId: " + langparser.QuoteString(p.WorkerId),
	}
	return "createModelPull(" + strings.Join(args, ", ") + ")"
}

// evaluateFleetModelPullExpression serves the `fleetModelPull` builtin.
func (e *MemQLEngine) evaluateFleetModelPullExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	acting := strings.TrimSpace(actingUserFromContext(ctx))
	if acting == "" {
		return nil, modelPullRefusal{
			Code:    "not_your_machine",
			Message: "no machine of yours has that registration id",
		}
	}

	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	machine, err := e.modelPullMachineFor(ctx, registrationId)
	if err != nil {
		return nil, err
	}

	plan, err := planModelPull(machine, acting, stringArg(args, "model"), time.Now())
	if err != nil {
		return nil, err
	}

	// The row is written under the CALLER'S actor, so `ownerUserId` is stamped
	// from it by the mutation and the row belongs to the person who pressed
	// the button. Internal origin is stamped because createModelPull is
	// @serverOnly: the caller may own the row and still must not be able to
	// name the replica that will act on it.
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), renderCreateModelPullCall(plan)); err != nil {
		return nil, fmt.Errorf("fleetModelPull: open the pull record: %w", err)
	}

	return singleVirtualRow(ModelPullResultConcept, plan.PullId, map[string]any{
		"pullId":       plan.PullId,
		"workerId":     plan.WorkerId,
		"model":        plan.Model,
		"status":       "requested",
		"targetNodeId": plan.TargetNodeId,
		"requestedAt":  plan.RequestedAt,
		"machine":      machineLabel(machine),
	})
}

// modelPullMachineFor resolves one of the caller's registrations.
//
// It reads through `workersForUser`, the AUTHORIZED path, rather than
// selecting the row directly: the concept is owner-tiered, so a read under the
// caller's actor cannot return somebody else's machine and this function does
// not have to be trusted to check. A registration id that is not the caller's
// simply is not in the answer, which is why the not-found and not-yours
// refusals are the same sentence.
func (e *MemQLEngine) modelPullMachineFor(ctx context.Context, registrationId string) (modelPullMachine, error) {
	notYours := modelPullRefusal{
		Code:    "not_your_machine",
		Message: "no machine of yours has that registration id",
	}
	if registrationId == "" {
		return modelPullMachine{}, notYours
	}
	acting := strings.TrimSpace(actingUserFromContext(ctx))
	call := "workersForUser(ownerUserId: " + langparser.QuoteString(acting) + ")"
	res, err := e.Execute(ctx, call)
	if err != nil {
		return modelPullMachine{}, fmt.Errorf("fleetModelPull: read your machines: %w", err)
	}

	// workersForUser is SHAPED, so the bundle is nil and the rows arrive on
	// the output payload -- the same reading extractRowIds does for every
	// other engine-internal consumer of a shaped query.
	for _, row := range modelPullRows(res.OutputPayload()) {
		rowId := mapString(row, "id")
		if rowId != registrationId && trimConceptPrefix(rowId) != registrationId {
			continue
		}
		return modelPullMachine{
			RegistrationId: registrationId,
			OwnerUserId:    mapString(row, "ownerUserId"),
			// The credential bound to this registration (epic memql#5327,
			// design D2). It is on the shape already -- workerRegistrationFull
			// projects identityId -- and it is read HERE rather than by a
			// second lookup because this is the one resolution that has
			// already proven the machine belongs to the caller.
			IdentityId:      mapString(row, "identityId"),
			DisplayName:     mapString(row, "displayName"),
			Name:            mapString(row, "name"),
			ConnectedNodeId: mapString(row, "connectedNodeId"),
			LastSeenAt:      mapString(row, "lastSeenAt"),
			RevokedAt:       mapString(row, "revokedAt"),
		}, nil
	}
	return modelPullMachine{}, notYours
}

// modelPullRows normalises a shaped query's output into row maps. The two
// shapes a shaped result arrives in -- a bare list, or a map with `nodes` --
// are the ones extractRowIds already handles; this reads the whole row rather
// than only its id.
func modelPullRows(payload any) []map[string]any {
	var raw []any
	switch v := payload.(type) {
	case []any:
		raw = v
	case map[string]any:
		if nodes, ok := v["nodes"].([]any); ok {
			raw = nodes
		} else {
			raw = []any{v}
		}
	default:
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// trimConceptPrefix reduces a canonical id to its short form. The engine
// bare-ifies ids on egress, so a caller's argument is bare while a row read
// back may be canonical; comparing both spellings is cheaper than deciding
// which layer this answer came through.
func trimConceptPrefix(nodeId string) string {
	if at := strings.LastIndex(nodeId, ":"); at >= 0 {
		return nodeId[at+1:]
	}
	return nodeId
}
