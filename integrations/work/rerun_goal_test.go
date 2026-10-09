package work

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRerunReopensOwnedGoalBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, status, owner string
		missing, writeFails bool
	}{
		{name: "closed", status: "closed", owner: actOwner},
		{name: "cancelled wait", status: "closed", owner: actOwner},
		{name: "already open", status: "open", owner: actOwner},
		{name: "missing", missing: true},
		{name: "another owner", status: "closed", owner: "someone-else"},
		{name: "goal write fails", status: "closed", owner: actOwner, writeFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng, steps := newActsIntegration(t)
			addPristineRun(steps, actRunId, "fetch", "draft", "publish")
			run := actRunRow(runStatusCancelled)
			run["cancelRequested"] = true
			if tc.name == "cancelled wait" {
				run["status"] = runStatusWaiting
			}
			eng.reply("workRunForOwner", run)
			future := testNow.Add(time.Minute)
			if tc.missing {
				eng.reply("workGoalForOwner")
			} else {
				eng.reply("workGoalForOwner", map[string]any{"id": actGoalId, "ownerUserId": tc.owner, "status": tc.status, "createdAt": rfc(future), "closeReason": "stopped", "closedAt": rfc(future)})
			}
			if tc.writeFails {
				eng.fail["updateWorkGoal"] = errors.New("goal write failed")
			}
			_, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0)
			refused := tc.missing || tc.owner != actOwner || tc.writeFails
			if (err != nil) != refused {
				t.Fatalf("err=%v refused=%v", err, refused)
			}
			writes := mutationsIn(eng)
			if refused {
				for _, w := range writes {
					if w.Name() == "updateWorkRun" {
						t.Fatal("run dispatched without reopening its owned goal")
					}
				}
				return
			}
			if tc.status == "open" {
				if len(writes) != 1 || writes[0].Name() != "updateWorkRun" {
					t.Fatalf("open goal was rewritten: %s", eng.summary())
				}
				return
			}
			if len(writes) != 2 || writes[0].Name() != "updateWorkGoal" || writes[1].Name() != "updateWorkRun" {
				t.Fatalf("wrong lifecycle order: %s", eng.summary())
			}
			a := writes[0].Args(t)
			if a["status"] != "open" || a["closedAt"] != "" || a["closeReason"] != "" {
				t.Fatalf("stale closure: %v", a)
			}
			stamp, e := time.Parse(time.RFC3339Nano, a["versionTime"].(string))
			if e != nil || !stamp.After(future) {
				t.Fatalf("version %v must survive clock skew", a["versionTime"])
			}
			for _, key := range []string{"ceilings", "spent", "accountIds", "statement"} {
				if _, ok := a[key]; ok {
					t.Fatalf("reopening must preserve %s", key)
				}
			}
			if !writes[0].Origin.IsInternal() || !strings.HasSuffix(writes[0].Actor, ":"+actOwner) {
				t.Fatalf("wrong write authority: %+v", writes[0])
			}
		})
	}
}
