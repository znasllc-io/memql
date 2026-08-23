package workbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// workspace_store.go is the Go writer for v1:workbench:workspace rows
// (memql#4354).
//
// Until this file the concept was declared in the DSL and written by nothing:
// the package doc in workspace.go said so, and the consequence was that a
// workspace had no recorded location. That is what made the two-replicas split
// invisible -- `deploy/k8s/base/workbench.yaml` runs two replicas, the agent's
// peer picker was any-fit, so one plan got a directory on each disk and
// neither side was told. A call writes a file; the next call lands on the other
// replica and does not find it, and every layer reports success.
//
// AUTHORIZATION IS THE LOAD-BEARING PART OF THIS FILE.
//
// v1:workbench:workspace declares @rowAuthz(owner="ownerUserId", clusterOwner).
// Two consequences that a reader has to hold at once:
//
//   - The READ gate has no internal-origin bypass. A read with no actor in
//     context returns ZERO ROWS -- not an error. So an unactored read is
//     indistinguishable from "this plan has no workspace", which would make the
//     integration provision a second one on every call. Every read below runs
//     under auth.ContextWithUserActor.
//   - provisionWorkspace stamps ownerUserId from actor.userId (a declared owner
//     field written from caller args fails TestDeclaredOwnerFieldsAreServerStamped),
//     so the WRITE must run under that same actor. The value is the parent
//     plan's requestedBy.
//
// And the trap underneath both: auth.ContextWithUserActor is a NO-OP on a blank
// id. It returns the context unchanged, the write lands with whatever actor was
// already there (frequently none), ownerUserId is stamped "" and the row is
// owned by nobody -- invisible to the user, invisible to the operator, and
// indistinguishable from an absent row on the next read. So every method here
// refuses a blank owner rather than proceeding; see errNoPlanOwner.
//
// This package may NOT use auth.ContextWithInternalOrigin: integrations/workbench
// is deliberately absent from the allowlist in the repo-root
// call_origin_conformance_test.go, and adding it would fail
// TestOnlyAllowlistedPackagesStampInternalOrigin. Consequently none of the
// workbench constructs may be @serverOnly, and none are.

// errNoPlanOwner is the refusal when the parent plan's owner could not be
// resolved. It is an error rather than a skip because the alternative --
// writing under a blank actor -- succeeds at every layer and produces a row
// nobody can see.
var errNoPlanOwner = errors.New("workbench: cannot resolve the parent plan's owner, so the workspace row would be stamped with no owner and be unreadable by anyone")

// workspaceRowStatus values, mirroring the concept's status enum.
const (
	workspaceStatusProvisioned = "provisioned"
	workspaceStatusReleased    = "released"
)

// Release reasons accepted by the releaseWorkspace mutation. Only node_lost is
// written from here; the others belong to the DSL automation and to operators.
const (
	releaseReasonNodeLost = "node_lost"
)

// workspaceRow is the projection of v1:workbench:workspace this integration
// reads back off workspaceForPlan.
type workspaceRow struct {
	Id          string
	PlanId      string
	StorageRoot string
	NodeId      string
	Status      string
	OwnerUserId string
}

// workspaceStore routes the v1:workbench:workspace lifecycle calls through the
// engine. Mirrors integrations/agent/worker's EngineStore: a thin typed wrapper
// whose only job is to build the call and bind the right actor, so the actor
// binding lives in one place instead of at five call sites.
//
// A nil store makes every method a no-op returning no row. That is the
// pre-existing MVP posture (the same one promoteWorkbenchOutput takes) and is a
// different situation from a blank owner: with no engine there is no
// persistence layer at all to be wrong about, whereas a blank owner means the
// layer is there and we are about to write into it incorrectly.
//
// The engine is held as a QUERY FUNCTION rather than as the engine itself.
// *memql.ExecuteResult carries its shape payload on an unexported field with no
// exported setter, so a fake engine outside component/memql cannot return rows
// at all -- and a row store whose read path has no coverage is precisely the
// half of this file that has to be right.
type workspaceStore struct {
	exec func(ctx context.Context, query string) ([]map[string]any, error)
}

// newWorkspaceStore adapts an engine into a store, or returns nil when there is
// no engine to adapt.
func newWorkspaceStore(engine memql.IntegrationEngineAccess) *workspaceStore {
	if engine == nil {
		return nil
	}
	return &workspaceStore{exec: func(ctx context.Context, query string) ([]map[string]any, error) {
		res, err := engine.Execute(ctx, query)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, nil
		}
		return outputPayloadRows(res.OutputPayload()), nil
	}}
}

func (s *workspaceStore) available() bool { return s != nil && s.exec != nil }

// planRow reads the parent plan. The single planById reader in this package:
// the workspace owner and the Library promotion's owner are the same value by
// the same memql#952 rule (payload.requestedBy over the row-intrinsic
// createdBy), and two readers would be two places for that rule to drift.
func (s *workspaceStore) planRow(ctx context.Context, planId string) (map[string]any, error) {
	if !s.available() {
		return nil, nil
	}
	rows, err := s.exec(ctx, fmt.Sprintf(`query planById(planId:%s)`, langparser.QuoteString(planId)))
	if err != nil {
		return nil, fmt.Errorf("workbench: planById: %w", err)
	}
	for _, row := range rows {
		if row != nil {
			return row, nil
		}
	}
	return nil, nil
}

// forPlan returns the LIVE workspace row for a plan, or nil when there is none.
//
// "Live" means status=provisioned. The query filters on planId alone and a plan
// that has survived a node loss carries both the released row and its
// successor, so picking by status here is what keeps the caller from adopting a
// directory that is gone with the node it lived on.
func (s *workspaceStore) forPlan(ctx context.Context, planId, ownerUserId string) (*workspaceRow, error) {
	if !s.available() {
		return nil, nil
	}
	actorCtx, err := s.actorContext(ctx, ownerUserId)
	if err != nil {
		return nil, err
	}
	rows, execErr := s.exec(actorCtx,
		fmt.Sprintf(`query workspaceForPlan(planId:%s)`, langparser.QuoteString(planId)))
	if execErr != nil {
		return nil, fmt.Errorf("workbench: workspaceForPlan: %w", execErr)
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		parsed := workspaceRow{
			Id:          strings.TrimSpace(stringFromRow(row, "id")),
			PlanId:      strings.TrimSpace(stringFromRow(row, "planId")),
			StorageRoot: strings.TrimSpace(stringFromRow(row, "storageRoot")),
			NodeId:      strings.TrimSpace(stringFromRow(row, "nodeId")),
			Status:      strings.TrimSpace(stringFromRow(row, "status")),
			OwnerUserId: strings.TrimSpace(stringFromRow(row, "ownerUserId")),
		}
		if parsed.Id == "" || parsed.Status != workspaceStatusProvisioned {
			continue
		}
		return &parsed, nil
	}
	return nil, nil
}

// provision writes the row for a freshly-created directory.
//
// ownerUserId is NOT an argument to the mutation -- provisionWorkspace stamps
// it from actor.userId, and actorCtx carries exactly that actor. nodeId IS an
// argument, because only the node that just made the directory knows it.
func (s *workspaceStore) provision(ctx context.Context, ownerUserId string, row workspaceRow) error {
	if !s.available() {
		return nil
	}
	actorCtx, err := s.actorContext(ctx, ownerUserId)
	if err != nil {
		return err
	}
	call := fmt.Sprintf(`mutation provisionWorkspace(workspaceId:%s, planId:%s, storageRoot:%s, nodeId:%s)`,
		langparser.QuoteString(row.Id),
		langparser.QuoteString(row.PlanId),
		langparser.QuoteString(row.StorageRoot),
		langparser.QuoteString(row.NodeId))
	if _, execErr := s.exec(actorCtx, call); execErr != nil {
		return fmt.Errorf("workbench: provisionWorkspace: %w", execErr)
	}
	return nil
}

// touch bumps lastUsedAt after a successful dispatch.
func (s *workspaceStore) touch(ctx context.Context, ownerUserId, workspaceId string) error {
	if !s.available() || strings.TrimSpace(workspaceId) == "" {
		return nil
	}
	actorCtx, err := s.actorContext(ctx, ownerUserId)
	if err != nil {
		return err
	}
	call := fmt.Sprintf(`mutation touchWorkspace(workspaceId:%s)`, langparser.QuoteString(workspaceId))
	if _, execErr := s.exec(actorCtx, call); execErr != nil {
		return fmt.Errorf("workbench: touchWorkspace: %w", execErr)
	}
	return nil
}

// release flips a workspace to status=released with the supplied reason.
func (s *workspaceStore) release(ctx context.Context, ownerUserId, workspaceId, reason string) error {
	if !s.available() || strings.TrimSpace(workspaceId) == "" {
		return nil
	}
	actorCtx, err := s.actorContext(ctx, ownerUserId)
	if err != nil {
		return err
	}
	call := fmt.Sprintf(`mutation releaseWorkspace(workspaceId:%s, reason:%s)`,
		langparser.QuoteString(workspaceId), langparser.QuoteString(reason))
	if _, execErr := s.exec(actorCtx, call); execErr != nil {
		return fmt.Errorf("workbench: releaseWorkspace: %w", execErr)
	}
	return nil
}

// actorContext binds the plan owner for one engine call, and refuses a blank
// one. The refusal is the whole safety property of this file -- see the
// errNoPlanOwner comment and the file header.
func (s *workspaceStore) actorContext(ctx context.Context, ownerUserId string) (context.Context, error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return nil, errNoPlanOwner
	}
	return withUserActor(ctx, owner), nil
}

// deriveWorkspaceId derives the row id from (planId, nodeId).
//
// Deterministic in both components, which buys two properties. A workbench
// replica that restarts and re-provisions the same plan's directory lands on
// the same id, so the row is adopted rather than duplicated. A DIFFERENT
// replica taking the plan over lands on a different id, which is required
// rather than incidental: the node-loss path has to release the old row and
// insert a successor, and one id cannot be both released and provisioned.
func deriveWorkspaceId(planId, nodeId string) string {
	h := genOutputIdEngine.MustFromMap(map[string]any{
		"planId": planId,
		"nodeId": nodeId,
	})
	return "wbws-" + string(h)[:16]
}

// selfNodeId reports THIS node's id, by the same derivation
// component/node.NewIdentity uses: MEMQL_NODE_ID, falling back to the
// hostname.
//
// Repeating the derivation rather than reading a shared accessor is not a
// preference -- there is no accessor to read (component/node/identity.go,
// component/config/config.go and component/campaigns/config.go each read the
// env var directly), and this package must not import the node identity
// constructor, which would build a whole Identity out of a dozen other
// variables as a side effect of asking one question.
//
// It has to AGREE with that derivation, though, and the reason is exact: the
// value stamped on the row is compared against PeerInfo.node_id, which is
// Identity.ID. Two spellings of "who am I" would mean the affinity lookup can
// never match, and the symptom would be a plan re-provisioning its workspace on
// every single call while every row looked correct.
func selfNodeId() string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil {
		return strings.TrimSpace(host)
	}
	return ""
}
