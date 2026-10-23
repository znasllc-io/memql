//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/repositoryauthority"
	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/dsl"
	work "github.com/znasllc-io/memql/integrations/work"
)

// Use actual native request/approval journals, actual Work intake and the
// production agent Dispatch on a second engine. Only provider access and the
// bounded repository read are fixtures; no model, GitHub or cluster is called.
func TestRepositoryWorkDispatchUsesExactApprovedClosure(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gate, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gate.Close()
	apps := []*App{{engine: writer, Logger: writer.Logger}, {engine: reader, Logger: reader.Logger}}
	var authorities []*repositoryauthority.Integration
	var workers []*work.Integration
	for _, a := range apps {
		w := work.New(a.engine, a.Logger, func() *bun.DB { return db })
		w.SetGateDB(func() *sql.DB { return gate })
		require.NoError(t, a.engine.RegisterIntegration(w))
		authority := repositoryauthority.New(repositoryauthority.Deps{})
		require.NoError(t, a.engine.RegisterIntegration(authority))
		w.SetPlanReviewValidator(repositoryauthority.ReviewType, authority.ValidateReview)
		workers = append(workers, w)
		authorities = append(authorities, authority)
	}
	for _, scenario := range []string{"unchanged", "entry changed after approval", "child changed before recovery", "logic changed before recovery", "child changes during execution", "revoked before dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			key := strings.ReplaceAll(id.NewShortId(), "-", "")
			owner, requester := "recipeowner"+key, "requester"+key
			ownerCtx := displayNameActor(owner, auth.RoleDeveloper)
			requesterCtx := displayNameActor(requester, auth.RoleDeveloper)
			templateMutation(t, writer, auth.ContextWithInternalOrigin(ownerCtx), "identity.createUserOnFirstLogin", map[string]any{"userId": owner, "displayName": "Recipe maintainer", "primaryEmail": owner + "@example.test", "role": "developer", "internal": false})
			t.Cleanup(func() {
				_, err := db.Exec(`DELETE FROM repository_change_requests WHERE requester = ? OR maintainer = ?`, requester, owner)
				require.NoError(t, err)
				_, err = db.Exec(`DELETE FROM "MemoryNodes" WHERE payload->>'ownerUserId' IN (?, ?) OR id = ?`, owner, "v1:identity:user:"+owner, "v1:identity:user:"+owner)
				require.NoError(t, err)
			})
			entry, child, logic, policy := "recipe"+key, "child"+key, "value"+key, "route"+key
			rootSource := `@template
automation ` + entry + ` {
 args { requestId string! repository string! repositoryId int! statement string! operation string! paths []string! parentRequestId string! }
 evidence := builtin repositoryChangeContext(requestId: args.requestId)
 result := automation ` + child + `()
 return result
}`
			childSource := "@template\nautomation " + child + " { return logic " + logic + "() }"
			logicSource := "logic " + logic + " { return 7 }"
			sources := map[string]string{
				"automation:" + entry: rootSource,
				"automation:" + child: childSource,
				"logic:" + logic:      logicSource,
				"automation:" + policy: `@template
automation ` + policy + ` {
 args {repository string! sourceId string! sourceOwnerId string! requesterCanWrite bool! bindings object!}
 return {sourceId: "maintainer-source", template: "` + entry + `", ttlSeconds: 3600}
}`,
			}
			tree := fstest.MapFS{dslfs.ManifestFile: {Data: []byte(dslfs.Manifest{Language: parser.LanguageVersion, Edition: parser.Edition}.Render())}}
			publish := func() {
				tree["automations.memql"] = &fstest.MapFile{Data: []byte(sources["automation:"+entry] + "\n" + sources["automation:"+child])}
				tree["logics.memql"] = &fstest.MapFile{Data: []byte(sources["logic:"+logic])}
			}
			publish()
			domain := "recipetest" + key
			dsl.RegisterTree(domain, tree)
			defer dsl.UnregisterTree(domain)
			reads := 0
			for n, authority := range authorities {
				a := apps[n]
				authority.Configure(func(d *repositoryauthority.Deps) {
					d.DB = db.DB
					d.Source = func(_ context.Context, source string, _ bool) (repositoryauthority.Source, error) {
						who := owner
						if source == "request-source" {
							who = requester
						}
						return repositoryauthority.Source{ID: source, OwnerUserID: who, CredentialID: "connection", Repository: "core/engine"}, nil
					}
					d.RepositoryAccess = func(_ context.Context, source repositoryauthority.Source) (packages.RepositoryAuthority, error) {
						return packages.RepositoryAuthority{Repository: source.Repository, RepositoryID: 42, CanWrite: source.OwnerUserID == owner}, nil
					}
					d.SystemVariable = func(_ context.Context, name string) (string, error) {
						if name == repositoryauthority.ResolverVariable {
							return policy, nil
						}
						return "{}", nil
					}
					d.WorkflowSource = func(kind, name string) (string, error) {
						if body, ok := sources[kind+":"+name]; ok {
							return body, nil
						}
						return "", fmt.Errorf("fixture source not installed: %s:%s", kind, name)
					}
					d.OpenGoal, d.Decision, d.ExecutionActive = a.openRepositoryAuthorityGoal, a.repositoryAuthorityDecision, a.repositoryAuthorityExecutionActive
					d.Read = func(context.Context, repositoryauthority.Authorization, []string) (repositoryauthority.RepositorySnapshot, error) {
						reads++
						if scenario == "child changes during execution" {
							sources["automation:"+child] = "@template\nautomation " + child + " { return builtin unrestrictedProbe() }"
							publish()
						}
						return repositoryauthority.RepositorySnapshot{ExpectedHead: strings.Repeat("a", 40), BaseBranch: "main", Files: []repositoryauthority.Replacement{{Path: "component/example.go", Content: "package example"}}}, nil
					}
				})
			}
			r, err := authorities[0].Request(requesterCtx, repositoryauthority.Request{SourceID: "request-source", RequestID: key, Statement: "Bounded fixture", Operation: repositoryauthority.OperationPropose, Paths: []string{"component/example.go"}})
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
			receipt := work.ReviewGoalIdentity(owner, r.ID)
			for _, cap := range workers[0].Capabilities() {
				if cap.Name == "decideApproval" {
					_, err = cap.Handler(ownerCtx, map[string]any{"approvalId": receipt.ApprovalID, "decision": "approved", "answer": map[string]any{"reason": "Approve exact fixture scope"}}, 0)
					require.NoError(t, err)
				}
			}
			journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ownerCtx), reader, receipt.RunID)
			require.NoError(t, err)
			require.Equal(t, "running", journal.Status)
			require.Equal(t, "repository-change", journal.TriggeredBy)
			require.Empty(t, journal.TemplateFingerprint, "the native recipe journal is the approval seal before first execution")
			switch scenario {
			case "entry changed after approval":
				sources["automation:"+entry] = strings.Replace(rootSource, "evidence :=", "unapproved := builtin unrestrictedProbe()\n evidence :=", 1)
			case "child changed before recovery":
				sources["automation:"+child] = "@template\nautomation " + child + " { return builtin unrestrictedProbe() }"
			case "logic changed before recovery":
				sources["logic:"+logic] = "logic " + logic + " { return 99 }"
			case "revoked before dispatch":
				_, err = authorities[0].Revoke(ownerCtx, r.ID, "Withdraw approval before execution")
				require.NoError(t, err)
			}
			publish()
			// Ensure the ordinary installed loader sees the changed source; the
			// refusal must come from the approval seal, not a missing template.
			loader := automations.NewLoader(automations.LoaderOptions{Logger: reader.Logger})
			_, err = loader.LoadByName(entry)
			require.NoError(t, err)
			probe := &stepsRan{}
			registry := steps.NewRegistry()
			registry.Register(automations.StepTypeFunction, probe)
			a := apps[1]
			a.automationLoader, a.stepRegistry = loader, registry
			executor := automations.NewExecutor(automations.ExecutorOptions{Logger: reader.Logger, Engine: reader, StepRegistry: registry})
			defer executor.Close()
			dispatcher := &workRunDispatcher{app: a, exec: executor}
			dispatcher.Dispatch(context.Background(), work.DispatchRequest{RunId: receipt.RunID, GoalId: receipt.GoalID, OwnerUserId: owner, Status: "running", AutomationName: entry, Recovery: true})
			after, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ownerCtx), reader, receipt.RunID)
			require.NoError(t, err)
			probe.mu.Lock()
			require.Empty(t, probe.ran, "repository recipes must never use the ambient executor for new operations")
			probe.mu.Unlock()
			if scenario == "unchanged" || scenario == "child changes during execution" {
				require.Equal(t, 1, reads)
				require.Equal(t, "succeeded", after.Status)
				require.Equal(t, "repository-change", after.TriggeredBy, "recovery keeps its native admission type")
				require.EqualValues(t, 7, after.Steps["result"].Value, "the child and pure logic resolve only from the captured closure")
			} else {
				require.Zero(t, reads, "a changed recipe must refuse before even its original first operation")
				require.Equal(t, "failed", after.Status)
				require.Empty(t, after.Steps)
			}
		})
	}
}
