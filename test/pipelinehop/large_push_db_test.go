package pipelinehop

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

// A push over the receiver's 256 KiB default still opens its run.
//
// A push payload lists every pushed commit with its changed files, so one push
// of a long branch -- a feature branch merged to main in one go -- is far over
// the default. While GitHub's source was read at the default, such a push was
// answered 413 by the receiver, staged nothing, and its commit never got a run
// or a check, with nothing in the cluster saying why. Driven here at the
// harness's 256 KiB receiver default, through the real receiver, the shipped
// trigger and the run store, with GitHub's source cap as app/'s
// githubWebhookPolicy sets it.
func TestAPushOverTheReceiverDefaultStillOpensItsRun(t *testing.T) {
	c := newCluster(t, dbtest.DSN())
	if c == nil {
		return
	}
	s := c.connect(t, "long", pipelinerun.DeliveryWebhook)
	landed := c.commit(s, "main-long")

	body := s.envelope()
	body["ref"], body["before"], body["after"] = "refs/heads/main", s.base, landed
	body["head_commit"] = map[string]any{"message": "Merge a long branch"}
	commits := make([]map[string]any, 0, 400)
	for i := 0; i < 400; i++ {
		commits = append(commits, map[string]any{
			"id":       commitFor(c.run, s.label, fmt.Sprintf("long-%03d", i)),
			"message":  strings.Repeat("One line of a commit message. ", 30),
			"modified": []string{fmt.Sprintf("component/long/file_%03d.go", i)},
		})
	}
	body["commits"] = commits
	payload := mustJSON(t, body)
	if len(payload) <= 256<<10 {
		t.Fatalf("the push is %d bytes, which is not over the receiver's 256 KiB default", len(payload))
	}

	answer := c.deliverAndFire(t, "push", "long-"+c.run, payload)
	run := c.onlyRun(t, pipelines.RunKey(s.name, landed, pipelines.ModeFull, pipelines.EventPush))
	if opened := answered(t, answer, "opened"); !slices.Equal(opened, []string{run.ID}) {
		t.Errorf("a %d-byte push opened %v; want the run %s", len(payload), opened, run.ID)
	}
	if run.SHA != landed || run.Event != pipelines.EventPush || run.Trigger != pipelinerun.TriggerWebhook {
		t.Errorf("the run row = %+v", run)
	}
}
