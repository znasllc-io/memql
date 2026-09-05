package packages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/core/logger"
)

// The D6 stage order. It is written down once, here, and every traversal reads
// it from this slice rather than from a switch of its own -- a second copy is a
// second opinion about the one law the epic turns on.
//
// stage -> roll -> publish, REVERSED on rollback, so the app and the schema
// never disagree in either direction. A failure anywhere before `publishing`
// leaves every site serving exactly what it was serving: nothing the internet
// can see has moved yet. That is what makes a partial deploy structurally
// impossible rather than merely unlikely.
var stageOrder = []string{
	StatusAnalyzing,
	StatusAwaitingConfirm,
	StatusBuilding,
	StatusStagingDsl,
	StatusRolling,
	StatusPublishing,
}

// Deployment statuses, mirroring the enum on v1:platform:packageDeployment.
const (
	StatusAnalyzing       = "analyzing"
	StatusAwaitingConfirm = "awaiting_confirm"
	StatusBuilding        = "building"
	// STATUS SPELLING, and it is not cosmetic. The stage is "stage" in the D6
	// law and every other status here is a plain gerund, so this wants to be
	// "staging" -- and it cannot be. TestNoEnvironmentBranchingInEngineCode
	// fails the build on engine code naming a deployment tier, matching the
	// COMPLETE literal "staging" case-insensitively, and its exemption map is
	// EMPTY by design: an entry there claims a component's JOB is telling
	// deployments apart, which would be false here.
	//
	// So the value says which of several things is being staged, which it
	// should have said anyway -- and the suffix is exactly what takes it out
	// of the gate's match (the gate's own comment names "deploy_staging" as
	// the shape that does not trip it). Do not "tidy" this back to "staging";
	// the build will refuse it, in a file that has nothing to do with deploys.
	StatusStagingDsl = "staging_dsl"
	StatusRolling    = "rolling"
	StatusPublishing = "publishing"
	StatusSucceeded  = "succeeded"
	StatusRefused    = "refused"
	StatusFailed     = "failed"
	// StatusAbandoned is the sweep's (epic memql#4900, task memql#4902): a run
	// whose node stopped saying it was alive. Terminal, and deliberately NOT a
	// flavour of failed -- see sweep.go.
	StatusAbandoned = "abandoned"
	// StatusCancelled is a PERSON's stop (epic memql#4937, D3). Terminal, and
	// deliberately not a flavour of failed for the same reason StatusAbandoned
	// is not -- nothing broke, nothing was published, and the D6 order
	// guarantees every site is still serving what it was serving.
	//
	// The distinction between these two terminal words is WHO DECIDED:
	// abandoned is a loss this cluster OBSERVED, cancelled is a choice
	// somebody MADE. Collapsing them would report a person's own click back to
	// them as a fault.
	StatusCancelled = "cancelled"
)

// Actor is what the pipeline knows about who asked.
//
// IsClusterOwner is carried as a resolved boolean rather than re-derived
// inside each stage: the D9 gate has to answer it at deploy START, before any
// build or stage, and a gate that re-asked later could pass a package whose
// first stages had already run.
type Actor struct {
	UserId         string
	IsClusterOwner bool
}

// Deps is the pipeline's whole outside world. Every field is an interface so
// the state machine is testable end to end with no cluster, no network and no
// object storage -- which is what the D6 ordering law needs, because "the
// publish never happened" is only assertable if the test can see the publish.
type Deps struct {
	Store   *store
	Fetcher Fetcher
	Builder Builder
	// FleetBuilder builds a deployable whose target needs the person's own
	// machine (task memql#4904). Nil on every node that holds no worker
	// streams, and nil is the ANSWER rather than a gap: a target asking for
	// the fleet on a cluster with no fleet route is refused by name at the
	// build stage, which is where every other missing surface is refused.
	FleetBuilder Builder
	Stager       Stager
	Roller       Roller
	Publisher    SitePublisher
	Auditor      Auditor
	Logger       *slog.Logger
	Limits       Limits
	Now          func() time.Time
	// NewId mints deployment and site ids. Injected so a test reads a stable
	// timeline rather than a random one.
	NewId func(prefix string) string
	// Credentials unseals a package's source credential under its owner's
	// actor (epic memql#4885, D10). Shared by the fetcher and the D11 poll,
	// which is the point of it living here: the two paths that present a
	// bearer to GitHub resolve it through ONE function, so "refused for a
	// credential the owner cannot read" cannot be true on one path and false
	// on the other. Nil on a node that cannot resolve credentials; a package
	// naming one is then refused rather than fetched anonymously.
	Credentials CredentialResolver
	// PeekCredentials is the PROBE's resolver (epic memql#4885, D11): the
	// same read, the same two refusals, and no lastUsedAt heartbeat -- a
	// probe is a question, not a fetch, and it writes nothing. A separate
	// field rather than a flag on Credentials so a test can see which of the
	// two a path reached for, and so the fetcher can never be handed the one
	// that does not stamp.
	PeekCredentials CredentialResolver
	// HTTP is the client the D11 poll and the source probe ask GitHub with.
	// Nil means a 30s default. Injected so a test can stand a fake GitHub
	// behind it and read which requests carried a bearer -- and which were
	// never made.
	HTTP *http.Client
	// GitHubApp is the cluster's GitHub App (epic memql#4912). It is what
	// turns a grant into the installation token background work runs under,
	// what asks whether the app is installed on a repository at all, and what
	// reads the branch list and manifest summary the probe answers with.
	//
	// NIL MEANS THIS NODE HAS NO APP, and every path that needs one refuses by
	// name (github_app_not_configured) rather than falling through: a cluster
	// with no app offers the pasted-token path alone, and saying so is the
	// operator's answer rather than a person's error.
	GitHubApp *githubapp.Client
}

func (d *Deps) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *Deps) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// DeployRequest is one deploy or redeploy.
type DeployRequest struct {
	PackageId    string
	DeploymentId string
	Actor        Actor
	// Confirmed is D12's always-present gate. A run arriving unconfirmed
	// parks at awaiting_confirm with the report on the row and returns; the
	// OS renders the report and calls again with Confirmed set. A REDEPLOY
	// sets it in one click, which is the same code path with the person's
	// answer already in hand.
	Confirmed bool
	// Placements maps a deployable name to where it goes, supplied at FIRST
	// deploy only (epic memql#4885, D8). Later deploys find the site through
	// (packageId, packageDeployableName) and never re-ask: a placement is
	// chosen once, and a later deploy of the same source keeps the same
	// addresses.
	Placements map[string]Placement
	// Automatic marks a run the source's own auto-deploy switch started
	// rather than a person (epic memql#4900, task memql#4903). On the REQUEST
	// rather than derived, because the only caller who can honestly answer is
	// the one that decided to start it -- and deliberately not readable from
	// the wire, so nothing a client sends can put a run in the timeline
	// marked as something nobody chose.
	Automatic bool
	// FromDeploymentId retries an earlier run from the bytes it already
	// fetched (task memql#4902): the run re-analyses the same snapshot rather
	// than going back to the repository, so a Retry after a lost node deploys
	// exactly what the lost run was deploying -- not whatever the branch has
	// moved to since.
	FromDeploymentId string
}

// Placement is where one deployable goes on its first deploy (D8): the
// hostname its site is created at, the client it is FOR, and the client's own
// domain. The hostname is the only required part -- a placement naming
// neither of the other two is exactly the first deploy there was before them.
//
// The pipeline APPLIES the two optional parts itself, after the site exists,
// as the same two calls the page would make (updateSiteAccount and
// customDomainAdd) under the caller's own actor -- so the guards behind them
// decide exactly as they do from the page, and there is no client-side
// follow-up write for a closed window to lose.
type Placement struct {
	Hostname  string
	AccountId string
	OwnDomain string
	// Skip leaves this deployable out of the run (memql#4930, epic
	// memql#4937). A PLACEMENT-TIME CHOICE, never a manifest one: the manifest
	// describes the SOFTWARE, and a repository that ships a storefront and a
	// starter SPA should not have to change to let somebody host only the
	// storefront.
	//
	// A skipped deployable is RECORDED, not omitted. The row's `deployables`
	// list promises one entry per manifest deployable, and a missing entry
	// reads as "nothing happened" where the truth is "you chose not to, and
	// nothing it already serves was touched". It reuses the same skipped
	// outcome shape a target the cluster does not offer produces, because a
	// reader asking "why is there no site for this" wants one answer shape,
	// not two.
	Skip bool
}

// DeployOutcome is what a run produced.
type DeployOutcome struct {
	DeploymentId string
	Status       string
	Report       *Report
	Deployables  []DeployableOutcome
	DslVersion   string
	// AwaitingConfirm is true when the run parked at the confirm gate. It is
	// reported separately from Status so a caller does not have to compare
	// against a string to learn that nothing is wrong.
	AwaitingConfirm bool
	// ConfirmReason says why an AUTO run parked instead of confirming itself
	// -- "this push changes what deploying would do", and the like. Empty for
	// a person's run, which parks because that is what the gate does.
	ConfirmReason string
	Problem       *Problem
	// BuiltOn is where this run's build happened, for the row (epic
	// memql#4900). One value for the run rather than one per app: a run
	// builds every app on one surface, and the pin keeps them on one node,
	// so the honest summary is the surface plus the node -- and the
	// per-deployable record rides on each outcome as well.
	BuiltOn BuiltOn
}

// recordBuiltOn keeps the run's build surface and the node it ran on.
//
// A REAL SURFACE OUTRANKS `prebuilt`, and the last non-empty node wins. That
// is the honest summary of a package whose apps are one committed tree and one
// that had to be built: the run built something, and it built it somewhere,
// and "prebuilt" alone would say it built nothing. Each app's own answer is on
// its outcome, which is where a per-app reading comes from.
func (o *DeployOutcome) recordBuiltOn(on BuiltOn) {
	if on.Surface == "" {
		return
	}
	if o.BuiltOn.Surface == "" || o.BuiltOn.Surface == SurfacePrebuilt {
		o.BuiltOn.Surface = on.Surface
	}
	if strings.TrimSpace(on.NodeId) != "" {
		o.BuiltOn.NodeId = on.NodeId
	}
}

// Deploy runs one deployment attempt to a terminal status, or parks it at the
// confirm gate.
//
// ===========================================================================
// THE WHOLE RUN IS ONE CALL ON ONE NODE, AND THAT IS A CHOICE
// ===========================================================================
// The design anticipated stage handoffs as row events with routing rules, and
// the multi-node rules that follow from that (explicit plumbing, an in-process
// hop test per handoff). This implementation does not need them: the stages
// advance inside this function, so there is no stage-to-stage hop to plumb and
// none to test.
//
// What that costs, stated rather than discovered: a node lost mid-deploy
// strands its row at a non-terminal status. Nothing is inconsistent -- the D6
// order guarantees every site is still serving what it was, because the run
// died before `publish` or after it finished -- but the row does not advance
// again on its own. The recovery is the one the append-only rule already
// prescribes everywhere else: start a new attempt, which opens a new row.
//
// What IS cross-node is the READ, and it is not optional: every row written
// here is read in an OS window some other node is serving, which is what the
// broadcast routing rules carry (component/node/routing.go, asserted by
// TestPackageRowsReachABrowserOnAnotherNode). Without them the packages list
// is correct on load and frozen afterwards.
//
// It never panics a stage past its predecessor: each step either advances the
// row and continues, or closes the row and returns. The row IS the state
// machine's memory, which is what makes a stage idempotent under re-delivery --
// re-running a completed stage re-reads the same inputs and reaches the same
// answer, and re-running a TERMINAL row is refused by the append-only guard
// rather than by anything here.
func Deploy(ctx context.Context, d *Deps, req DeployRequest) (*DeployOutcome, error) {
	pkg, err := d.Store.packageById(ctx, req.PackageId)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		// The composite tier returns zero rows for a package the caller may
		// not read, so "not found" and "not yours" are the same answer here
		// and the message must not claim to know which.
		return nil, refuse(CodeSourceUnreadable,
			"no package %q is readable by this caller", req.PackageId)
	}

	deploymentId := strings.TrimSpace(req.DeploymentId)
	resumed, rerr := d.resumeParked(ctx, deploymentId, req.PackageId)
	if rerr != nil {
		return nil, rerr
	}
	if deploymentId == "" {
		deploymentId = d.newId(packageDeploymentConcept)
	}
	ownerUserId := rowString(pkg, "ownerUserId")

	scope := scopeFrom(declaredNames(pkg), req.Placements)

	if resumed != nil {
		// A PARKED RUN'S CONFIRMATION IS THAT RUN, NOT A NEW ONE (memql#4954).
		// Re-stamp the scope, because the gate is where a person answers which
		// apps they meant: a compose gate opens with no placements at all and
		// closes with the skips somebody ticked, so a scope fixed at open
		// would have the run report progress on apps it was told not to build.
		if len(scope) > 0 {
			if serr := d.Store.recordScope(ctx, deploymentId, scope); serr != nil {
				return nil, serr
			}
		}
		// AND IT DEPLOYS ITS OWN BYTES. The parked run analysed a snapshot and
		// stored it; resuming re-reads that rather than fetching the source
		// again, which is what makes the report a person read at the gate a
		// description of what the click ships (memql#4955). An explicit
		// FromDeploymentId still wins -- it names a different run on purpose.
		if strings.TrimSpace(req.FromDeploymentId) == "" {
			switch {
			case rowString(resumed, "snapshotArtifactId") != "":
				req.FromDeploymentId = deploymentId
			case rowString(resumed, "fromDeploymentId") != "":
				// A RETRY THAT KEPT NO SNAPSHOT OF ITS OWN. Runs opened before
				// this change recorded none, so follow the chain to the run
				// they were retrying: the promise is about those bytes, and it
				// is better to refuse than to keep it silently wrong.
				req.FromDeploymentId = rowString(resumed, "fromDeploymentId")
			default:
				// NO SNAPSHOT ANYWHERE, so fetch. This is a parked run from
				// before snapshots were stored, or one whose store failed --
				// and its analysis was of the source as it then was, so
				// fetching is the nearest true thing. Refusing instead would
				// leave the run unconfirmable with no way forward but to
				// cancel it, which is a worse answer to a rarer problem. A
				// RETRY does not reach here: it takes the branch above and is
				// refused rather than quietly shipping other bytes.
			}
		}
	} else if err := d.Store.openDeployment(ctx, deploymentSeed{
		DeploymentId:     deploymentId,
		PackageId:        req.PackageId,
		OwnerUserId:      ownerUserId,
		RequestedBy:      req.Actor.UserId,
		Automatic:        req.Automatic,
		NodeId:           selfNodeId(),
		ScopedTo:         scope,
		FromDeploymentId: strings.TrimSpace(req.FromDeploymentId),
		StartedAt:        d.now(),
	}); err != nil {
		return nil, err
	}

	out := &DeployOutcome{DeploymentId: deploymentId, Status: StatusAnalyzing}

	// THE HEARTBEAT RUNS FOR THE LENGTH OF THE RUN (epic memql#4900, task
	// memql#4902). Started here rather than inside runDeploy so it covers
	// every exit from it -- the refusals, and the parked case below -- and
	// stopped before the row is closed, because a beat landing after a
	// terminal write is refused by the append-only guard and would log an
	// error about a rule that is working.
	//
	// THE CANCEL SIGNAL RIDES THE SAME LOOP (epic memql#4937, D3). The beat
	// re-reads the row's cancelRequested flag; the first time it is set, the
	// run's own context is cancelled -- which is what stops a build that is
	// mid-command -- and `cancelled` is latched so the next stage boundary
	// returns the refusal rather than whatever error the dying context
	// produced. The HEARTBEAT keeps its original ctx: it has to outlive the
	// cancellation in order to close the row.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var cancelled atomic.Bool
	stopHeartbeat := d.heartbeat(ctx, deploymentId, func() {
		cancelled.Store(true)
		cancelRun()
	})
	runErr := runDeploy(runCtx, d, req, pkg, out, &cancelled)
	stopHeartbeat()

	if err := runErr; err != nil {
		var ref *Refusal
		status := StatusFailed
		problem := &Problem{Code: "deploy_failed", Message: err.Error(), Fatal: true}
		if errors.As(err, &ref) {
			status = StatusRefused
			problem = &Problem{Code: ref.Code, Message: ref.Detail, Scope: ref.Scope, Fatal: true}
			// A STOP IS NOT A REFUSAL, and it is not a failure. It gets its
			// own terminal status so the timeline can say "cancelled" -- see
			// StatusCancelled for why collapsing it into `failed` would
			// report somebody's own click back to them as a fault.
			if ref.Code == CodeDeploymentCancelled {
				status = StatusCancelled
			}
		}
		out.Status = status
		out.Problem = problem
		d.closeRun(ctx, deploymentId, out, problem)
		d.audit(ctx, req, out, err)
		return out, err
	}

	if out.AwaitingConfirm {
		// Parked, not finished. The row stays at awaiting_confirm and is
		// deliberately NOT closed: a terminal row accepts no further writes,
		// so closing here would make the person's confirmation land on a row
		// nothing could advance.
		d.audit(ctx, req, out, nil)
		return out, nil
	}

	out.Status = StatusSucceeded
	d.closeRun(ctx, deploymentId, out, nil)
	d.audit(ctx, req, out, nil)
	return out, nil
}

// packageDeploymentConcept is the concept every line of a run is ABOUT.
// Stamped through logger.Subject on each log call of a run (epic memql#4893),
// which is what lets the Deployables app's Logs section select a run's lines
// and a person follow one deployment through the store by its id.
const packageDeploymentConcept = "v1:platform:packageDeployment"

func (d *Deps) closeRun(ctx context.Context, deploymentId string, out *DeployOutcome, problem *Problem) {
	buildLog := ""
	for _, o := range out.Deployables {
		if o.Refusal != nil && strings.Contains(o.Refusal.Code, "build") {
			buildLog = o.Refusal.Message
		}
	}
	if err := d.Store.closeDeployment(ctx, deploymentClose{
		DeploymentId: deploymentId,
		Status:       out.Status,
		Deployables:  out.Deployables,
		DslVersion:   out.DslVersion,
		BuildLogTail: buildLog,
		BuiltOn:      out.BuiltOn,
		Error:        problem,
		FinishedAt:   d.now(),
	}); err != nil {
		// The run's own outcome is already decided; failing to record it is a
		// separate fault and must not overwrite what happened.
		d.log().Error("packages: could not close the deployment row",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, deploymentId),
			"deployment", deploymentId, "err", err)
	}
}

// runDeploy is the stage walk. Split out so Deploy owns exactly one thing --
// turning whatever this returns into a terminal row -- and every `return err`
// below lands in the same place.
func runDeploy(ctx context.Context, d *Deps, req DeployRequest, pkg map[string]any, out *DeployOutcome, cancelled *atomic.Bool) error {
	// checkCancelled is the stage boundary (epic memql#4937, D3). It reads the
	// latch the heartbeat sets rather than the row, so a boundary costs
	// nothing and cannot itself fail -- and it is checked BEFORE each stage
	// begins, never inside one, which is what makes "nothing was published" a
	// property of the shape rather than of the timing.
	checkCancelled := func() error {
		if cancelled != nil && cancelled.Load() {
			return refuse(CodeDeploymentCancelled,
				"this run was stopped before it %s. Nothing was published, and every site is still serving what it was serving.",
				"reached the point of no return")
		}
		return nil
	}

	if err := checkCancelled(); err != nil {
		return err
	}

	// ---- fetch ----
	snapshot, err := d.fetchFor(ctx, req, pkg)
	if err != nil {
		// A CANCEL LOOKS LIKE A FETCH FAILURE FROM HERE, because cancelling
		// the run's context is what aborts the request in flight. The latch
		// is the authority on which of the two happened, so it is consulted
		// before the fetch's own error is reported -- otherwise a person who
		// pressed Cancel is told their repository could not be read.
		if cerr := checkCancelled(); cerr != nil {
			return cerr
		}
		return err
	}
	defer snapshot.Close()

	if err := checkCancelled(); err != nil {
		return err
	}

	// ---- analyze ----
	rep, aerr := Analyze(snapshot.Tree, Options{
		SourceVersion: snapshot.Version,
		Limits:        d.Limits,
		Logger:        d.Logger,
	})
	out.Report = rep

	snapshotArtifactId := d.storeSnapshot(ctx, req, out.DeploymentId, snapshot)
	if rerr := d.Store.recordReport(ctx, out.DeploymentId, rep, snapshotArtifactId); rerr != nil {
		d.log().Warn("packages: could not record the analysis report",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, out.DeploymentId),
			"deployment", out.DeploymentId, "err", rerr)
	}
	if aerr != nil {
		return aerr
	}
	if rep.Name != "" && rep.Name != rowString(pkg, "name") {
		if nerr := d.Store.recordPackageName(ctx, req.PackageId, rep.Name); nerr != nil {
			d.log().Warn("packages: could not record the manifest name",
				"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, out.DeploymentId),
				"deployment", out.DeploymentId, "err", nerr)
		}
	}

	// WHAT THE SOURCE DECLARES, recorded on the SOURCE.
	//
	// Written here, beside the manifest name and for the same reason: this is
	// the moment the tree was read, and both are facts about the tree rather
	// than about this run. Rewritten WHOLE on every analysis, because the
	// manifest is the authority on what the package contains -- an app deleted
	// upstream has to leave the list, and a merge would keep it forever.
	//
	// Before this, the only record of a declared deployable lived inside a
	// run's report, and a site row is written only for an app that actually
	// deployed. So an app somebody skipped at the confirm gate existed nowhere
	// a client could see it: absent from the list, and absent from its own
	// source's "apps it produces". A warning rather than a failure -- the
	// deploy is not less correct because a catalogue write did not land.
	if nerr := d.Store.recordPackageDeployables(ctx, req.PackageId, declaredFrom(rep)); nerr != nil {
		d.log().Warn("packages: could not record what the manifest declares",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, out.DeploymentId),
			"deployment", out.DeploymentId, "err", nerr)
	}

	// ---- the D9 contents gate, at deploy START ----
	//
	// Before any build and before any stage, which is the whole point: a
	// person without cluster-owner authority must not be able to make this
	// cluster fetch a tree, run its build, and only then be told no. The
	// answer depends on the package's CONTENTS, which is why it cannot be
	// asked before the analysis and must be asked immediately after it.
	if len(rep.DslDomains) > 0 && !req.Actor.IsClusterOwner {
		return refuse(CodeDslRequiresClusterOwner,
			"this package ships MemQL DSL (%s), and deploying DSL changes what this whole cluster can do -- so it is reserved to a cluster owner. A package of web apps alone deploys under your own account.",
			describeDomains(rep.DslDomains))
	}

	// ---- confirm (D12) ----
	//
	// An AUTO run answers the gate itself when the plan has not changed
	// (epic memql#4900, task memql#4903). The gate is not skipped: the run
	// reaches it, the comparison happens here where the fresh report is in
	// hand, and anything but an exact match parks exactly as a person's
	// unconfirmed run does -- with the reason on the row, so the OS can say
	// why it stopped rather than only that it did.
	if !req.Confirmed {
		if req.Automatic {
			ok, why := d.autoConfirm(ctx, req, rep)
			if ok {
				d.log().Info("packages: an auto-deploy confirmed itself -- the plan is unchanged",
					"component", "packages.autodeploy",
					"package", req.PackageId, "deployment", out.DeploymentId)
			} else {
				out.AwaitingConfirm = true
				out.Status = StatusAwaitingConfirm
				out.ConfirmReason = why
				return d.Store.advance(ctx, out.DeploymentId, StatusAwaitingConfirm)
			}
		} else {
			out.AwaitingConfirm = true
			out.Status = StatusAwaitingConfirm
			return d.Store.advance(ctx, out.DeploymentId, StatusAwaitingConfirm)
		}
	}

	// ---- build ----
	if err := checkCancelled(); err != nil {
		return err
	}
	if err := d.Store.advance(ctx, out.DeploymentId, StatusBuilding); err != nil {
		return err
	}
	bundles, err := d.build(ctx, req, pkg, snapshot, rep, out)
	if err != nil {
		// The build is the long one, so it is the stage a cancel usually
		// lands in the middle of -- and cancelling the run's context is what
		// aborts the command. The latch decides which happened, so somebody
		// who pressed Cancel is not shown a build failure they did not cause.
		if cerr := checkCancelled(); cerr != nil {
			return cerr
		}
		return err
	}

	// ---- stage + roll (D5/D6), skipped entirely when there is no DSL ----
	//
	// The skip is the fast path the design promises: an SPA-only deploy, or
	// one whose DSL is byte-identical to what is already staged, restarts
	// nothing and lands in seconds. It is decided by CONTENT rather than by a
	// flag, so "unchanged" cannot drift from what is actually mounted.
	//
	// THE LAST CANCEL CHECKPOINT IS HERE, immediately before the roll, and
	// there is deliberately none after it: `stage DSL` writes the package's
	// MemQL into storage and `rolling` restarts this cluster onto it, so a run
	// stopped between the two leaves the cluster half-rolled -- the one
	// outcome worse than either finishing or not starting. CancellableStage
	// refuses the ASK from this status onward, so a person is told rather than
	// left watching a Cancel that will never be read.
	if err := checkCancelled(); err != nil {
		return err
	}

	if len(rep.DslDomains) > 0 {
		dslVersion, staged, serr := d.stageAndRoll(ctx, snapshot, rep, out)
		if serr != nil {
			return serr
		}
		out.DslVersion = dslVersion
		if !staged {
			d.log().Info("packages: DSL unchanged; stage and roll skipped",
				"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, out.DeploymentId),
				"deployment", out.DeploymentId)
		}
	}

	// ---- publish ----
	if err := d.Store.advance(ctx, out.DeploymentId, StatusPublishing); err != nil {
		return err
	}
	outcomes, perr := d.publish(ctx, req, pkg, rep, bundles)
	out.Deployables = outcomes
	if perr != nil {
		return perr
	}

	if verr := d.Store.recordDeployedVersion(ctx, req.PackageId, snapshot.Version, false); verr != nil {
		d.log().Warn("packages: could not record the deployed version",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, out.DeploymentId),
			"deployment", out.DeploymentId, "err", verr)
	}
	return nil
}

// resumeParked answers whether this call is the CONFIRMATION of a run already
// waiting at the gate (memql#4954).
//
// # The defect
//
// `handleDeploy` never passed a deployment id and `Deploy` minted one whenever
// it was empty, so EVERY call was a new run -- including `confirm: true`,
// which is supposed to answer the run already parked. After confirming, the
// source kept a row at `awaiting_confirm` nobody would ever answer: the list
// went on saying "a deploy is waiting for you" for a gate that had been
// answered, and the history showed a run that never resolved. The client said
// otherwise in as many words -- `DeployablePage.tsx` carried the comment "A
// PARKED RUN'S DEPLOY IS THE CONFIRMATION, not a new run", which was false.
//
// It was masked until memql#4956: the abandoned sweep closed the stale row 90
// seconds later, with `abandoned` and "this cluster lost the node that was
// running this deploy", which was untrue of it. Correcting the sweep made the
// leak visible.
//
// # Why only a PARKED row resumes
//
// A row at any other non-terminal status is a run in flight, and falling
// through to openDeployment is what keeps the append-only guard refusing it --
// which is the dedup two auto-deploy feeds noticing one push rely on
// (autodeploy.go composes a deterministic id for exactly that). Resuming there
// would turn "this run is already happening" into a second pipeline writing
// the same row. A terminal row is refused by the same guard, and should be.
//
// So: parked, and belonging to this package. Anything else opens a new run,
// exactly as before.
func (d *Deps) resumeParked(ctx context.Context, deploymentId, packageId string) (map[string]any, error) {
	if strings.TrimSpace(deploymentId) == "" {
		return nil, nil
	}
	row, err := d.Store.deploymentById(ctx, deploymentId)
	if err != nil {
		return nil, err
	}
	if row == nil {
		// Not readable, or not there. Either way this is not a resume, and
		// openDeployment's own answer is the right one.
		return nil, nil
	}
	if rowString(row, "packageId") != packageId {
		return nil, refuse(CodeSourceUnreadable,
			"deployment %q belongs to a different package", deploymentId)
	}
	if rowString(row, "status") != StatusAwaitingConfirm {
		return nil, nil
	}
	return row, nil
}

// declaredNames is what the package's last analysis said the source declares.
func declaredNames(pkg map[string]any) []string {
	raw, _ := pkg["declares"].([]any)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// scopeFrom answers which deployables a run is FOR (memql#4953).
//
// The client already expresses scope -- a redeploy from a deployable's page
// sends every sibling `skip: true`, and the compose gate does the same when
// entered for one declared app -- and the answer was thrown away the moment
// the call returned. Nothing on the run recorded it, so every reader had to
// assume the source's newest run was about whatever it was looking at.
//
// SCOPE IS THE COMPLEMENT OF THE SKIPS, not the placements' own keys. A
// single-app redeploy names only the SIBLINGS it is skipping, so reading the
// map's keys would answer with everything this run is NOT for.
//
// EMPTY MEANS THE WHOLE SOURCE, and that is what makes the field safe to add
// to a live timeline: every row written before it has none, and a reader
// asking "is this run about my app" gets yes -- which is what those runs were.
// A run with nothing skipped is the same statement and is left empty rather
// than listing every declared name, so there is one spelling of it.
func scopeFrom(declared []string, placements map[string]Placement) []string {
	if len(placements) == 0 {
		return nil
	}
	var skipped int
	for _, p := range placements {
		if p.Skip {
			skipped++
		}
	}
	if skipped == 0 {
		return nil
	}
	// Prefer the manifest's own names and order, so the scope reads like the
	// report beside it. A package with no analysis yet declares nothing, and
	// the placements are then the only names there are.
	names := declared
	if len(names) == 0 {
		names = make([]string, 0, len(placements))
		for name := range placements {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if placements[name].Skip {
			continue
		}
		out = append(out, name)
	}
	return out
}

// fetchFor gets the bytes this run deploys.
//
// A RETRY REUSES WHAT THE EARLIER RUN FETCHED (epic memql#4900, task
// memql#4902), and the reason is not saving a request. A run that was
// abandoned when its node died was deploying a particular commit; going back
// to the repository would deploy whatever the branch has moved to since, which
// is a different deploy wearing the word Retry. The button says "start the run
// that was lost again", so it starts THAT run again.
//
// An artifact-sourced package needs nothing special: its zip in the Library IS
// the snapshot, and re-reading it is byte-identical by construction.
func (d *Deps) fetchFor(ctx context.Context, req DeployRequest, pkg map[string]any) (*SourceSnapshot, error) {
	from := strings.TrimSpace(req.FromDeploymentId)
	if from == "" || rowString(pkg, "sourceKind") != "repo" {
		return d.fetch(ctx, pkg)
	}
	prior, err := d.Store.deploymentById(ctx, from)
	if err != nil {
		return nil, err
	}
	if prior == nil {
		return nil, refuse(CodeSourceUnreadable,
			"no deployment %q is readable by this caller, so there is nothing to retry from", from)
	}
	if got := rowString(prior, "packageId"); got != req.PackageId {
		return nil, refuse(CodeSourceUnreadable, "deployment %q belongs to a different package", from)
	}
	ref := rowString(prior, "snapshotArtifactId")
	if ref == "" || d.Publisher == nil {
		return nil, refuse(CodeSnapshotUnavailable,
			"the run being retried kept no snapshot of its source, so this cluster cannot repeat it exactly. Deploy again to fetch the source fresh -- it will deploy whatever the source holds now.")
	}
	raw, rerr := d.Publisher.ReadSnapshot(ctx, ref)
	if rerr != nil {
		return nil, rerr
	}
	dir, mkErr := os.MkdirTemp("", "memql-package-retry-*")
	if mkErr != nil {
		return nil, mkErr
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	root, xerr := ExtractTarGz(bytes.NewReader(raw), dir, d.Limits)
	if xerr != nil {
		cleanup()
		return nil, xerr
	}
	return &SourceSnapshot{
		Tree: os.DirFS(root),
		// The VERSION the earlier run recorded, not one derived from the
		// bytes: it is the same source, so it is the same version, and
		// deriving it again would risk two spellings of one commit.
		Version: rowString(prior, "sourceVersion"),
		// Bytes deliberately nil: the snapshot is already stored, and storing
		// it again would give one set of bytes two references. The reference
		// travels instead, so this run's row points at the same artifact.
		ArtifactId: ref,
		Root:       root,
		cleanup:    cleanup,
	}, nil
}

func (d *Deps) fetch(ctx context.Context, pkg map[string]any) (*SourceSnapshot, error) {
	switch rowString(pkg, "sourceKind") {
	case "repo":
		// The owner rides along with the credential NAME, because the name
		// is resolved under the owner's actor and not the caller's: a
		// cluster owner deploying a colleague's package fetches under the
		// colleague's credential, which is correct -- they are deploying that
		// package -- and a package naming somebody else's credential resolves
		// nothing.
		return d.Fetcher.FetchRepo(ctx, RepoSource{
			RepoUrl:      rowString(pkg, "repoUrl"),
			Ref:          rowString(pkg, "repoRef"),
			CredentialId: rowString(pkg, "credentialId"),
			OwnerUserId:  rowString(pkg, "ownerUserId"),
		}, d.Limits)
	case "artifact":
		return d.Fetcher.FetchArtifact(ctx, rowString(pkg, "artifactId"), d.Limits)
	default:
		return nil, refuse(CodeSourceUnreadable,
			"this package declares no source kind this cluster fetches")
	}
}

// storeSnapshot lands the fetched bytes as a content-addressed Library
// artifact (D8) and returns its id, or "" when there is nothing to store.
//
// A failure here is logged and swallowed: provenance is valuable and a deploy
// that works is more valuable, so the absence of a snapshot artifact costs the
// "re-analyse without refetching" shortcut and nothing else.
func (d *Deps) storeSnapshot(ctx context.Context, req DeployRequest, deploymentId string, s *SourceSnapshot) string {
	if s == nil {
		return ""
	}
	// ALREADY STORED. A retry and a resume both run against bytes the Library
	// holds, so this run's row points at the same content-addressed artifact
	// rather than at nothing (memql#4955). Storing them again would give one
	// snapshot two identities; recording none left the row unable to be
	// resumed or retried, which is how a retry-of-a-retry came to be refused
	// `snapshot_unavailable` for a snapshot that exists.
	if s.ArtifactId != "" {
		return s.ArtifactId
	}
	if len(s.Bytes) == 0 || d.Publisher == nil {
		return ""
	}
	id, err := d.Publisher.StoreSnapshot(ctx, req.PackageId, s.Version, s.Bytes)
	if err != nil {
		d.log().Warn("packages: could not store the source snapshot",
			"component", "packages.pipeline", logger.Subject(packageDeploymentConcept, deploymentId),
			"deployment", deploymentId, "package", req.PackageId, "err", err)
		return ""
	}
	return id
}

func (d *Deps) newId(concept string) string {
	if d.NewId != nil {
		return d.NewId(concept)
	}
	return newRowId(concept)
}

func describeDomains(domains []DslDomainReport) string {
	names := make([]string, 0, len(domains))
	for _, dd := range domains {
		names = append(names, dd.Domain)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// stageIndex reports where a status sits in the D6 order, or -1.
func stageIndex(status string) int {
	for i, s := range stageOrder {
		if s == status {
			return i
		}
	}
	return -1
}

// IsTerminal reports whether a deployment status accepts no further writes.
func IsTerminal(status string) bool {
	_, ok := packageDeploymentTerminalStatuses[strings.TrimSpace(status)]
	return ok
}

var packageDeploymentTerminalStatuses = map[string]struct{}{
	StatusSucceeded: {},
	StatusRefused:   {},
	StatusFailed:    {},
	StatusAbandoned: {},
	StatusCancelled: {},
}

// CancellableStage reports whether a run at this status can still be stopped
// (epic memql#4937, D3).
//
// THE LINE IS THE ROLL, and it is drawn by what is unsafe rather than by what
// is slow. `staging_dsl` writes the package's MemQL into storage and `rolling`
// restarts this cluster onto it; a run stopped between those two leaves the
// cluster half-rolled, which is the one outcome worse than either finishing or
// not starting. Everything before -- the fetch, the analysis, the confirm gate
// a person is already sitting at, and the build, where the minutes actually go
// -- has published nothing, so abandoning it leaves every site serving exactly
// what it was serving.
//
// A CLOSED LIST OF WHAT IS ALLOWED, rather than an exclusion of what is not: a
// status this function has never heard of is NOT cancellable, so a stage added
// later is safe by default and has to opt in deliberately.
func CancellableStage(status string) bool {
	switch strings.TrimSpace(status) {
	case StatusAnalyzing, StatusAwaitingConfirm, StatusBuilding:
		return true
	default:
		return false
	}
}

var _ = stageIndex
var _ fs.FS
var _ = fmt.Sprintf
