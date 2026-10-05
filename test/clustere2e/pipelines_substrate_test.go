//go:build clustere2e && agent

package clustere2e

// pipelines_substrate_test.go -- the pipelines substrate's cluster-e2e leg
// (epic memql#5478, issue memql#5497).
//
// ONE MANIFEST, END TO END, AGAINST A REAL CLUSTER. testdata/pipelines/
// manifest.yaml is read and compiled by the seam's own functions, the way a run
// reads its tree (component/pipelinerun/tree.go), and every step it compiles to
// is handed to the substrate's real Executor the way the driver hands one
// (component/pipelinerun/driver.go): a step with a Postgres sidecar, a step
// split over two shards, and a step needing a display, which only one of the
// owner's machines could offer. The check runs the seam composes -- for this
// run, and for a fork's pull request it refuses -- are then held to recorded
// GitHub fixtures.
//
// BUILT WITH TWO TAGS, `clustere2e` and `agent`. The executor's fleet half
// (integrations/pipelinesteps/fleet.go) is in the agent's build only, as the
// dispatcher it drives is, and this leg runs that half for real rather than a
// stand-in for it. With `clustere2e` alone the file is not compiled, so the
// test does not exist: the CI leg asks for a PASS line, not merely for no
// failure, and ci.yml's build-clustere2e lane compiles the package under both
// tag sets.
//
// WHAT IS REAL, AND WHAT STANDS IN (ruling R41). A deployed engine cannot be
// pointed at a fake GitHub without configuration the product does not have --
// the API base is GitHub's own, and the clone URL is composed as
// https://github.com/<owner>/<name>.git -- and this leg adds none. So:
//
//   - REAL, against the cluster: the agent's Executor and its fleet half
//     (pipelinesteps.NewFleet, with its own classification), and two workbench
//     replicas' Runners ("workbench-a", "workbench-b"), each behind the real
//     integrations/workbench ForwardHandler, joined by an in-process forward
//     transport (substrateMesh, the shape of integrations/pipelinesteps'
//     executor_hop_test.go). The Runners talk to the API server AS THE ENGINE:
//     with a token for memql/memql-engine, what `kubectl create token` asks
//     for, so every Job, Secret, pod and log goes through the DEPLOYED Role,
//     RoleBinding, quota, LimitRange, cache claim and NetworkPolicy of
//     deploy/k8s/components/pipelines; the namespace and the clone image are
//     read from the deployed memql-pipelines ConfigMap. The repository is this
//     one, public, cloned anonymously at a commit of main, and every image is
//     public and pinned by digest, because a step's Job pulls with no
//     credential.
//   - REAL, as pure functions: the seam's manifest reader, Validate, Compile,
//     ClassifyDelivery and CheckOutput, and component/pipelinerun's check-run
//     composition (ReportFor, ComposeCheckRun) and Trigger, which opens the
//     fork's run over rows held in memory.
//   - STANDING IN: GitHub, as an httptest server the real githubapp client
//     writes check runs to -- the bodies it receives are what the fixtures
//     record; the Library, as a recording LibraryStore; the clone token
//     minter, which answers installation 0 with no token, as app/'s does; and
//     the agent's dispatcher (noMachineDispatcher), which answers exactly as
//     integrations/agent/worker's Dispatcher answers an owner with no machine
//     at all, and records what the fleet half asked it for.
//
// WHAT IT DOES NOT COVER: the deployed agent and workbench wiring -- the app/
// adapter, the NodeService streams, the dispatcher itself -- which unit and
// hop tests cover (integrations/pipelinesteps' executor_hop_test.go and
// fleet_test.go, integrations/agent/worker, app/). What this leg adds is what
// no fake can stand in for: a real API server, kubelet, image pull, clone and
// network policy under the grant the product deploys.
//
// RUN, against a cluster made for it. The test creates Jobs and Secrets in the
// cluster's memql-pipelines namespace and holds slots of its ceiling while it
// runs, so it belongs on a throwaway cluster -- or on the one
// install-cluster-e2e.yml's `pipelines` leg installs from source -- never on a
// development cluster somebody else is using:
//
//	k3d cluster create pipelines-e2e --kubeconfig-update-default=false --kubeconfig-switch-context=false
//	k3d kubeconfig get pipelines-e2e > /tmp/pipelines-e2e.kubeconfig
//	# apply what the test needs from `kubectl kustomize deploy/k8s/overlays/local`:
//	# the memql and memql-pipelines namespaces, every object in memql-pipelines,
//	# memql/memql-engine and the memql/memql-pipelines ConfigMap
//	MEMQL_E2E_KUBECONFIG=/tmp/pipelines-e2e.kubeconfig MEMQL_E2E_KUBECONTEXT=k3d-pipelines-e2e \
//	  GOWORK=off go test -tags clustere2e,agent -count=1 -timeout=30m -run TestPipelinesSubstrate ./test/clustere2e/
//
// MEMQL_E2E_KUBECONFIG names the kubeconfig when it is not $KUBECONFIG or
// ~/.kube/config. With no context named the test SKIPS: it never runs against
// whichever context happens to be current. Once a context is named, anything
// missing -- the ConfigMap, the engine's ServiceAccount, the grant -- FAILS it.
// Two subtests read no cluster and run before it is reached, so they run --
// and can fail -- with no context named: report-mapping and normalization,
// which hold the test's own restatements of the driver's step report and of
// the fixtures' normalization.
//
// THE FIXTURES. `-update` rewrites the recorded check runs
// (testdata/pipelines/check_run.json and fork_check_run.json) from the run,
// normalized; the committed ones are what a real run produced.
// testdata/pipelines/fork_delivery.json is not recorded by the test: it is
// GitHub's documented pull_request.opened payload (octokit/webhooks,
// payload-examples/api.github.com/pull_request/opened.payload.json, the
// Codertocat/Hello-World sample) with its head moved to a fork --
// head.repo stranger/Hello-World with fork true, the head's user, the pull
// request's author and the sender with it -- every numeric id but the
// installation's (which ClassifyDelivery requires) set to 0, every node_id
// SCRUBBED, and the user ids in the avatar URLs set to 0.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

var updatePipelinesFixtures = flag.Bool("update", false,
	"rewrite testdata/pipelines/check_run.json and fork_check_run.json from this run of TestPipelinesSubstrate")

const (
	// The engine's identity and the mesh namespace it runs in: the subject
	// deploy/k8s/components/pipelines/rbac.yaml binds the runner's Role to,
	// and where config.yaml's ConfigMap lands.
	substrateMeshNamespace = "memql"
	substrateRunnerAccount = "memql-engine-workbench"
	substrateConfigMap     = "memql-pipelines"

	// The repository every step clones: this one, public, at a commit of
	// main. Installation 0 is an anonymous clone; the minter answers it with
	// no token and the clone script then sends no credential at all.
	substrateOwner = "znasllc-io"
	substrateRepo  = "memql"
	substrateSHA   = "1b0c170e03f50a91e05caf149fa93c989c255c38"

	// The OS origin a check run's details link is built on.
	substrateOSOrigin = "https://os.example.com"
	// The agent node the executor forwards from.
	substrateAgent = "agent-e2e"
	// The id GitHub gave the run's check run when it opened: the run's final
	// report MOVES it, as the driver's does (checkrun.go publishFinal).
	substrateCheckRunID = 4242
	// The owner every step runs for: the Library files and the cache
	// directory are this owner's.
	substrateOwnerUser = "pipelines-e2e-owner"
	// The step secret the database step reads its password from. The secret
	// store resolves it to the sidecar's own password (manifest.yaml).
	substrateSecret = "PGPASSWORD"

	// substrateRunBudget bounds the whole run: three stages, the first of
	// which pulls the images and every step of which clones the repository.
	substrateRunBudget = 20 * time.Minute
	// substrateCleanupPatience is how long the acked steps' Jobs, Secrets and
	// pods may take to go: background propagation, and a pod's grace period.
	substrateCleanupPatience = 3 * time.Minute
)

// substrateWorkbenches are the two workbench replicas the steps are spread
// over: the Job, not a replica's memory, is what a step's state lives in.
var substrateWorkbenches = []string{"workbench-a", "workbench-b"}

// substratePackages is the synthetic Go tree's packages: what Compile is given
// to select from, and what the sharded step's two Jobs must print between
// them.
var substratePackages = []string{"alpha", "beta", "delta", "gamma"}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestPipelinesSubstrate(t *testing.T) {
	// The test's own restatements of the seam, held first: they read no
	// cluster, so they run -- and can fail -- before the skip below.
	t.Run("report-mapping", testSubstrateReportMapping)
	t.Run("normalization", testSubstrateNormalization)

	cluster := connectSubstrateCluster(t)

	ctx, cancel := context.WithTimeout(context.Background(), substrateRunBudget)
	defer cancel()

	configFor := deployedRunnerConfig(ctx, t, cluster)
	namespace := configFor(substrateAgent).Namespace
	engine := engineClusterAPI(ctx, t, cluster, namespace)
	compiled := compileSubstratePlan(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	library := &recordingLibrary{}
	tokens := &anonymousCloneTokens{}
	dispatcher := &noMachineDispatcher{}
	mesh := &substrateMesh{inflight: map[string]chan *nodev1.WorkbenchForwardResponse{}, stepServedBy: map[string]string{}}
	for _, node := range substrateWorkbenches {
		cfg := configFor(node)
		handler := workbench.NewForwardHandler(nil, logger)
		handler.SetPipelineRunner(runnerAdapter{runner: pipelinesteps.NewRunner(cfg, pipelinesteps.NewKube(engine, cfg.Namespace), nil, library, tokens)})
		mesh.replicas = append(mesh.replicas, &substrateReplica{node: node, handler: handler})
	}
	fleet := pipelinesteps.NewFleet(configFor(substrateAgent), dispatcher, library, tokens, nil, logger)
	executor := pipelinesteps.NewExecutor(configFor(substrateAgent), mesh, fleet, logger)

	run := newSubstrateRun(compiled)
	selector := runSelector(run.runID)
	t.Cleanup(func() { sweepRunObjects(t, cluster, namespace, selector) })

	watch := watchRunObjects(ctx, cluster, namespace, selector)
	run.drive(ctx, executor, configFor(substrateAgent).RunCeiling)
	seen := watch.stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the run did not end within %s (%v); its steps as they stood:\n%s", substrateRunBudget, err, run.summary())
	}
	t.Logf("run %s on %s, in namespace %s:\n%s", run.runID, cluster.server, namespace, run.summary())

	// THE LEG'S PREMISES. Each is what makes the assertions below about the
	// cluster rather than about a run that never reached it.
	asked := tokens.asked()
	if len(asked) == 0 {
		t.Errorf("no clone token was ever asked for: no step's Job was created")
	}
	for _, installation := range asked {
		if installation != 0 {
			t.Errorf("a clone token was asked for installation %d; this leg clones a public repository anonymously (installation 0)", installation)
		}
	}
	served := mesh.servedSteps()
	for _, node := range substrateWorkbenches {
		if served[node] == 0 {
			t.Errorf("workbench replica %s ran no step (step forwards by replica: %v): the leg drives two Runners, so the Job "+
				"one creates is the state the other reads and acks", node, served)
		}
	}

	t.Run("postgres-sidecar", func(t *testing.T) {
		// The native sidecar is up before the step: psql connects once, over
		// TCP, with no retry, so a step that reached the server at all reached
		// it because the sidecar's startup probe -- the manifest's `ready`,
		// which the runner turns into one -- held the step back until the
		// server answered. The image's entrypoint runs under the sidecar's
		// allowPrivilegeEscalation: false, and the password reaches psql
		// through the step's environment, from the step's Secret.
		s := run.mustStep(t, pl.StepKey("database", "psql"))
		svc, ok := s.step.Services["postgres"]
		if !ok || strings.TrimSpace(svc.Ready) == "" {
			t.Errorf("the compiled step carries no postgres service with a ready check (%+v): there is no startup probe to hold the step back", s.step.Services)
		}
		// The native-sidecar shape as the API server held it while the Job
		// lived -- not the compiled Ready field.
		if job, read := seen.specs[s.step.Key]; !read {
			t.Errorf("the step's Job was never read back while it lived (Jobs seen: %v)", seen.jobs)
		} else {
			assertNativeSidecar(t, job, pipelinesteps.ServicePrefix+"postgres", svc.Ready)
		}
		res := s.mustResult(t)
		if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 {
			t.Fatalf("the step ended %s, exit %d, failure %+v; want succeeded, exit 0. Its output:\n%s",
				res.Status, res.ExitCode, res.Failure, res.LogTail)
		}
		if lines := outputLines(res.LogTail); !slices.Contains(lines, "1") {
			t.Errorf("the step's output %q has no line `1`, which `select 1` prints", lines)
		}
		wantJob := pipelinesteps.JobName(run.runID, s.step.Key, 1)
		if res.Where.Surface != "cluster" || res.Where.JobName != wantJob || !slices.Contains(substrateWorkbenches, res.Where.NodeID) {
			t.Errorf("the step ran at %+v; want the cluster, Job %s, on one of %v", res.Where, wantJob, substrateWorkbenches)
		}
	})

	t.Run("sharded-step", func(t *testing.T) {
		// shards: 2 over the tree's packages: two Jobs, each told its own
		// slice in MEMQL_PACKAGES, which together are every package Compile
		// was given, each exactly once.
		var shards []*substrateStep
		for _, s := range run.steps {
			if s.step.Stage == "shards" && s.step.Name == "packages" {
				shards = append(shards, s)
			}
		}
		if len(shards) != 2 {
			t.Fatalf("the packages step compiled to %d steps (%v); shards: 2 over %d packages must make two",
				len(shards), stepKeys(shards), len(compiled.packages))
		}
		printedBy := map[string]string{}
		for i, s := range shards {
			wantKey := fmt.Sprintf("%s#%d", pl.StepKey("shards", "packages"), i+1)
			if s.step.Key != wantKey || s.step.Shard != (pl.ShardRef{Index: i + 1, Count: 2}) {
				t.Errorf("shard %d is %s %+v; want %s, shard %d of 2", i+1, s.step.Key, s.step.Shard, wantKey, i+1)
			}
			res := s.mustResult(t)
			if res.Status != pl.OutcomeSucceeded {
				t.Errorf("shard %s ended %s, failure %+v; want succeeded. Its output:\n%s", s.step.Key, res.Status, res.Failure, res.LogTail)
				continue
			}
			printed := printedPackages(res.LogTail)
			if !slices.Equal(printed, s.step.Packages) {
				t.Errorf("shard %s printed %v; its MEMQL_PACKAGES is %v", s.step.Key, printed, s.step.Packages)
			}
			for _, pkg := range printed {
				switch other, twice := printedBy[pkg]; {
				case twice && other == s.step.Key:
					t.Errorf("package %s was printed twice by %s", pkg, other)
				case twice:
					t.Errorf("package %s was printed by both %s and %s: the shards overlap", pkg, other, s.step.Key)
				}
				printedBy[pkg] = s.step.Key
			}
		}
		union := slices.Sorted(maps.Keys(printedBy))
		if !slices.Equal(union, compiled.packages) {
			t.Errorf("the shards printed %v between them; Compile was given %v", union, compiled.packages)
		}
	})

	t.Run("fleet-refused", func(t *testing.T) {
		// A step that needs a display, in a pipeline that consented to the
		// fleet, is the fleet's: the executor hands it to its fleet half, which
		// asks the agent's dispatcher for one of the owner's machines that
		// offers a display and allows pipeline steps. With none, the
		// dispatcher's verdict is that nothing started, and the fleet half's
		// own classification makes that a refusal -- never handed to a
		// workbench, never a Job.
		s := run.mustStep(t, pl.StepKey("fleet", "display"))
		res := s.mustResult(t)
		if res.Status != pl.OutcomeRefused || res.ExitCode != -1 || res.Failure == nil || res.Failure.Code != pl.CodeNoMachineForNeed {
			t.Errorf("the step ended %s, exit %d, failure %+v; want refused, exit -1, %s", res.Status, res.ExitCode, res.Failure, pl.CodeNoMachineForNeed)
		}
		// The sentence quotes the dispatcher's code and words: it is the fleet
		// half's reading of the verdict, not a sentence this test supplied.
		if res.Failure != nil && !strings.Contains(res.Failure.Message, "(no_worker_available): no machines are paired to this account") {
			t.Errorf("the refusal reads %q; want the fleet half's sentence quoting the dispatcher's no_worker_available", res.Failure.Message)
		}
		if res.Where.Surface != "fleet" {
			t.Errorf("the step reports it ran at %+v; want the fleet", res.Where)
		}
		asked := dispatcher.asked()
		if len(asked) != 1 {
			t.Fatalf("the dispatcher was asked %d times (%v); want exactly once, for %s", len(asked), dispatchedKeys(asked), s.step.Key)
		}
		// What the fleet half asks for, pinned by value: a display-capable
		// machine carries display=true, and fleet labels match exactly.
		got := asked[0]
		wantLabels := map[string]string{"display": "true", worker.PipelinesLabel: worker.PipelinesAllowed}
		switch r := got.req; {
		case r.Tool != "workerHost" || r.Action != worker.PipelineStepAction || r.Purpose != worker.PurposePipeline:
			t.Errorf("the dispatch was %s.%s under purpose %q; want workerHost.%s under %q",
				r.Tool, r.Action, r.Purpose, worker.PipelineStepAction, worker.PurposePipeline)
		case r.StepId != s.step.Key || r.RunId != run.workRunID || r.CorrelationId != run.runID || r.OwnerUserId != substrateOwnerUser:
			t.Errorf("the dispatch named step %q, work run %q, run %q, owner %q; want %s, %s, %s, %s",
				r.StepId, r.RunId, r.CorrelationId, r.OwnerUserId, s.step.Key, run.workRunID, run.runID, substrateOwnerUser)
		case r.AgentId != "":
			t.Errorf("the dispatch named agent %q; a pipeline step names none", r.AgentId)
		case !maps.Equal(r.RequireLabels, wantLabels):
			t.Errorf("the dispatch required labels %v; want %v", r.RequireLabels, wantLabels)
		case !got.internal:
			t.Error("the dispatch carried no internal origin; the dispatcher admits the pipeline purpose only from the engine's own Go")
		}
		if forwards := mesh.stepForwards(s.step.Key); len(forwards) > 0 {
			t.Errorf("the step was forwarded to workbench replicas %v; a fleet step never reaches one", forwards)
		}
		// The watch is what says no Job of the step EVER existed: a Job that
		// ran and was acked is gone by now, so a read alone cannot.
		if slices.Contains(seen.stepKeys, s.step.Key) {
			t.Errorf("a Job for %s existed during the run; a fleet step has none", s.step.Key)
		}
		job := pipelinesteps.JobName(run.runID, s.step.Key, 1)
		if _, status, err := cluster.call(context.Background(), http.MethodGet,
			"apis/batch/v1/namespaces/"+namespace+"/jobs/"+job, "", nil); err != nil || status != http.StatusNotFound {
			t.Errorf("reading Job %s, the fleet step's name, answered status %d (%v); want 404", job, status, err)
		}
	})

	t.Run("fork-refused", func(t *testing.T) {
		// A pull request whose head lives in another repository runs somebody
		// else's code: the seam refuses it at the opening -- a run concluded
		// refused, never queued -- and says so in a FAILING check run.
		delivery := readSubstrateFixture(t, "fork_delivery.json")
		trigger, ignored, err := pl.ClassifyDelivery("pull_request", delivery)
		if err != nil || ignored != nil {
			t.Fatalf("ClassifyDelivery(fork_delivery.json) = ignored %+v, error %v; want a pull request it acts on", ignored, err)
		}
		if !trigger.Fork || trigger.Event != pl.EventPullRequest || trigger.HeadRepository == "" ||
			strings.EqualFold(trigger.HeadRepository, trigger.Repository) {
			t.Fatalf("ClassifyDelivery read the delivery as %+v; want a pull request from a fork", trigger)
		}
		opened, body := openForkRun(t, logger, delivery, trigger)
		if opened.Status != pipelinerun.StatusCompleted || opened.Conclusion != pipelinerun.ConclusionRefused ||
			opened.RefusalCode != pl.CodeForkRefused {
			t.Errorf("the fork's run opened %s / %s / %q; want completed, refused, %s",
				opened.Status, opened.Conclusion, opened.RefusalCode, pl.CodeForkRefused)
		}
		got, err := normalizedCheckRun(body, opened.ID, trigger.SHA)
		if err != nil {
			t.Fatalf("the fork's check run is not JSON: %v\n%s", err, body)
		}
		matchSubstrateFixture(t, "fork_check_run.json", got)
	})

	t.Run("check-run", func(t *testing.T) {
		// The run's final check run, as the seam composes it: CheckOutput
		// over the steps' real results, wrapped by ComposeCheckRun, as GitHub
		// receives it when the driver moves the check run the opening created.
		got, err := normalizedCheckRun(run.checkRunBody(t), run.runID, substrateSHA)
		if err != nil {
			t.Fatalf("the run's check run is not JSON: %v", err)
		}
		matchSubstrateFixture(t, "check_run.json", got)
	})

	t.Run("library", func(t *testing.T) {
		// Every cluster step's archived log reaches its owner's Library,
		// bound to the work run and the step, and the database step's one
		// declared artifact with its content. A fleet step that never ran
		// stores nothing.
		files := library.all()
		clusterSteps := 0
		for _, s := range run.steps {
			if !s.sent || len(s.step.Needs) > 0 {
				continue
			}
			clusterSteps++
			res := s.mustResult(t)
			logName := strings.ReplaceAll(s.step.Key, "#", "-") + ".log"
			logs := filesNamed(files, s.step.Key, logName)
			if len(logs) != 1 {
				t.Errorf("%s stored %d files named %s; want its one log", s.step.Key, len(logs), logName)
				continue
			}
			log := logs[0]
			if !strings.HasPrefix(log.MimeType, "text/plain") || log.OwnerUserID != substrateOwnerUser || log.WorkRunID != run.workRunID {
				t.Errorf("%s's log was stored as %s for owner %q, work run %q; want text/plain for %s, %s",
					s.step.Key, log.MimeType, log.OwnerUserID, log.WorkRunID, substrateOwnerUser, run.workRunID)
			}
			if res.LogFileID == "" || res.LogFileID != log.id {
				t.Errorf("%s's result names log file %q; the Library stored %q", s.step.Key, res.LogFileID, log.id)
			}
			// The archive is the whole step: the clone's last words name the
			// commit it fetched, anonymously, before the step ran.
			if !bytes.Contains(log.Bytes, []byte("memql: checked out "+substrateSHA)) {
				t.Errorf("%s's log does not say the clone checked out %s:\n%s", s.step.Key, substrateSHA, log.Bytes)
			}
		}
		if clusterSteps == 0 {
			t.Fatal("no cluster step ran, so the Library has nothing to hold")
		}

		psql := run.mustStep(t, pl.StepKey("database", "psql"))
		res := psql.mustResult(t)
		// The runner names an artifact by its path, every slash a double
		// underscore, and types it by its extension through the mime table of
		// the node it runs on -- this process -- which has no type for .txt
		// unless the system's table names one.
		wantMIME := cmp.Or(mime.TypeByExtension(".txt"), "application/octet-stream")
		artifacts := filesNamed(files, psql.step.Key, "out__select-1.txt")
		switch {
		case len(artifacts) != 1:
			t.Errorf("the declared artifact out/select-1.txt was stored %d times (notes: %+v); want once", len(artifacts), res.Notes)
		case string(artifacts[0].Bytes) != "1\n" || artifacts[0].MimeType != wantMIME:
			t.Errorf("the artifact was stored as %s holding %q; want %s holding %q", artifacts[0].MimeType, artifacts[0].Bytes, wantMIME, "1\n")
		case !slices.Equal(res.ArtifactFileIDs, []string{artifacts[0].id}):
			t.Errorf("the step's result names artifacts %v; the Library stored %q", res.ArtifactFileIDs, artifacts[0].id)
		}
		if len(res.Notes) > 0 {
			t.Errorf("the database step carries notes %+v; its log and its one artifact were all stored", res.Notes)
		}
		if fleetFiles := filesFor(files, pl.StepKey("fleet", "display")); len(fleetFiles) > 0 {
			t.Errorf("the fleet step, which never ran, stored %d files", len(fleetFiles))
		}
		if want := clusterSteps + 1; len(files) != want {
			t.Errorf("the Library holds %d files (%s); want %d: one log per cluster step and the one artifact", len(files), fileNames(files), want)
		}
	})

	t.Run("isolation", func(t *testing.T) {
		assertIsolationProof(t, run)
	})

	t.Run("cleanup", func(t *testing.T) {
		// After the ack, nothing of the run is left: not its Jobs, which hold
		// slots of the ceiling, and not its Secrets, which hold what a step
		// was given. The positive first: the label selector found the run's
		// objects while they lived, so an empty answer now is not a selector
		// that matches nothing.
		if len(seen.jobs) == 0 || len(seen.secrets) == 0 || len(seen.pods) == 0 {
			t.Fatalf("while the run lived, %q matched %d Jobs, %d Secrets and %d pods; the selector cannot vouch for an empty answer",
				selector, len(seen.jobs), len(seen.secrets), len(seen.pods))
		}
		left, err := awaitRunObjectsGone(cluster, namespace, selector, substrateCleanupPatience)
		if err != nil {
			t.Fatalf("listing the run's objects: %v", err)
		}
		if !left.empty() {
			t.Errorf("%s after the steps were acked, the run still has %s in %s (selector %q)",
				substrateCleanupPatience, left, namespace, selector)
		}
	})
}

// assertIsolationProof is where the runner's isolation proof (ruling R12) is
// held to the cluster. k3s enforces the namespace's NetworkPolicy, so no step
// may be refused pipeline_isolation_unenforced here, nor carry it as a note:
// the proof a replica makes before its first Job must pass. The proof's own
// verdict is the runner's to expose (Runner.Isolation, which comes with the
// proof), and this function is where it is asserted beside these checks.
func assertIsolationProof(t *testing.T, run *substrateRun) {
	t.Helper()
	for _, s := range run.steps {
		if !s.answered {
			continue
		}
		if f := s.result.Failure; f != nil && f.Code == pl.CodeIsolationUnenforced {
			t.Errorf("%s was refused %s: %s", s.step.Key, f.Code, f.Message)
		}
		for _, note := range s.result.Notes {
			if note.Code == pl.CodeIsolationUnenforced {
				t.Errorf("%s carries the note %s: %s", s.step.Key, note.Code, note.Message)
			}
		}
	}
}

// assertNativeSidecar holds a step's Job, as the API server held it, to the
// native-sidecar shape: the service is an init container that runs for the
// pod's life (restartPolicy Always), behind a startup probe that runs the
// manifest's `ready` -- through the runner's $-escape (ruling R25), so the
// container receives it as written -- started after the clone, and with no
// privilege to gain; the step is the pod's one container. The kubelet starts
// the step only once that probe passes, which is what the one-shot psql relies
// on.
func assertNativeSidecar(t *testing.T, job pipelinesteps.Job, sidecar, ready string) {
	t.Helper()
	pod := job.Spec.Template.Spec
	var inits []string
	for _, c := range pod.InitContainers {
		inits = append(inits, c.Name)
	}
	at := slices.Index(inits, sidecar)
	if at < 0 {
		t.Errorf("the Job's init containers are %v; want the sidecar %s among them", inits, sidecar)
		return
	}
	c := pod.InitContainers[at]
	if c.RestartPolicy == nil || *c.RestartPolicy != "Always" {
		t.Errorf("%s has restartPolicy %v; a native sidecar runs for the pod's life, restartPolicy Always", sidecar, c.RestartPolicy)
	}
	wantProbe := []string{"/bin/sh", "-c", strings.ReplaceAll(ready, "$", "$$")}
	if c.StartupProbe == nil || c.StartupProbe.Exec == nil || !slices.Equal(c.StartupProbe.Exec.Command, wantProbe) {
		t.Errorf("%s's startup probe is %+v; want exec %q", sidecar, c.StartupProbe, wantProbe)
	}
	if clone := slices.Index(inits, pipelinesteps.ContainerClone); clone < 0 || clone > at {
		t.Errorf("the init containers run in the order %v; the sidecar starts after the clone", inits)
	}
	if sc := c.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("%s may gain privileges (%+v); a service runs with allowPrivilegeEscalation false", sidecar, sc)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != pipelinesteps.ContainerStep {
		t.Errorf("the pod's containers are %+v; want the step alone", pod.Containers)
	}
}

// testSubstrateReportMapping holds report() to the driver's mapping case by
// case: receiptFor's, then report()'s masking (component/pipelinerun/driver.go
// and checkrun.go). The sentences are the driver's own -- exitMessage,
// receiptFor's default, the executor-error receipt -- restated here because
// none of them is exported. A refused outcome is a FAILED work step there, so
// a refusal with no message reads the exit sentence as a failure does.
func testSubstrateReportMapping(t *testing.T) {
	const secret = "e2e-secret-value"
	values := []string{secret}
	step := pl.Step{Key: "fleet.display", Stage: "fleet", Name: "display"}
	const waited = 1500 * time.Millisecond
	answered := func(res pl.StepResult) substrateStep {
		return substrateStep{step: step, sent: true, answered: true, result: res, elapsed: waited}
	}
	cases := []struct {
		name string
		s    substrateStep
		want pl.StepReport
	}{{
		name: "refused with no message: the exit sentence",
		s: answered(pl.StepResult{Status: pl.OutcomeRefused, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeNoMachineForNeed}}),
		want: pl.StepReport{Status: pipelinerun.StepRefused, Code: pl.CodeNoMachineForNeed,
			Message: "The step's command did not run.", DurationMs: 1500},
	}, {
		name: "refused with no failure at all",
		s:    answered(pl.StepResult{Status: pl.OutcomeRefused, ExitCode: -1}),
		want: pl.StepReport{Status: pipelinerun.StepRefused, Message: "The step's command did not run.", DurationMs: 1500},
	}, {
		name: "failed with no sentence: its exit status",
		s:    answered(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 3}),
		want: pl.StepReport{Status: pipelinerun.StepFailed, Message: "The step's command exited with status 3.", DurationMs: 1500},
	}, {
		name: "failed with its own sentence, trimmed and masked",
		s: answered(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 1,
			Failure: &pl.Failure{Code: pl.CodeServiceFailed, Message: "  password " + secret + " refused \n"}}),
		want: pl.StepReport{Status: pipelinerun.StepFailed, Code: pl.CodeServiceFailed,
			Message: "password *** refused", DurationMs: 1500},
	}, {
		name: "cancelled with no message: none, a cancelled work step is not a failed one",
		s:    answered(pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1}),
		want: pl.StepReport{Status: pipelinerun.StepCancelled, DurationMs: 1500},
	}, {
		name: "succeeded, the runner's own times and a masked log tail",
		s: answered(pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: 0,
			StartedAt: "2026-10-04T12:00:00Z", FinishedAt: "2026-10-04T12:00:45Z", LogTail: "the value is " + secret + "\n"}),
		want: pl.StepReport{Status: pipelinerun.StepSucceeded, DurationMs: 45000, LogTail: "the value is ***\n"},
	}, {
		name: "an outcome the driver does not know",
		s:    answered(pl.StepResult{Status: "exploded", ExitCode: 0}),
		want: pl.StepReport{Status: pipelinerun.StepFailed, Code: pl.CodeExecutorError,
			Message: `The runner answered an outcome this driver does not know ("exploded").`, DurationMs: 1500},
	}, {
		name: "no answer it could read: the executor's error, masked",
		s:    substrateStep{step: step, sent: true, answered: true, err: errors.New("stream reset carrying " + secret)},
		want: pl.StepReport{Status: pipelinerun.StepFailed, Code: pl.CodeExecutorError,
			Message: "The runner could not report how the step ended: stream reset carrying ***"},
	}}
	for _, c := range cases {
		c.want.Key, c.want.Stage, c.want.Name = step.Key, step.Stage, step.Name
		if got := c.s.report(values); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// testSubstrateNormalization holds normalizedCheckRun to what it may replace:
// the run's id, the commit -- the whole SHA and its first seven, each under a
// placeholder of its own -- the times, and the table's and the title's
// durations, in every form formatDuration writes; a time nobody measured ("-")
// and every other byte stay.
func testSubstrateNormalization(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const runID = "v1:pipelines:run:e2e-normalization"
	body := func(commit string) []byte {
		return encodeJSON(map[string]any{
			"name":         "MemQL / pipelines-e2e",
			"external_id":  runID,
			"details_url":  substrateOSOrigin + "/?pipelineRun=" + runID,
			"head_sha":     sha,
			"status":       "completed",
			"conclusion":   "success",
			"started_at":   "2026-10-04T12:00:00Z",
			"completed_at": "2026-10-04T13:02:05Z",
			"output": map[string]any{
				"title": "Passed: 5 stages in 1h 02m 05s",
				"summary": "Mode full · Push to the default branch · Commit " + commit + "\n\n" +
					"| Stage | Status | Steps | Time |\n| --- | --- | --- | --- |\n" +
					"| a | Passed | 1 passed | 45s |\n" +
					"| b | Passed | 2 passed | 1m 02s |\n" +
					"| c | Passed | 1 passed | 1h 02m 05s |\n" +
					"| d | Passed | 1 passed | <1s |\n" +
					"| e | Skipped | 1 skipped | - |\n",
			},
		})
	}
	const want = `{
  "completed_at": "<timestamp>",
  "conclusion": "success",
  "details_url": "https://os.example.com/?pipelineRun=<run-id>",
  "external_id": "<run-id>",
  "head_sha": "<sha>",
  "name": "MemQL / pipelines-e2e",
  "output": {
    "summary": "Mode full · Push to the default branch · Commit <sha7>\n\n| Stage | Status | Steps | Time |\n| --- | --- | --- | --- |\n| a | Passed | 1 passed | <duration> |\n| b | Passed | 2 passed | <duration> |\n| c | Passed | 1 passed | <duration> |\n| d | Passed | 1 passed | <duration> |\n| e | Skipped | 1 skipped | - |\n",
    "title": "Passed: 5 stages in <duration>"
  },
  "started_at": "<timestamp>",
  "status": "completed"
}
`
	short, err := normalizedCheckRun(body(sha[:7]), runID, sha)
	if err != nil {
		t.Fatalf("normalizing: %v", err)
	}
	if string(short) != want {
		t.Errorf("normalized:\n%s\nwant:\n%s", short, want)
	}
	long, err := normalizedCheckRun(body(sha), runID, sha)
	if err != nil {
		t.Fatalf("normalizing: %v", err)
	}
	if bytes.Equal(long, short) || !strings.Contains(string(long), "Commit <sha>\\n") {
		t.Errorf("a summary naming the whole commit normalizes as %s; want `Commit <sha>`, apart from the short form's `Commit <sha7>`", long)
	}
}

// ---------------------------------------------------------------------------
// The cluster, as the kubeconfig names it
// ---------------------------------------------------------------------------

// substrateCluster is the cluster under test.
type substrateCluster struct {
	server string
	// operator is the kubeconfig user's client: the test's own eyes, which
	// read the deployed ConfigMap, ask for the engine's token and watch what
	// the runners leave behind. The runners never use it.
	operator *http.Client
	// roots trusts the API server's certificate authority and nothing else.
	// The engine's client is built on it with no client certificate, so its
	// requests carry the engine's token and nothing that could outrank it.
	roots *x509.CertPool
}

// connectSubstrateCluster skips without a named context and fails on anything
// after: a context named on purpose is a cluster this leg must reach.
func connectSubstrateCluster(t *testing.T) *substrateCluster {
	t.Helper()
	contextName := strings.TrimSpace(os.Getenv("MEMQL_E2E_KUBECONTEXT"))
	if contextName == "" {
		t.Skip("MEMQL_E2E_KUBECONTEXT not set -- this leg runs steps as Jobs in a cluster's memql-pipelines namespace, " +
			"so it runs only against a cluster named on purpose: set it to the kube context of a cluster that runs the " +
			"pipelines component (install-cluster-e2e.yml's pipelines leg does), and MEMQL_E2E_KUBECONFIG when that " +
			"context's kubeconfig is not $KUBECONFIG or ~/.kube/config")
	}
	path := substrateKubeconfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MEMQL_E2E_KUBECONTEXT names %q, and the kubeconfig %s cannot be read: %v", contextName, path, err)
	}
	c, err := clusterFromKubeconfig(raw, filepath.Dir(path), contextName)
	if err != nil {
		t.Fatalf("MEMQL_E2E_KUBECONTEXT names %q in %s: %v", contextName, path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if body, status, err := c.call(ctx, http.MethodGet, "version", "", nil); err != nil || status != http.StatusOK {
		t.Fatalf("the API server of %q (%s) did not answer /version: status %d, %v %s", contextName, c.server, status, err, body)
	}
	return c
}

// substrateKubeconfigPath is MEMQL_E2E_KUBECONFIG, else the first file
// $KUBECONFIG names, else ~/.kube/config.
func substrateKubeconfigPath() string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_E2E_KUBECONFIG")); p != "" {
		return p
	}
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

// clusterFromKubeconfig reads the server, its certificate authority and the
// user's credential of one context. A client certificate or a token is read;
// an exec or auth-provider plugin is not -- this leg drives a k3d cluster,
// whose kubeconfig carries a client certificate.
func clusterFromKubeconfig(raw []byte, dir, contextName string) (*substrateCluster, error) {
	var kc struct {
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server                   string `yaml:"server"`
				CertificateAuthorityData string `yaml:"certificate-authority-data"`
				CertificateAuthority     string `yaml:"certificate-authority"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
				User    string `yaml:"user"`
			} `yaml:"context"`
		} `yaml:"contexts"`
		Users []struct {
			Name string `yaml:"name"`
			User struct {
				ClientCertificateData string `yaml:"client-certificate-data"`
				ClientKeyData         string `yaml:"client-key-data"`
				ClientCertificate     string `yaml:"client-certificate"`
				ClientKey             string `yaml:"client-key"`
				Token                 string `yaml:"token"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("the kubeconfig does not parse: %w", err)
	}
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == contextName {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	if clusterName == "" {
		return nil, errors.New("no context of that name")
	}

	// A file named relative to the kubeconfig is relative to its directory.
	read := func(data, file string) ([]byte, error) {
		if strings.TrimSpace(data) != "" {
			return base64.StdEncoding.DecodeString(strings.TrimSpace(data))
		}
		if strings.TrimSpace(file) == "" {
			return nil, nil
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
		return os.ReadFile(file)
	}

	c := &substrateCluster{}
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		c.server = strings.TrimSuffix(strings.TrimSpace(cl.Cluster.Server), "/")
		pem, err := read(cl.Cluster.CertificateAuthorityData, cl.Cluster.CertificateAuthority)
		if err != nil {
			return nil, fmt.Errorf("cluster %q's certificate authority cannot be read: %w", clusterName, err)
		}
		if len(pem) == 0 {
			return nil, fmt.Errorf("cluster %q names no certificate authority, so the engine's client could not trust the API server", clusterName)
		}
		c.roots = x509.NewCertPool()
		if !c.roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cluster %q's certificate authority holds no certificate", clusterName)
		}
	}
	if c.server == "" {
		return nil, fmt.Errorf("the context names cluster %q, which has no server", clusterName)
	}

	tlsConfig := &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}
	var transport http.RoundTripper = &http.Transport{TLSClientConfig: tlsConfig}
	found := false
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		found = true
		certPEM, err := read(u.User.ClientCertificateData, u.User.ClientCertificate)
		if err != nil {
			return nil, fmt.Errorf("user %q's client certificate cannot be read: %w", userName, err)
		}
		keyPEM, err := read(u.User.ClientKeyData, u.User.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("user %q's client key cannot be read: %w", userName, err)
		}
		switch {
		case len(certPEM) > 0 && len(keyPEM) > 0:
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, fmt.Errorf("user %q's client certificate: %w", userName, err)
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		case strings.TrimSpace(u.User.Token) != "":
			transport = bearerTransport{token: strings.TrimSpace(u.User.Token), next: transport}
		default:
			return nil, fmt.Errorf("user %q carries neither a client certificate nor a token; an exec or auth-provider "+
				"plugin is not read by this leg", userName)
		}
	}
	if !found {
		return nil, fmt.Errorf("the context names user %q, which the kubeconfig does not hold", userName)
	}
	c.operator = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	return c, nil
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// call is one request as the operator. accept replaces application/json.
func (c *substrateCluster) call(ctx context.Context, method, path, accept string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+"/"+strings.TrimPrefix(path, "/"), rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", cmp.Or(accept, "application/json"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.operator.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return out, resp.StatusCode, err
}

// deployedRunnerConfig is the runner configuration a workbench replica named
// node reads: pipelinesteps.ConfigFromEnv over the deployed memql-pipelines
// ConfigMap -- the env the workbench Deployment's envFrom gives it -- and the
// replica's own MEMQL_NODE_ID.
func deployedRunnerConfig(ctx context.Context, t *testing.T, c *substrateCluster) func(node string) pipelinesteps.Config {
	t.Helper()
	path := "api/v1/namespaces/" + substrateMeshNamespace + "/configmaps/" + substrateConfigMap
	body, status, err := c.call(ctx, http.MethodGet, path, "", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("the deployed ConfigMap %s/%s could not be read (status %d, %v): the pipelines component "+
			"(deploy/k8s/components/pipelines) is not deployed on this cluster. %s",
			substrateMeshNamespace, substrateConfigMap, status, err, body)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		t.Fatalf("the ConfigMap %s/%s does not decode: %v", substrateMeshNamespace, substrateConfigMap, err)
	}
	for _, key := range []string{"MEMQL_PIPELINES_NAMESPACE", "MEMQL_PIPELINES_CLONE_IMAGE"} {
		if strings.TrimSpace(cm.Data[key]) == "" {
			t.Fatalf("the deployed ConfigMap %s/%s carries no %s; the workbench would run no pipeline step",
				substrateMeshNamespace, substrateConfigMap, key)
		}
	}
	return func(node string) pipelinesteps.Config {
		return pipelinesteps.ConfigFromEnv(func(key string) string {
			if key == "MEMQL_NODE_ID" {
				return node
			}
			return cm.Data[key]
		})
	}
}

// engineClusterAPI is the engine's client: the deployed ServiceAccount's token
// (a TokenRequest, as `kubectl create token` makes one) over the cluster's
// certificate authority, and nothing else. The grant is asked for before the
// run, so a missing Role reads as one rather than as every step failing.
func engineClusterAPI(ctx context.Context, t *testing.T, c *substrateCluster, namespace string) *deploycontrol.ClusterAPI {
	t.Helper()
	path := "api/v1/namespaces/" + substrateMeshNamespace + "/serviceaccounts/" + substrateRunnerAccount + "/token"
	request := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest","spec":{"expirationSeconds":3600}}`)
	body, status, err := c.call(ctx, http.MethodPost, path, "", request)
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
		t.Fatalf("no token for %s/%s could be had (status %d, %v): the runners run as the engine, whose ServiceAccount "+
			"the base deploys. %s", substrateMeshNamespace, substrateRunnerAccount, status, err, body)
	}
	var minted struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &minted); err != nil || strings.TrimSpace(minted.Status.Token) == "" {
		t.Fatalf("the TokenRequest for %s/%s answered no token (%v)", substrateMeshNamespace, substrateRunnerAccount, err)
	}
	api := deploycontrol.NewClusterAPIWith(c.server, minted.Status.Token, &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}},
	})
	// Asked with a call the runner makes -- a read of one Job by name -- of a
	// name no step's Job has (a step's is mp- and 24 hex): the API server
	// answers 404 to an identity that may read Jobs here and 403 to one that
	// may not. Not a list: the runner never lists Jobs, and its Role does not
	// let it.
	grantCheck := "apis/batch/v1/namespaces/" + namespace + "/jobs/pipelines-e2e-grant-check"
	if _, err := api.Do(ctx, http.MethodGet, grantCheck, "", nil); !deploycontrol.IsNotFound(err) {
		t.Fatalf("the engine (%s/%s) cannot read Jobs in %s (%v) -- the runner's Role and RoleBinding "+
			"(deploy/k8s/components/pipelines/rbac.yaml) are not deployed, or do not reach it",
			substrateMeshNamespace, substrateRunnerAccount, namespace, err)
	}
	return api
}

// ---------------------------------------------------------------------------
// The plan, compiled the way a run compiles it
// ---------------------------------------------------------------------------

// substratePlan is the manifest compiled as component/pipelinerun/tree.go's
// readPlan compiles a run's tree: read by component/packages' strict reader,
// validated, its Go packages chosen from the import graph of the tree it sits
// in, and compiled -- here for a push to the default branch, a full run, by an
// owner who consented to the fleet and allowed the one secret it names.
type substratePlan struct {
	name     string
	plan     pl.Plan
	packages []string // the import graph's packages: what Compile selected from
	secrets  map[string]string
}

func compileSubstratePlan(t *testing.T) substratePlan {
	t.Helper()
	tree := fstest.MapFS{
		pl.ManifestPath: {Data: readSubstrateFixture(t, "manifest.yaml")},
		"go.mod":        {Data: []byte("module example.com/e2e\n\ngo 1.26\n")},
	}
	for _, name := range substratePackages {
		tree[name+"/"+name+".go"] = &fstest.MapFile{Data: []byte("package " + name + "\n")}
	}
	manifest, err := packages.ReadManifest(tree)
	if err != nil {
		t.Fatalf("testdata/pipelines/manifest.yaml does not read: %v", err)
	}
	spec := manifest.Pipeline
	if spec == nil {
		t.Fatal("testdata/pipelines/manifest.yaml declares no pipeline block")
	}
	if refusal := pl.Validate(spec); refusal != nil {
		t.Fatalf("testdata/pipelines/manifest.yaml does not validate: %v", refusal)
	}
	in := pl.CompileInput{
		Mode:           pl.ModeFull,
		Event:          pl.EventPush,
		Compute:        pl.ComputeClusterAndFleet,
		AllowedSecrets: []string{substrateSecret},
	}
	var all []string
	if pl.NeedsSelector(spec) {
		graph, err := pl.ScanGoTree(tree)
		if err != nil {
			t.Fatalf("the synthetic Go tree does not scan: %v", err)
		}
		var full []string
		if spec.Select != nil {
			full = spec.Select.Full
		}
		// A full run reads no change list, and Affected reads none as all.
		selection, err := pl.Affected(graph, nil, full)
		if err != nil {
			t.Fatalf("selecting the synthetic tree's packages: %v", err)
		}
		in.Selector = pl.GraphSelector(graph, selection)
		all = in.Selector.All()
	}
	plan, refusal := pl.Compile(spec, in)
	if refusal != nil {
		t.Fatalf("testdata/pipelines/manifest.yaml does not compile for a push: %v", refusal)
	}
	if len(all) != len(substratePackages) {
		t.Fatalf("the synthetic tree's import graph holds %v; want one package per directory of %v", all, substratePackages)
	}
	// The secret store holds what the owner stored under PGPASSWORD: the
	// sidecar's password, which the step must present.
	password := spec.Services["postgres"].Env["POSTGRES_PASSWORD"]
	if password == "" {
		t.Fatal("testdata/pipelines/manifest.yaml's postgres service sets no POSTGRES_PASSWORD")
	}
	return substratePlan{name: manifest.Name, plan: plan, packages: all, secrets: map[string]string{substrateSecret: password}}
}

// ---------------------------------------------------------------------------
// The run, driven the way the driver drives one
// ---------------------------------------------------------------------------

// substrateStep is one compiled step and how it ended.
type substrateStep struct {
	step pl.Step
	// sent: handed to the executor. blocked: never sent, because the stage
	// named failed before this step's began.
	sent    bool
	blocked string
	// answered: the executor answered, with result or err.
	answered bool
	result   pl.StepResult
	err      error
	elapsed  time.Duration
}

// substrateRun is one run of the compiled plan.
type substrateRun struct {
	pipeline                     string
	pipelineID, runID, workRunID string
	started, finished            time.Time
	stages                       [][]*substrateStep
	steps                        []*substrateStep // plan order
	secrets                      map[string]string
}

func newSubstrateRun(compiled substratePlan) *substrateRun {
	r := &substrateRun{
		pipeline:   compiled.name,
		pipelineID: "pipeline-e2e-" + id.NewShortId(),
		runID:      "run-e2e-" + id.NewShortId(),
		workRunID:  "work-e2e-" + id.NewShortId(),
		secrets:    compiled.secrets,
	}
	for _, stage := range compiled.plan.Stages {
		var tracks []*substrateStep
		for _, step := range stage.Steps {
			s := &substrateStep{step: step}
			tracks = append(tracks, s)
			r.steps = append(r.steps, s)
		}
		r.stages = append(r.stages, tracks)
	}
	return r
}

// request is what the executor is handed for one step: the driver's
// request(), field for field, with this run's facts.
func (r *substrateRun) request(step pl.Step) pl.StepRequest {
	var secrets map[string]string
	if len(step.Secrets) > 0 {
		secrets = make(map[string]string, len(step.Secrets))
		for _, name := range step.Secrets {
			secrets[name] = r.secrets[name]
		}
	}
	return pl.StepRequest{
		RunID:        r.runID,
		WorkRunID:    r.workRunID,
		StepKey:      step.Key,
		Attempt:      1,
		RunAttempt:   1,
		RunStartedAt: r.started.UTC().Format(time.RFC3339),
		PipelineID:   r.pipelineID,
		OwnerUserID:  substrateOwnerUser,
		// setFacts composes the clone URL from the repository's name.
		Repository: pl.Repository{Owner: substrateOwner, Name: substrateRepo,
			CloneURL: "https://github.com/" + substrateOwner + "/" + substrateRepo + ".git"},
		SHA:            substrateSHA,
		Mode:           pl.ModeFull,
		Event:          pl.EventPush,
		Version:        pl.Version(pl.EventPush, substrateSHA, ""),
		InstallationID: 0,
		Compute:        pl.ComputeClusterAndFleet,
		Step:           step,
		Secrets:        secrets,
	}
}

// drive runs the stages strictly in the order written, a stage's steps at
// once, and blocks every later stage once one fails (driver.go execute): each
// step bounded by its run's ceiling and the grace the driver gives a runner
// past it (driver.go stepDeadline, ruling R31b) -- not by its own timeout,
// which runs from its Job's creation.
func (r *substrateRun) drive(ctx context.Context, exec pl.Executor, ceiling time.Duration) {
	r.started = time.Now()
	blockedBy := ""
	for _, stage := range r.stages {
		if blockedBy != "" {
			for _, s := range stage {
				if s.step.Skip == nil {
					s.blocked = blockedBy
				}
			}
			continue
		}
		var wg sync.WaitGroup
		for _, s := range stage {
			if s.step.Skip != nil {
				continue // settled as skipped, never sent
			}
			s.sent = true
			wg.Add(1)
			go func(s *substrateStep, req pl.StepRequest) {
				defer wg.Done()
				stepCtx, cancel := context.WithDeadline(ctx, r.started.Add(ceiling+10*time.Minute))
				defer cancel()
				began := time.Now()
				res, err := exec.Execute(stepCtx, req)
				s.result, s.err, s.elapsed, s.answered = res, err, time.Since(began), true
			}(s, r.request(s.step))
		}
		wg.Wait()
		for _, s := range stage {
			switch s.report(nil).Status {
			case pipelinerun.StepFailed, pipelinerun.StepRefused, pipelinerun.StepCancelled:
				blockedBy = s.step.Stage
			}
		}
	}
	r.finished = time.Now()
}

// report is one step as the driver reports it to the check run: receiptFor's
// mapping of the executor's answer (component/pipelinerun/driver.go, restated
// because it is unexported and the seam exports no path from a StepResult to
// its report), its message masked with every secret value the drive resolved
// as the driver's report() masks it, and the seam's own StepState.Report. It
// follows receiptFor rule for rule, the work status included: a refused
// outcome is a FAILED work step there, so a refusal with no message gets the
// exit sentence as a failure does (the report-mapping subtest holds it).
func (s *substrateStep) report(values []string) pl.StepReport {
	st := pipelinerun.StepState{Key: s.step.Key, Stage: s.step.Stage, Name: s.step.Name}
	mask := func(text string) string { return pl.MaskSecrets(strings.TrimSpace(text), values) }
	switch {
	case s.step.Skip != nil:
		st.Status, st.Code, st.Message = pipelinerun.StepSkipped, s.step.Skip.Code, s.step.Skip.Reason
	case s.blocked != "":
		st.Status, st.Code, st.Message = pipelinerun.StepSkipped, pl.CodeStageBlocked, "Not run: stage "+s.blocked+" failed."
	case !s.answered:
		st.Status = pipelinerun.StepPending
	case s.err != nil:
		st.Status, st.Code = pipelinerun.StepFailed, pl.CodeExecutorError
		st.Message = "The runner could not report how the step ended: " + pl.MaskSecrets(s.err.Error(), values)
	default:
		res := s.result
		// workStatus is receiptFor's rec.status, the v1:work:step status the
		// outcome becomes; st.Status is its rec.report.
		var workStatus string
		switch res.Status {
		case pl.OutcomeSucceeded:
			workStatus, st.Status = pipelinerun.WorkStepDone, pipelinerun.StepSucceeded
		case pl.OutcomeFailed:
			workStatus, st.Status = pipelinerun.WorkStepFailed, pipelinerun.StepFailed
		case pl.OutcomeRefused:
			workStatus, st.Status = pipelinerun.WorkStepFailed, pipelinerun.StepRefused
		case pl.OutcomeCancelled:
			workStatus, st.Status = pipelinerun.WorkStepCancelled, pipelinerun.StepCancelled
		default:
			workStatus, st.Status, st.Code = pipelinerun.WorkStepFailed, pipelinerun.StepFailed, pl.CodeExecutorError
			st.Message = fmt.Sprintf("The runner answered an outcome this driver does not know (%q).", mask(string(res.Status)))
		}
		if res.Failure != nil {
			st.Code, st.Message = mask(res.Failure.Code), mask(res.Failure.Message)
		}
		if st.Message == "" && workStatus == pipelinerun.WorkStepFailed {
			if res.ExitCode < 0 {
				st.Message = "The step's command did not run."
			} else {
				st.Message = fmt.Sprintf("The step's command exited with status %d.", res.ExitCode)
			}
		}
		st.DurationMs = stepDurationMs(res, s.elapsed)
		st.LogTail = pl.MaskSecrets(res.LogTail, values)
	}
	return st.Report()
}

// stepDurationMs is the step's time as the runner measured it, else how long
// the driver waited (driver.go durationOf).
func stepDurationMs(res pl.StepResult, waited time.Duration) int64 {
	started, err1 := time.Parse(time.RFC3339, strings.TrimSpace(res.StartedAt))
	finished, err2 := time.Parse(time.RFC3339, strings.TrimSpace(res.FinishedAt))
	if err1 == nil && err2 == nil && !finished.Before(started) {
		if ms := finished.Sub(started).Milliseconds(); ms > 0 {
			return ms
		}
	}
	return max(waited.Milliseconds(), 1)
}

// conclusion is the run's, from its steps (driver.go verdictOfSteps).
func (r *substrateRun) conclusion() string {
	for _, s := range r.steps {
		switch s.report(nil).Status {
		case pipelinerun.StepFailed, pipelinerun.StepRefused, pipelinerun.StepCancelled, pipelinerun.StepPending, pipelinerun.StepRunning:
			return pipelinerun.ConclusionFailure
		}
	}
	return pipelinerun.ConclusionSuccess
}

// checkRunBody is the body GitHub receives when the run's check run is moved
// to its conclusion: ComposeCheckRun over ReportFor over every step's report,
// sent by the real githubapp client as an update of the check run the opening
// created.
func (r *substrateRun) checkRunBody(t *testing.T) []byte {
	t.Helper()
	gh := newGitHubCapture(t)
	values := make([]string, 0, len(r.secrets))
	for _, v := range r.secrets {
		values = append(values, v)
	}
	steps := make([]pl.StepReport, 0, len(r.steps))
	for _, s := range r.steps {
		steps = append(steps, s.report(values))
	}
	p := pipelinerun.Pipeline{ID: r.pipelineID, OwnerUserID: substrateOwnerUser, Name: r.pipeline,
		Repository: substrateOwner + "/" + substrateRepo, Compute: pl.ComputeClusterAndFleet, Status: pipelinerun.PipelineActive}
	run := pipelinerun.Run{
		ID: r.runID, OwnerUserID: substrateOwnerUser, PipelineID: r.pipelineID, Repository: p.Repository,
		SHA: substrateSHA, Mode: pl.ModeFull, Event: pl.EventPush, Attempt: 1, Version: substrateSHA,
		Status: pipelinerun.StatusCompleted, Conclusion: r.conclusion(), CheckRunID: substrateCheckRunID,
		WorkRunID: r.workRunID, StartedAt: r.started.UTC().Truncate(time.Second), FinishedAt: r.finished.UTC().Truncate(time.Second),
	}
	check := pipelinerun.ComposeCheckRun(substrateOSOrigin, p, run, pipelinerun.ReportFor(p, run, steps))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gh.client.UpdateCheckRun(ctx, "e2e-installation-token", substrateOwner, substrateRepo, substrateCheckRunID, check); err != nil {
		t.Fatalf("writing the run's check run to the recording GitHub: %v", err)
	}
	return gh.only(t, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/check-runs/%d", substrateOwner, substrateRepo, substrateCheckRunID))
}

func (r *substrateRun) step(key string) *substrateStep {
	for _, s := range r.steps {
		if s.step.Key == key {
			return s
		}
	}
	return nil
}

// mustStep is the step compiled under key, or the test ends saying the plan
// has none.
func (r *substrateRun) mustStep(t *testing.T, key string) *substrateStep {
	t.Helper()
	s := r.step(key)
	if s == nil {
		t.Fatalf("the plan has no step %s; it compiled to %v", key, stepKeys(r.steps))
	}
	return s
}

// mustResult is the executor's answer for a step that was sent, or the test
// ends saying why there is none.
func (s *substrateStep) mustResult(t *testing.T) pl.StepResult {
	t.Helper()
	switch {
	case s.blocked != "":
		t.Fatalf("%s never ran: stage %s failed before it", s.step.Key, s.blocked)
	case !s.sent:
		t.Fatalf("%s was never sent (skip: %+v)", s.step.Key, s.step.Skip)
	case !s.answered:
		t.Fatalf("%s was sent and never answered", s.step.Key)
	case s.err != nil:
		t.Fatalf("the executor could not report how %s ended: %v", s.step.Key, s.err)
	}
	return s.result
}

// summary is one line per step, for the test's log.
func (r *substrateRun) summary() string {
	var b strings.Builder
	for _, s := range r.steps {
		fmt.Fprintf(&b, "  %-20s ", s.step.Key)
		switch {
		case s.step.Skip != nil:
			fmt.Fprintf(&b, "skipped: %s\n", s.step.Skip.Reason)
		case s.blocked != "":
			fmt.Fprintf(&b, "not run: stage %s failed\n", s.blocked)
		case !s.answered:
			b.WriteString("never answered\n")
		case s.err != nil:
			fmt.Fprintf(&b, "error: %v\n", s.err)
		default:
			res := s.result
			code := ""
			if res.Failure != nil {
				code = " " + res.Failure.Code
			}
			fmt.Fprintf(&b, "%s exit %d%s, on %s (node %q, Job %q), %s, log %q, %d artifacts\n", res.Status, res.ExitCode, code,
				res.Where.Surface, res.Where.NodeID, res.Where.JobName, s.elapsed.Round(time.Second),
				res.LogFileID, len(res.ArtifactFileIDs))
		}
	}
	return b.String()
}

func stepKeys(steps []*substrateStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.step.Key)
	}
	return out
}

// outputLines is a step's output, line by line, without blank lines: the
// wrapper prints one before an artifact frame.
func outputLines(tail string) []string {
	var out []string
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// printedPackages is what a shard printed: its one line of output, which
// `echo "$MEMQL_PACKAGES"` makes, split into import paths. A line the runner
// wrote into the log itself ("memql: ...") is not the step's.
func printedPackages(tail string) []string {
	var printed []string
	for _, line := range outputLines(tail) {
		if strings.HasPrefix(line, "memql:") {
			continue
		}
		printed = append(printed, strings.Fields(line)...)
	}
	return printed
}

// ---------------------------------------------------------------------------
// The forward transport: the agent's road to two workbench replicas
// ---------------------------------------------------------------------------

// substrateReplica is one workbench replica: the real ForwardHandler, with a
// Runner behind it.
type substrateReplica struct {
	node    string
	handler *workbench.ForwardHandler
}

// substrateSend is one forward the executor sent.
type substrateSend struct {
	node, action, runID, stepKey string
}

// substrateMesh is the Executor's Forwarder over in-process replicas, the
// shape of integrations/pipelinesteps' hopMesh. Every replica is healthy and
// every reply arrives: this leg is about the cluster, and the hop tests are
// about the mesh going wrong.
//
// WHICH REPLICA ANSWERS. A step forward goes round the replicas, so the two
// share the steps. A status or an ack goes to a replica OTHER than the one
// that ran the step: the real router may pick either (ruling R29), and the
// other is the case the runner's design exists for -- a replica holding none
// of the step in memory, answering from the Job alone.
type substrateMesh struct {
	mu           sync.Mutex
	replicas     []*substrateReplica
	next         int
	inflight     map[string]chan *nodev1.WorkbenchForwardResponse
	sends        []substrateSend
	stepServedBy map[string]string // runID/stepKey -> the replica the step went to
}

var _ pipelinesteps.Forwarder = (*substrateMesh)(nil)

func (m *substrateMesh) SelfNodeId() string   { return substrateAgent }
func (m *substrateMesh) SelfNodeType() string { return "agent" }

func (m *substrateMesh) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin string) (*nodev1.WorkbenchForwardResponse, string, error) {
	return m.forward(ctx, req, pin, "", nil)
}

func (m *substrateMesh) ForwardWatchedExcluding(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, _ time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	return m.forward(ctx, req, pin, exclude, onSelected)
}

func (m *substrateMesh) pickLocked(req *nodev1.WorkbenchForwardRequest, pin, exclude string) *substrateReplica {
	if pin != "" {
		for _, r := range m.replicas {
			if r.node == pin && r.node != exclude {
				return r
			}
		}
	}
	key := req.GetRunId() + "/" + req.GetStepId()
	if req.GetAction() != workbench.PipelineStepAction {
		if ran := m.stepServedBy[key]; ran != "" {
			for _, r := range m.replicas {
				if r.node != ran {
					return r
				}
			}
		}
	}
	for range m.replicas {
		r := m.replicas[m.next%len(m.replicas)]
		m.next++
		if r.node != exclude || len(m.replicas) == 1 {
			if req.GetAction() == workbench.PipelineStepAction {
				m.stepServedBy[key] = r.node
			}
			return r
		}
	}
	return nil
}

func (m *substrateMesh) forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	m.mu.Lock()
	r := m.pickLocked(req, pin, exclude)
	if r == nil {
		m.mu.Unlock()
		return nil, "", workbench.ErrNoWorkbenchPeer
	}
	if req.RequestId == "" {
		req.RequestId = id.NewShortId()
	}
	ch := make(chan *nodev1.WorkbenchForwardResponse, 1)
	m.inflight[req.RequestId] = ch
	m.sends = append(m.sends, substrateSend{node: r.node, action: req.GetAction(), runID: req.GetRunId(), stepKey: req.GetStepId()})
	m.mu.Unlock()
	if onSelected != nil {
		onSelected(r.node)
	}
	defer func() {
		m.mu.Lock()
		delete(m.inflight, req.RequestId)
		m.mu.Unlock()
	}()

	// The replica's receive loop calls the handler inline, as the node
	// stream does; a step is started on a goroutine of its own and replies
	// when it ends.
	r.handler.HandleForwardedRequest(context.Background(), req, m.deliver)
	select {
	case resp := <-ch:
		return resp, r.node, nil
	case <-ctx.Done():
		// What the router sends when the caller stops waiting: the replica
		// cancels the work, which for a step is deleting its Job.
		r.handler.CancelForwardedRequest(context.Background(), req.RequestId)
		return nil, r.node, ctx.Err()
	}
}

// deliver is every replica's send: the reply goes to whoever still waits.
func (m *substrateMesh) deliver(msg *nodev1.NodeServerMessage) error {
	resp := msg.GetWorkbenchForwardResponse()
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch := m.inflight[resp.GetRequestId()]; ch != nil {
		select {
		case ch <- resp:
		default:
		}
	}
	return nil
}

// servedSteps is how many step forwards each replica took.
func (m *substrateMesh) servedSteps() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, s := range m.sends {
		if s.action == workbench.PipelineStepAction {
			out[s.node]++
		}
	}
	return out
}

// stepForwards is every replica a step forward for key went to.
func (m *substrateMesh) stepForwards(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.sends {
		if s.action == workbench.PipelineStepAction && s.stepKey == key {
			out = append(out, s.node)
		}
	}
	return out
}

// runnerAdapter is the workbench's PipelineRunner over the substrate's Runner:
// JSON in, JSON out. Its production twin is app/pipelines_runner_adapter.go
// (pipelinesRunnerAdapter, issue memql#5495), which is unexported in package
// app, so the translation is restated here; when both are on one branch, the
// two are reconciled -- same decode refusals, same reply shapes.
type runnerAdapter struct {
	runner *pipelinesteps.Runner
}

var _ workbench.PipelineRunner = runnerAdapter{}

func (a runnerAdapter) Readiness(context.Context) ([]byte, string) {
	return encodeJSON(a.runner.Readiness()), ""
}

func (a runnerAdapter) RunStep(ctx context.Context, argsJSON []byte) []byte {
	var run pipelinesteps.StepRun
	if err := json.Unmarshal(argsJSON, &run); err != nil {
		return encodeJSON(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeExecutorError, Message: "the forwarded step could not be read"}})
	}
	return encodeJSON(a.runner.Run(ctx, run))
}

func (a runnerAdapter) Status(ctx context.Context, argsJSON []byte) ([]byte, string) {
	var req pipelinesteps.StatusRequest
	if err := json.Unmarshal(argsJSON, &req); err != nil {
		return nil, "decode_args"
	}
	return encodeJSON(a.runner.Status(ctx, req)), ""
}

func (a runnerAdapter) Ack(ctx context.Context, argsJSON []byte) string {
	var req pipelinesteps.AckRequest
	if err := json.Unmarshal(argsJSON, &req); err != nil {
		return "decode_args"
	}
	if err := a.runner.Ack(ctx, req); err != nil {
		return "ack_failed"
	}
	return ""
}

func (a runnerAdapter) CancelRun(ctx context.Context, argsJSON []byte) ([]byte, string) {
	var req pipelinesteps.CancelRequest
	if err := json.Unmarshal(argsJSON, &req); err != nil {
		return nil, "decode_args"
	}
	n, err := a.runner.CancelRun(ctx, req)
	reply := encodeJSON(map[string]int{"jobsDeleted": n})
	if err != nil {
		return reply, "cancel_failed"
	}
	return reply, ""
}

func encodeJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// What stands in for the rest of the engine
// ---------------------------------------------------------------------------

// noMachineDispatcher is the agent's dispatcher for an owner with no machine.
// It answers exactly what integrations/agent/worker's Dispatcher answers when
// the router finds no candidate because none is paired: OK false, the code
// no_worker_available, noCandidateMessage's sentence for no paired machine, and
// the dispatcher's own verdict that nothing started on any machine
// (dispatch.go, Dispatch's empty-candidate branch). That verdict is the
// dispatcher's tests' to prove. What this leg proves is the other half, for
// real: what the executor's fleet half asks for, and what its classification
// makes of the answer.
type noMachineDispatcher struct {
	mu       sync.Mutex
	requests []dispatchedStep
}

// dispatchedStep is one request the fleet half made, as the dispatcher saw it.
type dispatchedStep struct {
	req worker.Request
	// internal: the call carried internal origin, without which the dispatcher
	// refuses the pipeline purpose (pipeline_purpose.go).
	internal bool
}

var _ pipelinesteps.FleetDispatcher = (*noMachineDispatcher)(nil)

func (d *noMachineDispatcher) Dispatch(ctx context.Context, req worker.Request) (worker.Result, error) {
	d.mu.Lock()
	d.requests = append(d.requests, dispatchedStep{req: req, internal: auth.OriginFromContext(ctx).IsInternal()})
	d.mu.Unlock()
	return worker.Result{
		OK:                 false,
		ErrorCode:          "no_worker_available",
		ErrorMessage:       "no machines are paired to this account",
		RefusedBeforeStart: true,
	}, nil
}

func (d *noMachineDispatcher) asked() []dispatchedStep {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

func dispatchedKeys(steps []dispatchedStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.req.StepId)
	}
	return out
}

// storedFile is one file the Library was handed, with the id it answered.
type storedFile struct {
	pipelinesteps.RunFile
	id string
}

// recordingLibrary is the owner's Library: it keeps every file it is handed.
type recordingLibrary struct {
	mu    sync.Mutex
	files []storedFile
}

var _ pipelinesteps.LibraryStore = (*recordingLibrary)(nil)

func (l *recordingLibrary) StoreRunFile(_ context.Context, f pipelinesteps.RunFile) (pipelinesteps.StoredFile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f.Bytes = bytes.Clone(f.Bytes)
	stored := storedFile{RunFile: f, id: fmt.Sprintf("file-%d", len(l.files)+1)}
	l.files = append(l.files, stored)
	return pipelinesteps.StoredFile{FileID: stored.id}, nil
}

func (l *recordingLibrary) all() []storedFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.files)
}

func filesFor(files []storedFile, stepKey string) []storedFile {
	var out []storedFile
	for _, f := range files {
		if f.StepKey == stepKey {
			out = append(out, f)
		}
	}
	return out
}

func filesNamed(files []storedFile, stepKey, name string) []storedFile {
	var out []storedFile
	for _, f := range filesFor(files, stepKey) {
		if f.Name == name {
			out = append(out, f)
		}
	}
	return out
}

func fileNames(files []storedFile) string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.StepKey+":"+f.Name)
	}
	return strings.Join(names, ", ")
}

// anonymousCloneTokens is the clone token minter for a public repository:
// installation 0 is an anonymous clone, answered with no token and no error,
// as app/'s minter answers it. Any other installation is refused, so a step
// that asked for a real installation's token cannot pass.
type anonymousCloneTokens struct {
	mu    sync.Mutex
	calls []int64
}

var _ pipelinesteps.TokenMinter = (*anonymousCloneTokens)(nil)

func (m *anonymousCloneTokens) CloneToken(_ context.Context, installationID int64, owner, name string) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, installationID)
	m.mu.Unlock()
	if installationID != 0 {
		return "", fmt.Errorf("this leg clones %s/%s anonymously, and was asked for installation %d's token", owner, name, installationID)
	}
	return "", nil
}

func (m *anonymousCloneTokens) asked() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// ---------------------------------------------------------------------------
// GitHub, recorded
// ---------------------------------------------------------------------------

// githubCapture is GitHub's API as the real githubapp client reaches it: the
// check-run endpoints, answered as GitHub answers, each request kept.
type githubCapture struct {
	client   *githubapp.Client
	mu       sync.Mutex
	requests []githubRequest
}

type githubRequest struct {
	method, path string
	body         []byte
}

func newGitHubCapture(t *testing.T) *githubCapture {
	t.Helper()
	c := &githubCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, githubRequest{method: r.Method, path: r.URL.Path, body: body})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id": %d}`, substrateCheckRunID)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/check-runs/"):
			fmt.Fprintf(w, `{"id": %d}`, substrateCheckRunID)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message": "Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	c.client = githubapp.New(githubapp.Config{}, githubapp.WithAPIBase(srv.URL), githubapp.WithHTTPClient(srv.Client()))
	return c
}

// only is the body of the one request made, which must be method path.
func (c *githubCapture) only(t *testing.T, method, path string) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) != 1 || c.requests[0].method != method || c.requests[0].path != path {
		var got []string
		for _, r := range c.requests {
			got = append(got, r.method+" "+r.path)
		}
		t.Fatalf("GitHub was asked %v; want exactly %s %s", got, method, path)
	}
	return c.requests[0].body
}

// forkRows are the rows the fork's opening reads and writes: the staged
// delivery, the one pipeline connected to its repository, the runs it opens,
// and the pull request's unfinished runs a new head would supersede. Every
// other Store method is the nil interface's, so a path that reached one would
// fail loudly: an opening reads and writes nothing else, and a refused fork's
// run is never driven, re-run or concluded again.
type forkRows struct {
	pipelinerun.Store
	delivery pipelinerun.InboundDelivery
	pipeline pipelinerun.Pipeline
	mu       sync.Mutex
	runs     []pipelinerun.Run
}

func (s *forkRows) InboundDelivery(_ context.Context, requestID string) (*pipelinerun.InboundDelivery, error) {
	if requestID != s.delivery.ID {
		return nil, nil
	}
	d := s.delivery
	return &d, nil
}

func (s *forkRows) PipelinesForRepository(_ context.Context, repository string) ([]pipelinerun.Pipeline, error) {
	if !strings.EqualFold(repository, s.pipeline.Repository) {
		return nil, nil
	}
	return []pipelinerun.Pipeline{s.pipeline}, nil
}

func (s *forkRows) RunsForKey(_ context.Context, runKey string) ([]pipelinerun.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pipelinerun.Run
	for _, r := range s.runs {
		if r.RunKey == runKey {
			out = append(out, r)
		}
	}
	return out, nil
}

// RunsUnfinishedForPullRequest is what a new head of the pull request stops
// (component/pipelinerun's supersede.go): its unfinished runs among the ones
// opened here, which the opening reads before it creates its own. A fork's run
// is refused, and so finished, the moment it is created, so there is never
// one to stop; the answer is the rows' all the same.
func (s *forkRows) RunsUnfinishedForPullRequest(_ context.Context, pipelineID string, pullRequest int) ([]pipelinerun.Run, error) {
	if pullRequest <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pipelinerun.Run
	for _, r := range s.runs {
		if r.PipelineID == pipelineID && r.PullRequest == pullRequest && !r.Finished() {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *forkRows) CreateRun(_ context.Context, r pipelinerun.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, r)
	return nil
}

// forkGitHub is the pipelines GitHub port over the real githubapp client: the
// token mint answers at once, and the check run reaches the recording GitHub.
type forkGitHub struct {
	pipelinerun.GitHub
	client       *githubapp.Client
	installation int64
}

func (g forkGitHub) InstallationToken(context.Context, string, string, string) (string, int64, error) {
	return "e2e-installation-token", g.installation, nil
}

func (g forkGitHub) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	owner, name, _ := strings.Cut(repository, "/")
	return g.client.CreateCheckRun(ctx, token, owner, name, run)
}

// openForkRun stages the fork's delivery as a verified GitHub webhook and hands
// it to the seam's own Trigger, under the internal origin the trigger
// automation runs with: ClassifyDelivery, the pipeline match, the opening's
// dedup and the fork's refusal are the seam's, and so is the check run it
// writes. It answers the run opened and the body GitHub received.
func openForkRun(t *testing.T, logger *slog.Logger, delivery []byte, trigger pl.Trigger) (pipelinerun.Run, []byte) {
	t.Helper()
	gh := newGitHubCapture(t)
	rows := &forkRows{
		delivery: pipelinerun.InboundDelivery{
			ID:                "inbound-e2e-fork",
			Source:            "github",
			Body:              string(delivery),
			HeadersJSON:       `{"x-github-event":"pull_request","x-github-delivery":"delivery-e2e-fork"}`,
			SignatureVerified: true,
		},
		pipeline: pipelinerun.Pipeline{
			ID: "pipeline-e2e-fork", OwnerUserID: substrateOwnerUser, Name: "pipelines-e2e",
			Repository: trigger.Repository, DefaultBranch: trigger.DefaultBranch, InstallationID: trigger.InstallationID,
			CredentialID: "credential-e2e", Compute: pl.ComputeCluster, Status: pipelinerun.PipelineActive,
		},
	}
	var gate sync.Mutex
	integration := pipelinerun.New(pipelinerun.Deps{
		Store:  rows,
		GitHub: forkGitHub{client: gh.client, installation: trigger.InstallationID},
		Gate: func(ctx context.Context, _ string, fn func(context.Context) error) error {
			gate.Lock()
			defer gate.Unlock()
			return fn(ctx)
		},
		OSOrigin: func() string { return substrateOSOrigin },
		Now:      func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
		Logger:   logger,
	})
	ctx, cancel := context.WithTimeout(auth.ContextWithInternalOrigin(context.Background()), 30*time.Second)
	defer cancel()
	res, err := integration.Trigger(ctx, rows.delivery.ID)
	if err != nil {
		t.Fatalf("the seam's Trigger failed on the fork's delivery: %v", err)
	}
	if res.Ignored != "" || len(res.Opened) != 1 {
		t.Fatalf("the seam's Trigger ignored %q, opened %d runs, skipped %+v; want the fork's one run opened",
			res.Ignored, len(res.Opened), res.Skipped)
	}
	return res.Opened[0], gh.only(t, http.MethodPost, "/repos/"+trigger.Repository+"/check-runs")
}

// ---------------------------------------------------------------------------
// The recorded fixtures
// ---------------------------------------------------------------------------

var (
	// tableDuration is the Time cell of a stage-table row (pipelines'
	// formatDuration: 45s, 1m 02s, 1h 02m 05s, <1s); "-" is a time nobody
	// measured, and stays.
	tableDuration = regexp.MustCompile(`(?m)^(\|.*\| )((?:\d+h )?(?:\d+m )?\d+s|<1s)( \|)$`)
	// titleDuration is a passed run's total in its title.
	titleDuration = regexp.MustCompile(`^(Passed: .* in )((?:\d+h )?(?:\d+m )?\d+s|<1s)$`)
)

// normalizedCheckRun is a check-run request body with what differs from one
// run to the next written as a placeholder -- the run's id (the external id
// and the details link), its commit (<sha> whole, <sha7> by its first seven),
// its times and the stage table's durations -- and everything else as GitHub
// received it: the name, the status, the conclusion, the title, each stage's
// row and every sentence. Pretty-printed with sorted keys, so a diff of two
// reads line by line.
func normalizedCheckRun(body []byte, runID, sha string) ([]byte, error) {
	var check map[string]any
	if err := json.Unmarshal(body, &check); err != nil {
		return nil, err
	}
	var walk func(key string, v any) any
	walk = func(key string, v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				x[k] = walk(k, e)
			}
			return x
		case string:
			switch key {
			case "started_at", "completed_at":
				if _, err := time.Parse(time.RFC3339, x); err == nil {
					return "<timestamp>"
				}
			case "summary":
				x = tableDuration.ReplaceAllString(x, "${1}<duration>${3}")
			case "title":
				x = titleDuration.ReplaceAllString(x, "${1}<duration>")
			}
			x = strings.ReplaceAll(x, runID, "<run-id>")
			x = strings.ReplaceAll(x, sha, "<sha>")
			if len(sha) >= 7 {
				// The summary's meta line names the commit by its first seven,
				// under a placeholder of its own: a summary that switched from
				// the short form to the long one must not normalize the same.
				x = strings.ReplaceAll(x, sha[:7], "<sha7>")
			}
			return x
		}
		return v
	}
	walk("", check)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(check); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func readSubstrateFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pipelines", name))
	if err != nil {
		t.Fatalf("reading testdata/pipelines/%s: %v", name, err)
	}
	return b
}

// matchSubstrateFixture holds got to testdata/pipelines/name, or rewrites it
// under -update.
func matchSubstrateFixture(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "pipelines", name)
	if *updatePipelinesFixtures {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("rewriting %s: %v", path, err)
		}
		t.Logf("rewrote %s from this run", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s cannot be read (%v); record it with -update against a cluster", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the check run the seam composed differs from %s (both normalized).\n--- got\n%s--- want\n%s"+
			"If the seam's copy changed on purpose, record it again with -update.", path, got, want)
	}
}

// ---------------------------------------------------------------------------
// What the run leaves in the namespace
// ---------------------------------------------------------------------------

// runSelector selects every object the runner made for a run: the labels
// BuildJob and BuildSecret put on the Job, its pod template and its Secret.
func runSelector(runID string) string {
	return pipelinesteps.LabelManagedBy + "=" + pipelinesteps.ManagedBy + "," +
		pipelinesteps.LabelRun + "=" + pipelinesteps.RunLabelValue(runID)
}

// runObjects is what a run has in the namespace, by name.
type runObjects struct {
	jobs, secrets, pods []string
	// stepKeys is the memql.io/step-key of every Job.
	stepKeys []string
	// jobOfStep is each Job's name by its step key.
	jobOfStep map[string]string
	// specs is each step's Job as the API server held it, read back whole
	// once while it lived (the watch fills it; a listing does not).
	specs map[string]pipelinesteps.Job
}

func (o runObjects) empty() bool { return len(o.jobs)+len(o.secrets)+len(o.pods) == 0 }

func (o runObjects) String() string {
	return fmt.Sprintf("Jobs %v, Secrets %v, pods %v", o.jobs, o.secrets, o.pods)
}

// listRunObjects lists the run's Jobs, Secrets and pods as metadata alone, so
// no Secret's values are read.
func listRunObjects(ctx context.Context, c *substrateCluster, namespace, selector string) (runObjects, error) {
	const metadataOnly = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	query := "?labelSelector=" + url.QueryEscape(selector)
	var out runObjects
	for _, kind := range []struct {
		path string
		into *[]string
	}{
		{"apis/batch/v1/namespaces/" + namespace + "/jobs", &out.jobs},
		{"api/v1/namespaces/" + namespace + "/secrets", &out.secrets},
		{"api/v1/namespaces/" + namespace + "/pods", &out.pods},
	} {
		body, status, err := c.call(ctx, http.MethodGet, kind.path+query, metadataOnly, nil)
		if err != nil {
			return runObjects{}, err
		}
		if status != http.StatusOK {
			return runObjects{}, fmt.Errorf("listing %s answered %d: %s", kind.path, status, body)
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name        string            `json:"name"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return runObjects{}, fmt.Errorf("listing %s: %w", kind.path, err)
		}
		for _, item := range list.Items {
			*kind.into = append(*kind.into, item.Metadata.Name)
			if kind.into == &out.jobs {
				key := item.Metadata.Annotations[pipelinesteps.AnnotStepKey]
				out.stepKeys = append(out.stepKeys, key)
				if out.jobOfStep == nil {
					out.jobOfStep = map[string]string{}
				}
				out.jobOfStep[key] = item.Metadata.Name
			}
		}
	}
	return out, nil
}

// runWatch is every object of the run seen while it lived.
type runWatch struct {
	mu   sync.Mutex
	seen map[string]map[string]bool // "jobs" | "secrets" | "pods" | "stepKeys" -> names
	// specs is each step's Job read back whole, the first time it is listed.
	specs map[string]pipelinesteps.Job
	done  chan struct{}
	exit  chan struct{}
}

// watchRunObjects lists the run's objects every two seconds until stopped:
// the reachable positive the cleanup's empty answer is read against, the
// record of which steps ever had a Job, and each step's Job read back whole
// from the API server the first time it is listed -- what the cluster was
// actually handed, not what the test compiled.
func watchRunObjects(ctx context.Context, c *substrateCluster, namespace, selector string) *runWatch {
	w := &runWatch{seen: map[string]map[string]bool{}, specs: map[string]pipelinesteps.Job{},
		done: make(chan struct{}), exit: make(chan struct{})}
	readBack := func(key, name string) {
		body, status, err := c.call(ctx, http.MethodGet, "apis/batch/v1/namespaces/"+namespace+"/jobs/"+name, "", nil)
		if err != nil || status != http.StatusOK {
			return // gone already, or not readable now: the next listing tries again
		}
		var job pipelinesteps.Job
		if json.Unmarshal(body, &job) == nil {
			w.mu.Lock()
			w.specs[key] = job
			w.mu.Unlock()
		}
	}
	record := func(kind string, names []string) {
		if w.seen[kind] == nil {
			w.seen[kind] = map[string]bool{}
		}
		for _, n := range names {
			w.seen[kind][n] = true
		}
	}
	go func() {
		defer close(w.exit)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			if objs, err := listRunObjects(ctx, c, namespace, selector); err == nil {
				w.mu.Lock()
				record("jobs", objs.jobs)
				record("secrets", objs.secrets)
				record("pods", objs.pods)
				record("stepKeys", objs.stepKeys)
				var unread map[string]string
				for key, name := range objs.jobOfStep {
					if _, read := w.specs[key]; !read {
						if unread == nil {
							unread = map[string]string{}
						}
						unread[key] = name
					}
				}
				w.mu.Unlock()
				for key, name := range unread {
					readBack(key, name)
				}
			}
			select {
			case <-w.done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

// stop ends the watch and answers what it saw.
func (w *runWatch) stop() runObjects {
	close(w.done)
	<-w.exit
	w.mu.Lock()
	defer w.mu.Unlock()
	names := func(kind string) []string { return slices.Sorted(maps.Keys(w.seen[kind])) }
	return runObjects{jobs: names("jobs"), secrets: names("secrets"), pods: names("pods"), stepKeys: names("stepKeys"),
		specs: maps.Clone(w.specs)}
}

// awaitRunObjectsGone lists the run's objects until there are none or patience
// runs out, and answers what is left.
func awaitRunObjectsGone(c *substrateCluster, namespace, selector string, patience time.Duration) (runObjects, error) {
	deadline := time.Now().Add(patience)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		left, err := listRunObjects(ctx, c, namespace, selector)
		cancel()
		if err != nil || left.empty() || time.Now().After(deadline) {
			return left, err
		}
		time.Sleep(2 * time.Second)
	}
}

// sweepRunObjects deletes whatever the run left, whatever the test found: a
// leftover Job holds a slot of the ceiling on a cluster other runs share. The
// cleanup subtest has already judged what the runners left.
func sweepRunObjects(t *testing.T, c *substrateCluster, namespace, selector string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := "?labelSelector=" + url.QueryEscape(selector) + "&propagationPolicy=Background"
	for _, path := range []string{
		"apis/batch/v1/namespaces/" + namespace + "/jobs",
		"api/v1/namespaces/" + namespace + "/secrets",
	} {
		if body, status, err := c.call(ctx, http.MethodDelete, path+query, "", nil); err != nil || status >= 300 {
			t.Logf("sweeping the run's leftovers from %s: status %d, %v %s", path, status, err, body)
		}
	}
}
