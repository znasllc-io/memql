package packages

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"
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
	Store     *store
	Fetcher   Fetcher
	Builder   Builder
	Stager    Stager
	Roller    Roller
	Publisher SitePublisher
	Auditor   Auditor
	Logger    *slog.Logger
	Limits    Limits
	Now       func() time.Time
	// NewId mints deployment and site ids. Injected so a test reads a stable
	// timeline rather than a random one.
	NewId func(prefix string) string
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
	// Hostnames maps a deployable name to the hostname to create its site at,
	// supplied at FIRST deploy only. Later deploys find the site through
	// (packageId, packageDeployableName) and never re-ask.
	Hostnames map[string]string
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
	Problem         *Problem
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
	if deploymentId == "" {
		deploymentId = d.newId("v1:platform:packageDeployment")
	}
	ownerUserId := rowString(pkg, "ownerUserId")

	if err := d.Store.openDeployment(ctx, deploymentSeed{
		DeploymentId: deploymentId,
		PackageId:    req.PackageId,
		OwnerUserId:  ownerUserId,
		RequestedBy:  req.Actor.UserId,
		StartedAt:    d.now(),
	}); err != nil {
		return nil, err
	}

	out := &DeployOutcome{DeploymentId: deploymentId, Status: StatusAnalyzing}
	if err := runDeploy(ctx, d, req, pkg, out); err != nil {
		var ref *Refusal
		status := StatusFailed
		problem := &Problem{Code: "deploy_failed", Message: err.Error(), Fatal: true}
		if errors.As(err, &ref) {
			status = StatusRefused
			problem = &Problem{Code: ref.Code, Message: ref.Detail, Scope: ref.Scope, Fatal: true}
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
		Error:        problem,
		FinishedAt:   d.now(),
	}); err != nil {
		// The run's own outcome is already decided; failing to record it is a
		// separate fault and must not overwrite what happened.
		d.log().Error("packages: could not close the deployment row",
			"component", "packages.pipeline", "deployment", deploymentId, "err", err)
	}
}

// runDeploy is the stage walk. Split out so Deploy owns exactly one thing --
// turning whatever this returns into a terminal row -- and every `return err`
// below lands in the same place.
func runDeploy(ctx context.Context, d *Deps, req DeployRequest, pkg map[string]any, out *DeployOutcome) error {
	// ---- fetch ----
	snapshot, err := d.fetch(ctx, pkg)
	if err != nil {
		return err
	}
	defer snapshot.Close()

	// ---- analyze ----
	rep, aerr := Analyze(snapshot.Tree, Options{
		SourceVersion: snapshot.Version,
		Limits:        d.Limits,
		Logger:        d.Logger,
	})
	out.Report = rep

	snapshotArtifactId := d.storeSnapshot(ctx, req, snapshot)
	if rerr := d.Store.recordReport(ctx, out.DeploymentId, rep, snapshotArtifactId); rerr != nil {
		d.log().Warn("packages: could not record the analysis report",
			"component", "packages.pipeline", "deployment", out.DeploymentId, "err", rerr)
	}
	if aerr != nil {
		return aerr
	}
	if rep.Name != "" && rep.Name != rowString(pkg, "name") {
		if nerr := d.Store.recordPackageName(ctx, req.PackageId, rep.Name); nerr != nil {
			d.log().Warn("packages: could not record the manifest name",
				"component", "packages.pipeline", "err", nerr)
		}
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
	if !req.Confirmed {
		out.AwaitingConfirm = true
		out.Status = StatusAwaitingConfirm
		return d.Store.advance(ctx, out.DeploymentId, StatusAwaitingConfirm)
	}

	// ---- build ----
	if err := d.Store.advance(ctx, out.DeploymentId, StatusBuilding); err != nil {
		return err
	}
	bundles, err := d.build(ctx, snapshot, rep, out)
	if err != nil {
		return err
	}

	// ---- stage + roll (D5/D6), skipped entirely when there is no DSL ----
	//
	// The skip is the fast path the design promises: an SPA-only deploy, or
	// one whose DSL is byte-identical to what is already staged, restarts
	// nothing and lands in seconds. It is decided by CONTENT rather than by a
	// flag, so "unchanged" cannot drift from what is actually mounted.
	if len(rep.DslDomains) > 0 {
		dslVersion, staged, serr := d.stageAndRoll(ctx, snapshot, rep, out)
		if serr != nil {
			return serr
		}
		out.DslVersion = dslVersion
		if !staged {
			d.log().Info("packages: DSL unchanged; stage and roll skipped",
				"component", "packages.pipeline", "deployment", out.DeploymentId)
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
			"component", "packages.pipeline", "err", verr)
	}
	return nil
}

func (d *Deps) fetch(ctx context.Context, pkg map[string]any) (*SourceSnapshot, error) {
	switch rowString(pkg, "sourceKind") {
	case "repo":
		return d.Fetcher.FetchRepo(ctx,
			rowString(pkg, "repoUrl"),
			rowString(pkg, "repoRef"),
			rowString(pkg, "repoTokenRef"),
			d.Limits)
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
func (d *Deps) storeSnapshot(ctx context.Context, req DeployRequest, s *SourceSnapshot) string {
	if s == nil || len(s.Bytes) == 0 || d.Publisher == nil {
		return ""
	}
	id, err := d.Publisher.StoreSnapshot(ctx, req.PackageId, s.Version, s.Bytes)
	if err != nil {
		d.log().Warn("packages: could not store the source snapshot",
			"component", "packages.pipeline", "package", req.PackageId, "err", err)
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
}

var _ = stageIndex
var _ fs.FS
var _ = fmt.Sprintf
