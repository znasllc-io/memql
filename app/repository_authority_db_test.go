package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// Separate engines share only Postgres: these checks must not borrow the
// originating replica's cache, actor, or in-memory approval state.
func TestRepositoryAuthorityEngineBoundary(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	a := &App{engine: reader, Logger: reader.Logger}
	owner, outsider := "authority-owner-"+id.NewShortId(), "authority-outsider-"+id.NewShortId()
	ownerCtx := displayNameActor(owner, auth.RoleDeveloper)
	internal := auth.ContextWithInternalOrigin(ownerCtx)
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM "MemoryNodes" WHERE payload->>'ownerUserId' IN (?, ?) OR id = ?`, owner, "v1:identity:user:"+owner, "v1:identity:user:"+owner)
		require.NoError(t, err)
	})
	t.Run("SourceVisibilityAndTrustedBinding", func(t *testing.T) {
		account, source, credential := id.NewShortId(), id.NewShortId(), id.NewShortId()
		seed := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "authority-test"))
		templateMutation(t, writer, seed, "createClientAccount", map[string]any{"accountId": account, "name": "Authority test"})
		templateMutation(t, writer, ownerCtx, "createPackage", map[string]any{"accountId": account, "packageId": source, "name": "Engine", "sourceKind": "repo", "repoUrl": "https://github.com/Example/Engine.git", "credentialId": credential})
		t.Cleanup(func() {
			_, err := db.Exec(`DELETE FROM "MemoryNodes" WHERE id = ?`, "v1:contacts:clientAccount:"+account)
			require.NoError(t, err)
		})
		row, err := a.repositoryAuthoritySource(ownerCtx, source, false)
		require.NoError(t, err)
		require.Equal(t, repositoryauthority.Source{ID: source, OwnerUserID: owner, CredentialID: credential, Repository: "example/engine"}, row)
		stranger := displayNameActor(outsider, auth.RoleWriter)
		_, err = a.repositoryAuthoritySource(stranger, source, false)
		require.Error(t, err, "an unrelated user's visible-source path must not borrow the source owner")
		row, err = a.repositoryAuthoritySource(stranger, source, true)
		require.NoError(t, err, "only the native installed-policy binding uses this trusted read")
		require.Equal(t, owner, row.OwnerUserID)
		templateMutation(t, writer, ownerCtx, "setPackageSourceRemoved", map[string]any{"packageId": source, "removed": true})
		_, err = a.repositoryAuthoritySource(stranger, source, true)
		require.Error(t, err, "a cached trusted binding must not revive a removed source")
	})
	t.Run("MaintainerIdentityIsLoadedNotInherited", func(t *testing.T) {
		templateMutation(t, writer, internal, "identity.createUserOnFirstLogin", map[string]any{"userId": owner, "displayName": "Repository maintainer", "primaryEmail": owner + "@example.test", "role": "developer", "internal": false})
		incoming := displayNameActor(outsider, auth.RoleOwner)
		bound, err := a.repositoryMaintainerContext(incoming, owner)
		require.NoError(t, err)
		access, ok := auth.AccessFromContext(bound)
		require.True(t, ok)
		require.Equal(t, owner, memql.BareShortId(access.UserId))
		require.Equal(t, auth.RoleDeveloper, access.Role, "an incoming owner's role cannot become upstream authority")
		_, ok = auth.ForwardedAuthorityFromContext(bound)
		require.True(t, ok, "restored identity must carry explicit authority across a hop")
		templateMutation(t, writer, internal, "identity.updateUser", map[string]any{"userId": owner, "payload": map[string]any{"role": "reader", "revocationEpoch": 7}})
		bound, err = a.repositoryMaintainerContext(incoming, owner)
		require.NoError(t, err)
		access, _ = auth.AccessFromContext(bound)
		require.Equal(t, auth.RoleReader, access.Role, "another replica must observe the current identity")
		_, err = a.repositoryMaintainerContext(incoming, "absent-"+id.NewShortId())
		require.Error(t, err)
	})
	t.Run("ApprovalDecisionReadsExactOwnerAndSubjectOnAnotherReplica", func(t *testing.T) {
		for _, tc := range []struct {
			name, decision, expected    string
			corruptHash, foreignDecider bool
		}{
			{name: "approve", decision: "approved", expected: "approved"},
			{name: "decline", decision: "rejected", expected: "declined"},
			{name: "changed subject", decision: "approved", corruptHash: true},
			{name: "other decider", decision: "approved", foreignDecider: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				goal, run, approval, request := id.NewShortId(), id.NewShortId(), id.NewShortId(), id.NewShortId()
				now := time.Now().UTC().Format(time.RFC3339Nano)
				subject := map[string]any{"reviewType": repositoryauthority.ReviewType, "requestId": request, "fingerprint": "immutable-request", "operation": repositoryauthority.OperationPropose}
				hash := workstate.ArtifactHash(subject)
				if tc.corruptHash {
					hash = "different-subject"
				}
				templateMutation(t, writer, internal, "createWorkGoal", map[string]any{"goalId": goal, "statement": "Review bounded repository change", "origin": "user"})
				templateMutation(t, writer, internal, "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "boundedChange", "templateFingerprint": "fixed-recipe", "status": "waiting", "startedAt": now})
				templateMutation(t, writer, internal, "createWorkApproval", map[string]any{"approvalId": approval, "runId": run, "kind": workstate.ApprovalKindPlanReview, "subject": subject, "artifactHash": hash, "requestedAt": now})
				if !tc.corruptHash {
					pending, err := a.repositoryAuthorityDecision(ownerCtx, owner, approval)
					require.NoError(t, err)
					require.Equal(t, "pending", pending.State)
				}
				decider := owner
				if tc.foreignDecider {
					decider = outsider
				}
				templateMutation(t, writer, internal, "decideWorkApproval", map[string]any{"approvalId": approval, "decision": tc.decision, "decidedBy": decider, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"reason": "Recorded maintainer response"}})
				decision, err := a.repositoryAuthorityDecision(ownerCtx, owner, approval)
				if tc.corruptHash || tc.foreignDecider {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				require.Equal(t, tc.expected, decision.State)
				require.Equal(t, "Recorded maintainer response", decision.Reason)
				require.Equal(t, request, decision.RequestID)
				require.Equal(t, "immutable-request", decision.Fingerprint)
				require.Equal(t, goal, decision.GoalID)
				require.Equal(t, run, decision.RunID)
				_, err = a.repositoryAuthorityDecision(ownerCtx, outsider, approval)
				require.Error(t, err, "a different owner cannot reuse this approval")
			})
		}
	})
	t.Run("ExecutionObservesCancellationAndGoalClosureAcrossReplicas", func(t *testing.T) {
		for _, tc := range []string{"cancel requested", "completed", "closed goal", "wrong owner", "wrong goal"} {
			t.Run(tc, func(t *testing.T) {
				goal, run := id.NewShortId(), id.NewShortId()
				templateMutation(t, writer, internal, "createWorkGoal", map[string]any{"goalId": goal, "statement": "Bound repository execution", "origin": "user"})
				templateMutation(t, writer, internal, "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "boundedChange", "templateFingerprint": "recipe", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				require.NoError(t, a.repositoryAuthorityExecutionActive(ownerCtx, owner, goal, run))
				readOwner, readGoal := owner, goal
				switch tc {
				case "cancel requested":
					templateMutation(t, writer, internal, "updateWorkRun", map[string]any{"runId": run, "cancelRequested": true})
				case "completed":
					templateMutation(t, writer, internal, "updateWorkRun", map[string]any{"runId": run, "status": "succeeded"})
				case "closed goal":
					templateMutation(t, writer, internal, "updateWorkGoal", map[string]any{"goalId": goal, "status": "closed"})
				case "wrong owner":
					readOwner = outsider
				case "wrong goal":
					readGoal = id.NewShortId()
				}
				require.Error(t, a.repositoryAuthorityExecutionActive(ownerCtx, readOwner, readGoal, run), "a stale live context must not override the persisted lifecycle")
			})
		}
	})
	t.Run("ActualGoalAdmissionIsDurableAndWaitsBeforeExecution", func(t *testing.T) {
		templateMutation(t, writer, internal, "identity.updateUser", map[string]any{"userId": owner, "payload": map[string]any{"role": "developer"}})
		gate, err := database.NewGatePool(dbtest.DSN(), 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = gate.Close() })
		for _, e := range []*memql.MemQLEngine{writer, reader} {
			w := workspine.New(e, e.Logger, func() *bun.DB { return db })
			w.SetGateDB(func() *sql.DB { return gate })
			require.NoError(t, e.RegisterIntegration(w))
		}
		first := &App{engine: writer, Logger: writer.Logger}
		for _, immediate := range []bool{false, true} {
			key := id.NewShortId()
			r := repositoryauthority.GoalRequest{Key: key, OwnerUserID: owner, Template: "runPipelineStages", Statement: "Approved bounded source change", Input: map[string]any{"requestId": key}, Subject: map[string]any{"reviewType": repositoryauthority.ReviewType, "requestId": key, "fingerprint": "exact"}, Immediate: immediate}
			bound := false
			r.BeforeRun = func(_ context.Context, goal, run string) error { bound = goal != "" && run != ""; return nil }
			incoming := displayNameActor(outsider, auth.RoleOwner)
			receipt, err := first.openRepositoryAuthorityGoal(incoming, r)
			require.NoError(t, err)
			require.True(t, bound, "journal scope must bind before publication")
			again, err := a.openRepositoryAuthorityGoal(incoming, r)
			require.NoError(t, err)
			require.Equal(t, receipt, again)
			rows, err := workTemplateRows(memql.ContextWithFreshRead(ownerCtx), reader, "workRunForOwner", map[string]any{"runId": receipt.RunID})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			expected := "waiting"
			if immediate {
				expected = "running"
			}
			require.Equal(t, expected, rows[0]["status"])
			if !immediate {
				decision, err := a.repositoryAuthorityDecision(ownerCtx, owner, receipt.ApprovalID)
				require.NoError(t, err)
				require.Equal(t, "pending", decision.State)
			}
		}
	})
}
