package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func workflowPrivateMediaFixture(t *testing.T) (*callWorld, accountTestKey, *CommerceConfig, callrules.StartResult) {
	t.Helper()
	cw, key, config, started := workflowFixture(t, "workflow-private-media", true)
	if cw == nil {
		return cw, key, config, started
	}
	cw.media.setPresent(started.Call.ID, person(started.Join.Identity, MediaTrack{SID: "private-camera", Source: "camera"}))
	definition := callrules.WorkflowDefinition{ID: "private-media-guidance", Title: "Private guidance", Instructions: "Private host rubric", RequiresInsights: true,
		Roles: []callrules.WorkflowRole{{ID: "facilitator", Label: "Host", Required: true}}, EntryNodeID: "private",
		Nodes: []callrules.WorkflowNode{{ID: "private", Type: "discussion", Label: "Private label", Instructions: "Private facilitator notes", Visibility: "host", RoleID: "facilitator", DurationSeconds: 60, Requirements: callrules.WorkflowRequirements{Video: true}}, {ID: "finish", Type: "finish", Label: "Done", Visibility: "all"}},
		Edges: []callrules.WorkflowEdge{{ID: "next", Source: "private", Target: "finish"}}}
	workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "publish", Definition: &definition, IdempotencyKey: "private-media-publish"})
	active := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: definition.ID, Version: 1, IdempotencyKey: "private-media-start"})
	require.Equal(t, "running", active.Run.Status)
	return cw, key, config, started
}

func TestWorkflowSnapshotRechecksAuthorityAfterMediaInventory(t *testing.T) {
	for _, change := range []string{"credential_revoked", "session_retired", "grant_revoked", "read_scope_removed", "approval_changed"} {
		t.Run(change, func(t *testing.T) {
			cw, key, config, started := workflowPrivateMediaFixture(t)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peer := allowanceReplica(t, cw)
			peer.commerce = func() CommerceConfig { return *config }
			actor := ctx
			grantID := ""
			if change == "grant_revoked" || change == "read_scope_removed" || change == "approval_changed" {
				cleanupDelegationTestRows(t, cw)
				request := delegationTestRequest(cw)
				request.Scopes = append(request.Scopes, "skills_read")
				request.RoomIDs = []string{started.Call.ID}
				_, grantID = delegationTestGrant(t, cw, key, request)
				// The outer agent dispatcher has already selected its internal
				// session; the inner Skills read must still recheck the grant.
				actor = context.WithValue(ctx, agentCallContext{}, grantID)
				grant, err := cw.svc.store.delegation(ctx, cw.owner, grantID)
				require.NoError(t, err)
				actor = context.WithValue(actor, agentApprovalContext{}, delegationApprovalDigest(grant))
			}
			entered, resume, pause := admissionPause(t)
			cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
				backend, err := cw.media.backend(server, credential)
				return delayedHostInventory{MediaBackend: backend, pause: pause}, err
			}
			type answer struct {
				result callrules.WorkflowResult
				err    error
			}
			finished := make(chan answer, 1)
			go func() {
				result, err := cw.svc.Workflow(actor, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
				finished <- answer{result, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("workflow inventory never reached the external boundary")
			}
			release, err := peer.gates(ctx, accountGate(key.result.Account.KeyID))
			require.NoError(t, err)
			fresh := memql.ContextWithFreshRead(ctx)
			switch change {
			case "credential_revoked":
				credential, err := peer.store.credentialByID(fresh, cw.owner, key.result.Credentials[0].ID)
				require.NoError(t, err)
				// Credential revocation is durable before best-effort provider cleanup.
				require.NoError(t, peer.store.updateCredential(fresh, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
			case "session_retired":
				row, _, err := peer.session(fresh, cw.ref, key.result.Session.Token, cw.clock.Now())
				require.NoError(t, err)
				// This is the logout/rotation persistence boundary. Public Logout
				// intentionally refuses a currently occupied call; no bypass is added.
				require.NoError(t, peer.store.updateAccountSession(fresh, cw.owner, row, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
			default:
				grant, err := peer.store.delegation(fresh, cw.owner, grantID)
				require.NoError(t, err)
				if change == "grant_revoked" {
					grant.State.RevokedAt = cw.clock.Now()
				} else if change == "approval_changed" {
					grant.State.Request.Purpose = "Help with another approved conversation"
				} else {
					grant.State.Request.Scopes = []string{"read", "host"}
				}
				require.NoError(t, peer.store.saveDelegation(fresh, cw.owner, grant, cw.clock.Now()))
			}
			release()
			resume()
			select {
			case result := <-finished:
				require.Error(t, result.err, "the caller lost authority during provider I/O")
				require.Nil(t, result.result.Run, "private guidance escaped after authority ended")
				require.Nil(t, result.result.Binding)
			case <-ctx.Done():
				t.Fatal("workflow read did not finish")
			}
		})
	}
}
