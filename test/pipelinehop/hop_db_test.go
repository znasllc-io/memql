package pipelinehop

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

// hop_db_test.go -- the cases. Every one runs on the same two replicas, each on
// a repository and a pipeline of its own.
//
// HOW TO CONFIRM IT IS LOAD-BEARING: delete the two v1:pipelines:run rules
// from component/node/routing.go's core slice and run this file. The run's
// created event no longer reaches the agent, nothing claims the run, and the
// hop case fails waiting for its conclusion, saying "B heard 0 event(s) for
// the run". Skip EnableDriver on B in harness_test.go instead and it fails the
// same way with the event heard and nobody claiming it. If it passed either
// way it would prove nothing, which is the whole risk a cross-node feature
// carries.

// The routing decision the mesh link applies (harness_test.go) is the real
// one, asserted on its own so a dropped rule fails here, naming the file, and
// not only as a hang in the hop.
func TestTheMeshCarriesARunAndNotItsDelivery(t *testing.T) {
	for _, topic := range []string{runCreated, runUpdated} {
		if forward, broadcast, _ := node.ForwardDecisionFor(topic); !forward || !broadcast {
			t.Errorf("%s must broadcast across the mesh (forward=%v broadcast=%v): a run is opened on the bff and claimed "+
				"by an agent, and without the rule in component/node/routing.go's core slice the agent never hears of it", topic, forward, broadcast)
		}
	}
	if forward, _, _ := node.ForwardDecisionFor(stagedCreated); forward {
		t.Errorf("%s must stay on the node that staged it: a delivery is handled where it arrived, and only the run it opens crosses", stagedCreated)
	}
}

func TestAGitHubDeliveryIsOpenedOnTheBffAndDrivenOnTheAgent(t *testing.T) {
	c := newCluster(t, dbtest.DSN())
	if c == nil {
		return
	}

	t.Run("the trigger's filter admits a received GitHub row and nothing else", func(t *testing.T) {
		body := []byte(`{"zen":"Keep it logically awesome.","hook_id":5491,"run":"` + c.run + `"}`)
		staged := c.deliver(t, "ping", "filter-"+c.run, body)
		ev := c.stagedEvents(t, staged, 1)[0]
		if ok, err := c.admits(ev); err != nil || !ok {
			t.Fatalf("the filter refuses the row the receiver staged (%v): %v", err, ev.Payload["payload"])
		}
		for name, change := range map[string]map[string]any{
			"a processed row":      {"status": "processed"},
			"a failed row":         {"status": "failed"},
			"another source's row": {"source": "shopify"},
		} {
			variant := withFields(t, ev, change)
			if ok, err := c.admits(variant); err != nil || ok {
				t.Errorf("the filter admits %s (%v), which must not fire the trigger again", name, err)
			}
		}
	})

	t.Run("a pull request opened on the bff is claimed and driven by the agent", func(t *testing.T) {
		s := c.connect(t, "hop", pipelinerun.DeliveryWebhook)
		head := c.commit(s, "change-11")
		deliveryID := "hop-" + c.run
		answer := c.deliverAndFire(t, "pull_request", deliveryID, s.pullRequest(t, "opened", 11, head, s.name))

		key := pipelines.RunKey(s.name, head, pipelines.ModeAffected, pipelines.EventPullRequest)
		run := c.awaitConcluded(t, s, c.onlyRun(t, key).ID)
		if opened := answered(t, answer, "opened"); !slices.Equal(opened, []string{run.ID}) {
			t.Errorf("the trigger on %s opened %v; want the run %s", nodeA, opened, run.ID)
		}

		// The row: opened from the delivery, driven to success by the agent.
		if run.DriverNodeID != nodeB || run.Conclusion != pipelinerun.ConclusionSuccess {
			t.Fatalf("run %s concluded %s by %q (refusal %q %q); want success by %s\n%s",
				run.ID, run.Conclusion, run.DriverNodeID, run.RefusalCode, run.RefusalMessage, nodeB, c.diagnose(s, run.ID))
		}
		if run.Mode != pipelines.ModeAffected || run.Event != pipelines.EventPullRequest || run.Attempt != 1 ||
			run.Trigger != pipelinerun.TriggerWebhook || run.DeliveryID != deliveryID || run.PullRequest != 11 ||
			run.SHA != head || run.BaseSHA != s.base || run.WorkRunID == "" || run.CheckRunState != pipelinerun.CheckRunWritten {
			t.Errorf("the run row = %+v", run)
		}
		if len(run.Stages) != len(testStages) || run.Stages[0].Status != pipelinerun.StagePassed || run.Stages[1].Status != pipelinerun.StagePassed {
			t.Errorf("the run's stage table = %+v; want %v, passed", run.Stages, testStages)
		}

		// Its history. Opened queued with no driver -- on the bff, in ONE
		// write that already names its check run, so a driver claiming it the
		// moment it hears of it finds the row complete (open.go) -- and from
		// the claim on, held by the agent and nobody else.
		versions := c.versions(t, run.ID)
		if first := versions[0]; first.Status != pipelinerun.StatusQueued || first.Driver != "" {
			t.Errorf("the run's first version is %+v; want queued with no driver", first)
		}
		for i, v := range versions[1:] {
			if v.Driver != nodeB {
				t.Errorf("version %d of the run names driver %q; the opener writes the row once, and only %s, holding the lease, writes it after (%+v)",
					i+1, v.Driver, nodeB, versions)
			}
		}
		if last := versions[len(versions)-1]; last.Status != pipelinerun.StatusCompleted || last.Conclusion != pipelinerun.ConclusionSuccess {
			t.Errorf("the run's last version is %+v", last)
		}

		// The hop: the event announcing the row's first version crossed from
		// A, and the agent learned of the run from nothing else. (Every write
		// to an append-only row publishes graph.node.created; firstVersion
		// says which one materialized it. The agent's own writes -- its
		// claim, its progress, its conclusion -- are the later versions.)
		created := c.heardB.matching(func(ev events.Event) bool {
			return ev.Topic == runCreated && bareOf(ev.Payload["id"]) == run.ID
		})
		var opened []events.Event
		for _, ev := range created {
			switch {
			case ev.Payload["firstVersion"] == true:
				opened = append(opened, ev)
			case ev.OriginNodeId != "":
				t.Errorf("B heard a later version of the run from %q; the opener writes the row once, and only the agent holding the lease writes it after", ev.OriginNodeId)
			}
		}
		if len(opened) != 1 || opened[0].OriginNodeId != nodeA {
			origins := make([]string, 0, len(opened))
			for _, ev := range opened {
				origins = append(origins, ev.OriginNodeId)
			}
			t.Errorf("B heard the run's first version from %q; want it once, carried from %s", origins, nodeA)
		}
		if c.link.forwardedFrom(nodeA, runCreated) == 0 {
			t.Errorf("no %s crossed from %s", runCreated, nodeA)
		}
		// The delivery did not cross: it was published on A, and the rules
		// kept it there.
		if n := c.link.refusedFrom(nodeA, stagedCreated); n == 0 {
			t.Errorf("A published no %s for the link to keep local; the staged delivery's event is not where it should be", stagedCreated)
		}
		if n := c.link.forwardedFrom(nodeA, stagedCreated); n != 0 {
			t.Errorf("%d %s event(s) crossed to the agent; a delivery stays on the node that staged it", n, stagedCreated)
		}
		if heard := c.heardB.matching(func(ev events.Event) bool { return strings.HasSuffix(ev.Topic, "."+inboundConcept) }); len(heard) != 0 {
			t.Errorf("B's bus carried %d %s event(s)", len(heard), inboundConcept)
		}

		// The runner: every step, stage by stage, under the rows' own ids.
		steps, err := c.b.store.WorkSteps(fresh(), run.WorkRunID)
		if err != nil {
			t.Fatalf("workStepsForRun %s: %v", run.WorkRunID, err)
		}
		var workKeys []string
		for _, st := range steps {
			workKeys = append(workKeys, st.Key)
			if st.Status != pipelinerun.WorkStepDone || st.Attempt != 1 {
				t.Errorf("work step %s is %s at attempt %d; want done at 1", st.Key, st.Status, st.Attempt)
			}
		}
		sort.Strings(workKeys)
		if !slices.Equal(workKeys, testStepKeys) {
			t.Errorf("the work run's steps are %v; want %v", workKeys, testStepKeys)
		}
		reqs := c.runner.forRun(run.ID)
		var sent []string
		stage := 0
		for _, req := range reqs {
			sent = append(sent, req.StepKey)
			at := slices.Index(testStages, req.Step.Stage)
			if at < stage {
				t.Errorf("%s (stage %s) was handed to the runner after a later stage's step: %v", req.StepKey, req.Step.Stage, keysOf(reqs))
			}
			stage = max(stage, at)
			if req.RunID != run.ID || req.RunID != memql.BareShortId(req.RunID) {
				t.Errorf("%s: RunID %q; want the run row's bare id %q", req.StepKey, req.RunID, run.ID)
			}
			if req.WorkRunID != memql.BareShortId(run.WorkRunID) || req.WorkRunID != memql.BareShortId(req.WorkRunID) {
				t.Errorf("%s: WorkRunID %q; want the work run's bare id %q", req.StepKey, req.WorkRunID, memql.BareShortId(run.WorkRunID))
			}
			if req.Attempt != 1 || req.RunAttempt != 1 || req.SHA != head || req.Mode != pipelines.ModeAffected ||
				req.Event != pipelines.EventPullRequest || req.Version != head || req.InstallationID != s.installation ||
				req.PipelineID != memql.BareShortId(s.pipeline.ID) || req.Repository.Owner+"/"+req.Repository.Name != s.name {
				t.Errorf("%s: the request = %+v", req.StepKey, req)
			}
		}
		sort.Strings(sent)
		if !slices.Equal(sent, workKeys) {
			t.Errorf("the runner was handed %v; the work run's steps are %v -- each request's StepKey is its v1:work:step's key", sent, workKeys)
		}
		if work := c.workRunStatus(t, run.WorkRunID); work["status"] != "succeeded" || work["nodeId"] != nodeB ||
			work["triggeredBy"] != pipelines.WorkTriggerPrefix+string(pipelines.ModeAffected) {
			t.Errorf("the work run is %v, opened on %v, triggered by %v; want succeeded, opened by %s, the runner's own",
				work["status"], work["nodeId"], work["triggeredBy"], nodeB)
		}

		// GitHub: the check run created queued where the run was opened, and
		// moved by the agent that drove it -- in progress, then completed.
		writes := c.github.writesFor(s.name)
		if len(writes) < 3 {
			t.Fatalf("GitHub saw %d check-run writes for %s; want a create and at least two updates\n%s", len(writes), s.name, c.diagnose(s, run.ID))
		}
		created0 := writes[0]
		if !created0.Create || created0.Node != nodeA || created0.Run.Status != pipelinerun.StatusQueued ||
			created0.ID != run.CheckRunID || created0.Run.ExternalID != run.ID || created0.Run.HeadSHA != head ||
			created0.Run.Name != pipelines.CheckRunName("shop") || created0.Run.DetailsURL != pipelines.RunPageURL(osOrigin, run.ID) {
			t.Errorf("the first check-run write = %+v; want %s creating it queued, naming run %s", created0, nodeA, run.ID)
		}
		for i, w := range writes[1:] {
			if w.Create || w.Node != nodeB || w.ID != run.CheckRunID {
				t.Errorf("check-run write %d = %s create=%v id %d; want %s moving check run %d", i+1, w.Node, w.Create, w.ID, nodeB, run.CheckRunID)
			}
		}
		if first := writes[1].Run; first.Status != pipelinerun.StatusInProgress {
			t.Errorf("the agent's first check-run write is %s; want in_progress", first.Status)
		}
		for _, w := range writes[1 : len(writes)-1] {
			if w.Run.Status != pipelinerun.StatusInProgress {
				t.Errorf("a check-run write before the conclusion is %s/%s; want in_progress", w.Run.Status, w.Run.Conclusion)
			}
		}
		if last := writes[len(writes)-1].Run; last.Status != pipelinerun.StatusCompleted || last.Conclusion != "success" {
			t.Errorf("the last check-run write is %s/%s; want completed/success", last.Status, last.Conclusion)
		}

		t.Logf("run %s: first version %+v carried from %s, %d versions, every later one held by %s; check run %d created by %s and written %d times; runner handed %v",
			run.ID, versions[0], nodeA, len(versions), nodeB, run.CheckRunID, writes[0].Node, len(writes), keysOf(reqs))

		// The plan was read where the run was driven, at the run's commit,
		// and the change compared against the pull request's base.
		if !slices.Contains(c.github.callsOf(&c.github.trees, s.name), ghCall{Node: nodeB, Repository: s.name, Arg: head}) {
			t.Errorf("the agent never read the tree at %s: %v", head, c.github.callsOf(&c.github.trees, s.name))
		}
		if !slices.Contains(c.github.callsOf(&c.github.compares, s.name), ghCall{Node: nodeB, Repository: s.name, Arg: s.base + "..." + head}) {
			t.Errorf("the agent never compared %s...%s: %v", s.base, head, c.github.callsOf(&c.github.compares, s.name))
		}
	})

	t.Run("a pull request from a fork is refused where it is opened and never driven", func(t *testing.T) {
		s := c.connect(t, "fork", pipelinerun.DeliveryWebhook)
		head := c.commit(s, "fork-12")
		answer := c.deliverAndFire(t, "pull_request", "fork-"+c.run, s.pullRequest(t, "opened", 12, head, "mallory-"+c.run+"/shop"))

		run := c.onlyRun(t, pipelines.RunKey(s.name, head, pipelines.ModeAffected, pipelines.EventPullRequest))
		if opened := answered(t, answer, "opened"); !slices.Equal(opened, []string{run.ID}) {
			t.Errorf("the trigger opened %v; want the fork's refused run %s", opened, run.ID)
		}
		if run.Status != pipelinerun.StatusCompleted || run.Conclusion != pipelinerun.ConclusionRefused ||
			run.RefusalCode != pipelines.CodeForkRefused || run.CheckRunID == 0 || run.CheckRunState != pipelinerun.CheckRunWritten {
			t.Fatalf("the fork's run = %+v; want completed, refused %s, with its check run written", run, pipelines.CodeForkRefused)
		}
		writes := c.github.writesFor(s.name)
		if len(writes) != 1 || !writes[0].Create || writes[0].Node != nodeA || writes[0].ID != run.CheckRunID ||
			writes[0].Run.Status != pipelinerun.StatusCompleted || writes[0].Run.Conclusion != "failure" ||
			writes[0].Run.Output == nil || !strings.Contains(strings.ToLower(writes[0].Run.Output.Title), "fork") {
			t.Fatalf("GitHub saw %+v; want one check run created completed/failure by %s, refusing the fork", writes, nodeA)
		}

		// The agent heard of the run...
		awaitHeard(t, c, run.ID)
		// ...and a pull request of the same pipeline from its own repository,
		// delivered after the fork's, is driven to success by that agent --
		// so what follows is a refusal, not a pipeline nothing could drive.
		own := c.commit(s, "own-13")
		c.deliverAndFire(t, "pull_request", "fork-control-"+c.run, s.pullRequest(t, "opened", 13, own, s.name))
		control := c.awaitConcluded(t, s, c.onlyRun(t, pipelines.RunKey(s.name, own, pipelines.ModeAffected, pipelines.EventPullRequest)).ID)
		if control.DriverNodeID != nodeB || control.Conclusion != pipelinerun.ConclusionSuccess || len(c.runner.forRun(control.ID)) != len(testStepKeys) {
			t.Fatalf("the control run concluded %s by %q with %d steps run; want success by %s", control.Conclusion, control.DriverNodeID, len(c.runner.forRun(control.ID)), nodeB)
		}

		if reqs := c.runner.forRun(run.ID); len(reqs) != 0 {
			t.Errorf("the runner was handed %d step(s) of the fork's run: %v", len(reqs), keysOf(reqs))
		}
		if versions := c.versions(t, run.ID); len(versions) != 1 || versions[0].Driver != "" {
			t.Errorf("the fork's run was written again after it was refused: %+v", versions)
		}
		if writes := c.github.checkRun(run.CheckRunID); len(writes) != 1 {
			t.Errorf("the fork's check run was written %d times; want once, refused", len(writes))
		}
		t.Logf("fork run %s: refused %s on %s, heard by %s and never driven; control run %s driven by %s",
			run.ID, run.RefusalCode, nodeA, nodeB, control.ID, control.DriverNodeID)
	})

	t.Run("a redelivery and a poll of the same head open one run and one check run", func(t *testing.T) {
		s := c.connect(t, "dedup", pipelinerun.DeliveryPoll)

		// The first poll records the heads and opens nothing.
		if res := c.poll(t); len(runsOf(res.Opened, s)) != 0 || len(runsOf(res.Existing, s)) != 0 {
			t.Fatalf("the first poll acted on the pipeline: opened %v, existing %v", runsOf(res.Opened, s), runsOf(res.Existing, s))
		}
		if p, err := c.b.store.PipelineByID(fresh(), s.pipeline.ID); err != nil || p == nil || len(p.Heads) != 1 || p.Heads["branch:main"] != s.base {
			t.Fatalf("after the first poll the pipeline's heads are %+v (%v); want the default branch's head alone", p, err)
		}

		// The same delivery, twice: GitHub redelivers. One row, two events.
		head := c.commit(s, "change-21")
		body := s.pullRequest(t, "opened", 21, head, s.name)
		deliveryID := "dedup-" + c.run
		staged := c.deliver(t, "pull_request", deliveryID, body)
		if again := c.deliver(t, "pull_request", deliveryID, body); again != staged {
			t.Fatalf("a redelivery staged row %s beside %s; staging is idempotent by the signed body", again, staged)
		}
		evs := c.stagedEvents(t, staged, 2)
		// The first receipt's event first, as they were published.
		sort.SliceStable(evs, func(i, j int) bool {
			return evs[i].Payload["firstVersion"] == true && evs[j].Payload["firstVersion"] != true
		})
		if evs[0].Payload["firstVersion"] != true || evs[1].Payload["firstVersion"] != false {
			t.Errorf("the two events' firstVersion = %v, %v; want the receipt, then the redelivery", evs[0].Payload["firstVersion"], evs[1].Payload["firstVersion"])
		}
		receipt, redelivery := c.fire(t, evs[0]), c.fire(t, evs[1])
		key := pipelines.RunKey(s.name, head, pipelines.ModeAffected, pipelines.EventPullRequest)
		run := c.onlyRun(t, key)
		// Both reached the trigger: the receipt opened the run, and the
		// redelivery was answered by it -- the run key, not the filter, is
		// what kept it to one.
		if opened := answered(t, receipt, "opened"); !slices.Equal(opened, []string{run.ID}) {
			t.Errorf("the receipt's trigger opened %v; want %s", opened, run.ID)
		}
		if opened, existing := answered(t, redelivery, "opened"), answered(t, redelivery, "existing"); len(opened) != 0 || !slices.Equal(existing, []string{run.ID}) {
			t.Errorf("the redelivery's trigger opened %v and found %v; want nothing opened, %s found", opened, existing, run.ID)
		}

		// The poll, on the agent, sees the same head.
		c.github.update(s.name, func(r *fakeRepo) {
			r.pulls = []githubapp.PullRequestHead{{Number: 21, Title: "Change 21", HeadSHA: head, HeadRef: "change-21", HeadRepository: s.name, BaseSHA: s.base}}
		})
		res := c.poll(t)
		if opened := runsOf(res.Opened, s); len(opened) != 0 {
			t.Errorf("the poll opened %v for a head a delivery already opened a run for", opened)
		}
		if existing := runsOf(res.Existing, s); len(existing) != 1 || existing[0] != run.ID {
			t.Errorf("the poll found %v; want the delivery's run %s", existing, run.ID)
		}
		if again := c.onlyRun(t, key); again.ID != run.ID {
			t.Fatalf("the run of %s is %s after the poll, %s before", key, again.ID, run.ID)
		}
		if p, err := c.b.store.PipelineByID(fresh(), s.pipeline.ID); err != nil || p == nil || p.Heads["pr:21"] != head {
			t.Errorf("after the poll the pipeline's heads are %+v (%v); want pr:21 at %s", p, err, head)
		}

		done := c.awaitConcluded(t, s, run.ID)
		if done.DriverNodeID != nodeB || done.Conclusion != pipelinerun.ConclusionSuccess {
			t.Errorf("the run concluded %s by %q; want success by %s", done.Conclusion, done.DriverNodeID, nodeB)
		}
		var creates []checkWrite
		for _, w := range c.github.writesFor(s.name) {
			if w.Create {
				creates = append(creates, w)
			}
		}
		if len(creates) != 1 || creates[0].ID != done.CheckRunID {
			t.Errorf("GitHub saw %d check runs created for %s (%+v); want exactly one, the run's %d", len(creates), s.name, creates, done.CheckRunID)
		}
		if reqs := c.runner.forRepository(s.name); len(reqs) != len(testStepKeys) {
			t.Errorf("the runner was handed %d step(s) for %s; want one run's %d", len(reqs), s.name, len(testStepKeys))
		}
		// Asked now, long after both receipts: one created event per receipt.
		if n := len(c.stagedEvents(t, staged, 2)); n != 2 {
			t.Errorf("A's bus carried %d created events for the staged delivery; want one per receipt", n)
		}
		t.Logf("one delivery received twice (staged %s, fired twice) and polled once: run %s, check run %d, %d run row(s), %d check run(s) created",
			staged, run.ID, done.CheckRunID, c.runRows(t, key), len(creates))
	})

	t.Run("each event opens its mode, and a re-requested check run opens attempt 2", func(t *testing.T) {
		s := c.connect(t, "table", pipelinerun.DeliveryWebhook)
		pr, queued, landed, tagged := c.commit(s, "pr-31"), c.commit(s, "queue-31"), c.commit(s, "main-31"), c.commit(s, "v1.31.0")
		const tag = "v1.31.0"
		c.github.update(s.name, func(r *fakeRepo) { r.tags["tags/"+tag] = tagged })

		table := []struct {
			header  string
			body    []byte
			sha     string
			mode    pipelines.Mode
			event   pipelines.Event
			version string
		}{
			{"pull_request", s.pullRequest(t, "opened", 31, pr, s.name), pr, pipelines.ModeAffected, pipelines.EventPullRequest, pr},
			{"merge_group", s.mergeGroup(t, queued), queued, pipelines.ModeFull, pipelines.EventMergeGroup, queued},
			{"push", s.push(t, s.base, landed), landed, pipelines.ModeFull, pipelines.EventPush, landed},
			{"release", s.release(t, tag), tagged, pipelines.ModeFull, pipelines.EventRelease, tag},
		}
		runs := map[pipelines.Event]pipelinerun.Run{}
		for _, tc := range table {
			c.deliverAndFire(t, tc.header, "table-"+tc.header+"-"+c.run, tc.body)
			run := c.awaitConcluded(t, s, c.onlyRun(t, pipelines.RunKey(s.name, tc.sha, tc.mode, tc.event)).ID)
			if run.Mode != tc.mode || run.Event != tc.event || run.SHA != tc.sha || run.Version != tc.version || run.Attempt != 1 ||
				run.DriverNodeID != nodeB || run.Conclusion != pipelinerun.ConclusionSuccess {
				t.Errorf("%s: the run is %s/%s at %s version %q attempt %d, concluded %s by %q; want %s/%s at %s version %q, success by %s",
					tc.header, run.Mode, run.Event, run.SHA, run.Version, run.Attempt, run.Conclusion, run.DriverNodeID,
					tc.mode, tc.event, tc.sha, tc.version, nodeB)
			}
			reqs := c.runner.forRun(run.ID)
			if len(reqs) != len(testStepKeys) {
				t.Errorf("%s: the runner was handed %d steps; want %d", tc.header, len(reqs), len(testStepKeys))
			}
			for _, req := range reqs {
				if req.Mode != tc.mode || req.Event != tc.event || req.SHA != tc.sha || req.Version != tc.version {
					t.Errorf("%s: %s was handed as %s/%s at %s version %q", tc.header, req.StepKey, req.Mode, req.Event, req.SHA, req.Version)
				}
			}
			runs[tc.event] = run
		}
		// The release named a tag, and its commit was asked of GitHub where
		// the delivery was handled.
		if refs := c.github.callsOf(&c.github.refs, s.name); !slices.Equal(refs, []ghCall{{Node: nodeA, Repository: s.name, Arg: "tags/" + tag}}) {
			t.Errorf("the tag was resolved by %+v; want one ask for tags/%s by %s", refs, tag, nodeA)
		}

		// A re-requested check run is the next attempt of the run it reports,
		// in that run's mode and event.
		original := runs[pipelines.EventPullRequest]
		c.deliverAndFire(t, "check_run", "table-rerun-"+c.run, s.checkRunRerequested(t, original.CheckRunID, pr))
		key := pipelines.RunKey(s.name, pr, pipelines.ModeAffected, pipelines.EventPullRequest)
		if n := c.runRows(t, key); n != 2 {
			t.Fatalf("run key %s has %d runs after the re-request; want the original and its second attempt", key, n)
		}
		attempts, err := c.b.store.RunsForKey(fresh(), key)
		if err != nil {
			t.Fatal(err)
		}
		var second *pipelinerun.Run
		for i := range attempts {
			if attempts[i].ID != original.ID {
				second = &attempts[i]
			}
		}
		if second == nil {
			t.Fatalf("the re-request opened no second run of %s: %+v", key, attempts)
		}
		rerun := c.awaitConcluded(t, s, second.ID)
		if rerun.Attempt != 2 || rerun.Trigger != pipelinerun.TriggerRerun || rerun.RerunOf != original.ID ||
			rerun.Mode != pipelines.ModeAffected || rerun.Event != pipelines.EventPullRequest || rerun.SHA != pr ||
			rerun.DriverNodeID != nodeB || rerun.Conclusion != pipelinerun.ConclusionSuccess ||
			rerun.CheckRunID == 0 || rerun.CheckRunID == original.CheckRunID {
			t.Errorf("the re-run = %+v; want attempt 2 of %s, affected pull_request, its own check run, success by %s", rerun, original.ID, nodeB)
		}
		for _, req := range c.runner.forRun(rerun.ID) {
			if req.RunAttempt != 2 || req.Attempt != 1 {
				t.Errorf("%s of the re-run was handed as run attempt %d, step attempt %d; want 2 and 1", req.StepKey, req.RunAttempt, req.Attempt)
			}
		}
		if again, err := c.b.store.RunByID(fresh(), original.ID); err != nil || again == nil || again.Attempt != 1 || again.Conclusion != pipelinerun.ConclusionSuccess {
			t.Errorf("the original after its re-run = %+v (%v); want it as it ended", again, err)
		}
		for _, e := range []pipelines.Event{pipelines.EventPullRequest, pipelines.EventMergeGroup, pipelines.EventPush, pipelines.EventRelease} {
			r := runs[e]
			t.Logf("%s -> %s at %s, version %s, driven by %s", e, r.Mode, shortSHA(r.SHA), r.Version, r.DriverNodeID)
		}
		t.Logf("check_run rerequested -> %s/%s attempt %d of %s, driven by %s", rerun.Mode, rerun.Event, rerun.Attempt, rerun.RerunOf, rerun.DriverNodeID)
	})
}

// awaitHeard waits until B's bus has carried the run's created event from A.
func awaitHeard(t *testing.T, c *cluster, runID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if len(c.heardB.matching(func(ev events.Event) bool {
			return ev.Topic == runCreated && ev.OriginNodeId == nodeA && bareOf(ev.Payload["id"]) == runID
		})) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never heard run %s's created event from %s", runID, nodeA)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runsOf is the ids of the runs of s's pipeline among runs.
func runsOf(runs []pipelinerun.Run, s *source) []string {
	var out []string
	for _, r := range runs {
		if memql.BareShortId(r.PipelineID) == memql.BareShortId(s.pipeline.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func keysOf(reqs []pipelines.StepRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.StepKey)
	}
	return out
}

// withFields is ev with fields changed on the row it carries -- both the
// payload the event flattens to its top level and the stored payload under
// "payload" -- through a JSON copy, so the original is untouched.
func withFields(t *testing.T, ev events.Event, fields map[string]any) events.Event {
	t.Helper()
	raw, err := json.Marshal(ev.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	stored, _ := payload["payload"].(map[string]any)
	for k, v := range fields {
		payload[k] = v
		if stored != nil {
			stored[k] = v
		}
	}
	out := ev.Clone()
	out.Payload = payload
	return out
}
