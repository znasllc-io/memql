package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/edge"
)

// ---------------------------------------------------------------------------
// build (D4)
// ---------------------------------------------------------------------------

// The build surfaces a deployable can be built on -- the value that lands on
// the deployment row's `builtOn.surface` and the sentence the Build stop reads.
//
// A CLOSED SET of three, and the third is not a lesser member: a package whose
// output is committed is BUILT, by whoever's CI produced it, and saying
// "prebuilt" is what makes a deploy that took four seconds legible as a fast
// deploy rather than as a build that did not happen.
const (
	SurfacePrebuilt  = "prebuilt"
	SurfaceWorkbench = "workbench"
	SurfaceFleet     = "fleet"
)

// BuiltOn is where a deployable was built: the surface, and the node that ran
// it.
//
// The NODE is on the row because it is the only durable answer to "where did
// this build happen" once the directory it happened in is gone -- and it is
// gone by the time the row closes, deliberately. Empty for the prebuilt path,
// which ran nowhere.
type BuiltOn struct {
	Surface string `json:"surface"`
	NodeId  string `json:"nodeId,omitempty"`
}

// BuildResult is one deployable's built output.
type BuildResult struct {
	// Bundle is the flat path -> content map the publisher wants.
	Bundle edge.Bundle
	// LogTail is a bounded tail of the build output, captured whether the
	// build succeeded or failed. On failure it is what answers "why did my
	// build fail" inside the OS rather than in a pod log nobody can reach.
	LogTail string
	// BuiltOn is the surface and node this build ran on. Populated even on a
	// FAILURE -- "the build failed on workbench-7" is a more useful sentence
	// than "the build failed", and on a two-replica workbench it is the
	// difference between a bad build script and one sick replica.
	BuiltOn BuiltOn
}

// BuildRun is what a build needs to know about the RUN it belongs to, beside
// the deployable it is building.
//
// It exists because the three things here are facts about the attempt rather
// than about the app: the deployment keys the build's directory and is the
// subject its log lines carry, the owner is who the build is being run for,
// and the pin is the workbench replica an earlier deployable of this same run
// already built on. Passing them as a struct rather than as three arguments is
// what lets a fourth arrive without every implementation changing shape.
type BuildRun struct {
	DeploymentId string
	OwnerUserId  string
	// PinnedNodeId is the node the previous deployable of this run built on,
	// or empty for the first. A PREFERENCE and never a requirement: a replica
	// that has gone away since must not strand the second app of a package.
	PinnedNodeId string
	// Limits are the pipeline's own, passed in rather than re-read, so the
	// build surface can never enforce a bound the analysis did not.
	Limits Limits
}

// Builder runs one deployable's build. The production implementation is the
// workbench (D4): sandboxed, resource-capped, and with NO cluster credentials
// in its environment -- a package's build script is somebody else's code, and
// it runs where that is assumed rather than hoped.
type Builder interface {
	Build(ctx context.Context, run BuildRun, snapshot *SourceSnapshot, dep DeployableReport) (BuildResult, error)
}

// build walks the deployables, skipping the ones the analysis already found
// prebuilt (the D4 fast path).
func (d *Deps) build(ctx context.Context, req DeployRequest, pkg map[string]any, snapshot *SourceSnapshot, rep *Report, out *DeployOutcome) (map[string]edge.Bundle, error) {
	bundles := map[string]edge.Bundle{}
	run := BuildRun{
		DeploymentId: out.DeploymentId,
		// The PACKAGE's owner, never the caller's. A cluster owner deploying
		// somebody's package builds it for that person -- which is the same
		// rule the fetch already follows for the credential, and the same
		// reason: the run is about their package.
		OwnerUserId: rowString(pkg, "ownerUserId"),
		Limits:      d.Limits,
	}
	for _, dep := range rep.Deployables {
		if dep.Problem != nil {
			continue // already refused by the analysis; the run is failing anyway
		}
		// SKIPPED BEFORE ANY WORK (memql#4930). The choice is honoured at the
		// BUILD stage as well as at publish, so skipping the app somebody did
		// not want also saves the minutes of building it -- which is most of
		// what they were avoiding. Publish records the outcome; there is
		// nothing to record here, because nothing ran.
		if req.Placements[dep.Name].Skip {
			continue
		}
		if dep.Prebuilt {
			// Read the built tree straight out of the snapshot. No build, no
			// workbench, no restart -- and no network, which is what makes a
			// prebuilt package deployable on a cluster with no build surface
			// configured at all.
			bundle, err := bundleFromTree(snapshot.Tree, path.Join(dep.Path, dep.Output), d.Limits)
			if err != nil {
				return nil, err
			}
			bundles[dep.Name] = bundle
			out.recordBuiltOn(BuiltOn{Surface: SurfacePrebuilt})
			continue
		}
		builder := d.builderFor(dep)
		if builder == nil {
			return nil, refuse(CodeSourceUnreadable,
				"deployable %q needs a build (%s) and this cluster has no build surface configured. Commit the built output into the package, or configure the workbench.",
				dep.Name, dep.Command)
		}
		res, err := builder.Build(ctx, run, snapshot, dep)
		// RECORDED WHETHER IT WORKED OR NOT, and before the error is examined:
		// where a build ran is a fact about the attempt, and the case where
		// somebody most needs it is the one where it failed.
		out.recordBuiltOn(res.BuiltOn)
		if nodeId := strings.TrimSpace(res.BuiltOn.NodeId); nodeId != "" {
			// The next deployable of this run prefers the same replica: its
			// npm cache is warm and, more to the point, one run reads as one
			// place in the log store.
			run.PinnedNodeId = nodeId
		}
		if err != nil {
			out.Deployables = append(out.Deployables, DeployableOutcome{
				Name:    dep.Name,
				BuiltOn: res.BuiltOn,
				Refusal: &Problem{Code: buildRefusalCode(err), Message: buildFailureMessage(dep, res, err), Scope: dep.Name, Fatal: true},
			})
			return nil, refuseScoped(buildRefusalCode(err), dep.Name, "%s", buildFailureMessage(dep, res, err))
		}
		bundles[dep.Name] = res.Bundle
	}
	return bundles, nil
}

// builderFor picks the surface a deployable builds on (task memql#4904).
//
// The TARGET decides, not the pipeline: a deployable whose target declares a
// fleet build surface goes to a machine in the owner's Fleet, and everything
// else goes to the workbench. Web declares `workbench`, so today every
// registered target takes the first branch and the second ships with its hop
// test and no consumer -- which is the shape the epic asked for, and the
// reason the choice is a lookup rather than an `if kind == ...`.
func (d *Deps) builderFor(dep DeployableReport) Builder {
	if BuildSurfaceFor(dep.Kind) == SurfaceFleet {
		return d.FleetBuilder
	}
	return d.Builder
}

// buildRefusalCode keeps a typed refusal from the build surface as itself, and
// gives everything else the build-failed code.
//
// The distinction is what the Build stop renders: `no_workbench_peer` is an
// operator fact about this cluster and `deployable_build_timeout` is a fact
// about this build, and neither is helped by being told "the build failed".
func buildRefusalCode(err error) string {
	switch code := RefusalCode(err); code {
	case CodeNoWorkbenchPeer, CodeDeployableBuildTimeout, CodeNoWorkerAvailable:
		return code
	default:
		return CodeDeployableBuildFailed
	}
}

// buildFailureMessage is the sentence the OS renders verbatim.
//
// A TYPED refusal from the build surface is already written for the person who
// typed the repo URL -- it names the command, the timeout, or the missing
// peer, and carries the tail -- so it is passed through as itself. Anything
// else is an error from a layer that was not writing product copy, and gets
// the frame plus the tail.
func buildFailureMessage(dep DeployableReport, res BuildResult, err error) string {
	if ref, ok := err.(*Refusal); ok && strings.TrimSpace(ref.Detail) != "" {
		return ref.Detail
	}
	msg := fmt.Sprintf("the build for %q failed (%s): %v", dep.Name, dep.Command, err)
	if tail := strings.TrimSpace(res.LogTail); tail != "" {
		msg += "\n\n" + tail
	}
	return msg
}

// bundleFromTree reads a built output directory out of the snapshot, under the
// same limits the publisher enforces.
func bundleFromTree(tree fs.FS, root string, limits Limits) (edge.Bundle, error) {
	limits = limits.normalized()
	bundle := edge.Bundle{}
	var total int64
	err := fs.WalkDir(tree, path.Clean(root), func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, rerr := filepathRel(path.Clean(root), p)
		if rerr != nil {
			return rerr
		}
		data, derr := fs.ReadFile(tree, p)
		if derr != nil {
			return derr
		}
		if int64(len(data)) > limits.MaxFileBytes {
			return refuse(CodeSourceTooLarge, "%q is larger than %d bytes", rel, limits.MaxFileBytes)
		}
		total += int64(len(data))
		if total > limits.MaxSourceBytes {
			return refuse(CodeSourceTooLarge, "the built output exceeds %d bytes", limits.MaxSourceBytes)
		}
		if len(bundle) >= limits.MaxFileCount {
			return refuse(CodeSourceTooLarge, "the built output holds more than %d files", limits.MaxFileCount)
		}
		bundle[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

func filepathRel(base, p string) (string, error) {
	base = strings.TrimSuffix(path.Clean(base), "/")
	p = path.Clean(p)
	if base == "." {
		return p, nil
	}
	if !strings.HasPrefix(p, base+"/") {
		return "", fmt.Errorf("packages: %q is not under %q", p, base)
	}
	return strings.TrimPrefix(p, base+"/"), nil
}

// ---------------------------------------------------------------------------
// stage + roll (D5/D6)
// ---------------------------------------------------------------------------

// ActiveSetPath is the well-known pointer document: domain -> content-addressed
// prefix. One small JSON object, rewritten atomically, read by the dsl-bundle
// init-container's fetcher mode before a node boots.
//
// A POINTER rather than a manifest of files, and that is what makes the roll
// reversible: rolling back the DSL half is rewriting this one document and
// rolling, which is the same shape as a site rollback -- one write, pointing
// at bytes that still exist.
const ActiveSetPath = "packages/active.json"

// Stager writes content-addressed DSL trees and rewrites the active-set
// pointer.
type Stager interface {
	// StageDomain writes one domain's tree under packages/<domain>/<hash>/ and
	// returns the prefix. Idempotent by construction: the same tree produces
	// the same hash and therefore the same prefix, so re-staging overwrites
	// bytes with themselves.
	StageDomain(ctx context.Context, domain string, tree fs.FS) (prefix string, err error)
	ReadActiveSet(ctx context.Context) (map[string]string, error)
	WriteActiveSet(ctx context.Context, set map[string]string) error
}

// Roller drives the rolling restart that makes a staged tree live.
type Roller interface {
	Roll(ctx context.Context, reason string) error
}

// stageAndRoll writes the package's DSL and, IF ANYTHING CHANGED, flips the
// pointer and rolls.
//
// The "if anything changed" is not an optimisation. A roll is a restart of
// every DSL-consuming node, so performing one for a deploy that changed no DSL
// would make an SPA-only redeploy the most disruptive thing a person can do --
// the exact opposite of what D6 promises.
func (d *Deps) stageAndRoll(ctx context.Context, snapshot *SourceSnapshot, rep *Report, out *DeployOutcome) (string, bool, error) {
	if d.Stager == nil {
		return "", false, refuse(CodeSourceUnreadable,
			"this package ships DSL and this cluster has no DSL staging surface configured")
	}
	if err := d.Store.advance(ctx, out.DeploymentId, StatusStagingDsl); err != nil {
		return "", false, err
	}

	current, err := d.Stager.ReadActiveSet(ctx)
	if err != nil {
		return "", false, err
	}
	next := map[string]string{}
	for k, v := range current {
		next[k] = v
	}

	changed := false
	prefixes := make([]string, 0, len(rep.DslDomains))
	for _, domain := range rep.DslDomains {
		sub, serr := fs.Sub(snapshot.Tree, path.Join(DslRoot, domain.Domain))
		if serr != nil {
			return "", false, serr
		}
		prefix, perr := d.Stager.StageDomain(ctx, domain.Domain, sub)
		if perr != nil {
			return "", false, perr
		}
		prefixes = append(prefixes, prefix)
		if next[domain.Domain] != prefix {
			next[domain.Domain] = prefix
			changed = true
		}
	}
	sort.Strings(prefixes)
	dslVersion := strings.Join(prefixes, " ")

	if !changed {
		return dslVersion, false, nil
	}

	if err := d.Store.advance(ctx, out.DeploymentId, StatusRolling); err != nil {
		return "", false, err
	}
	// THE POINTER FLIP COMES FIRST, THEN THE ROLL. A roll against an
	// unwritten pointer restarts every node onto the tree it already had,
	// which looks like a successful deploy that changed nothing.
	if err := d.Stager.WriteActiveSet(ctx, next); err != nil {
		return "", false, err
	}
	if d.Roller == nil {
		return "", false, refuse(CodeSourceUnreadable,
			"this cluster has no rollout surface configured, so the staged DSL cannot be made live")
	}
	if err := d.Roller.Roll(ctx, "package deploy "+out.DeploymentId); err != nil {
		// Break-glass is written into the message rather than left to a
		// runbook: the pointer has moved and the roll has not, so the cluster
		// is serving the old trees with a new pointer -- recoverable by
		// rolling again, or by putting the pointer back.
		return "", false, fmt.Errorf(
			"the DSL was staged and the active-set pointer was updated, but the rolling restart failed: %w. Nothing is serving the new DSL yet; roll again, or revert the pointer", err)
	}
	return dslVersion, true, nil
}

// hashTree is the content address of a DSL domain's tree.
//
// Over the sorted (path, content) pairs rather than over a tarball, because a
// tarball carries timestamps and ordering that would make the same tree hash
// differently on two fetches -- which would stage a new prefix and roll the
// cluster for a deploy that changed nothing.
func hashTree(tree fs.FS) (string, map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(tree, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, derr := fs.ReadFile(tree, p)
		if derr != nil {
			return derr
		}
		files[p] = data
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:32], files, nil
}

// ---------------------------------------------------------------------------
// publish
// ---------------------------------------------------------------------------

// PublishResult is one deployable's published site.
type PublishResult struct {
	SiteId    string
	Hostname  string
	BundleRef string
	Version   string
	Created   bool
}

// SitePublisher is the publish half: create a site on first deploy, republish
// on later ones, and store a source snapshot in the Library.
type SitePublisher interface {
	// EnsureSite finds the site this package deployable published to last
	// time, or creates it (draft) at the requested hostname. Reports whether
	// it created one.
	EnsureSite(ctx context.Context, req EnsureSiteRequest) (siteId, hostname string, created bool, err error)
	// PublishBundle uploads under a fresh content-addressed prefix and only
	// then flips the site row -- so a failed publish leaves the site serving
	// exactly what it was serving.
	PublishBundle(ctx context.Context, siteId string, bundle edge.Bundle) (PublishResult, error)
	// RepointSite points a site back at a bundle version that already exists.
	// THE rollback operation, and it is the same write updateSiteBundle makes
	// forward -- which is the whole reason bundles live under versioned
	// content-addressed prefixes rather than being overwritten.
	RepointSite(ctx context.Context, siteId, bundleRef string) error
	// StoreSnapshot lands a fetched source archive as a content-addressed
	// Library artifact (D8).
	StoreSnapshot(ctx context.Context, packageId, version string, raw []byte) (artifactId string, err error)
	// ReadSnapshot reads back what StoreSnapshot wrote (epic memql#4900,
	// task memql#4902) -- the bytes a retry re-analyses rather than fetching
	// the source again.
	//
	// Declared BESIDE the write rather than on the Fetcher, because it is the
	// same object under the same key by the same client: a reader on the
	// fetch side would be a second answer to where a snapshot lives, and the
	// two would drift the first time either moved.
	ReadSnapshot(ctx context.Context, ref string) ([]byte, error)
}

// EnsureSiteRequest is what creating or finding a deployable's site needs.
type EnsureSiteRequest struct {
	PackageId      string
	DeployableName string
	Kind           string
	Hostname       string
	Binding        *ManifestBinding
	OwnerUserId    string
}

func (d *Deps) publish(ctx context.Context, req DeployRequest, pkg map[string]any, rep *Report, bundles map[string]edge.Bundle) ([]DeployableOutcome, error) {
	existing, err := d.Store.sitesForPackage(ctx, req.PackageId)
	if err != nil {
		return nil, err
	}
	byName := map[string]map[string]any{}
	for _, row := range existing {
		byName[rowString(row, "packageDeployableName")] = row
	}

	outcomes := make([]DeployableOutcome, 0, len(rep.Deployables))
	for _, dep := range rep.Deployables {
		// A DELIBERATE SKIP IS AN OUTCOME, NOT AN ABSENCE (memql#4930).
		//
		// It is recorded before the bundle lookup, because the build stage
		// produced no bundle for it and the branch below would otherwise read
		// it as an app that quietly did not happen. The refusal shape is the
		// one a not-offered target already uses -- one answer shape for "there
		// is no site for this, and here is why" -- with its own sentence, and
		// NOT Fatal: a partial deploy is a complete run.
		//
		// It also short-circuits deployable_binding_missing below, which is
		// the case memql#4930 calls out: an app that has never been deployed
		// and is being skipped has no hostname, and refusing the run for that
		// would make "deploy only the storefront" impossible on a first
		// deploy -- the exact situation the feature is for.
		if req.Placements[dep.Name].Skip {
			outcomes = append(outcomes, DeployableOutcome{
				Name: dep.Name,
				Refusal: &Problem{
					Code:    CodeDeployableSkipped,
					Message: "You chose not to deploy this one. Nothing was built for it, and anything it already serves is untouched.",
					Scope:   dep.Name,
				},
			})
			continue
		}
		bundle, ok := bundles[dep.Name]
		if !ok {
			// An app the build stage skipped over a NON-fatal problem -- a
			// target the model knows and does not offer (D9) -- is recorded
			// on the row with that problem rather than omitted. The row's
			// `deployables` promises one entry per manifest deployable, and
			// a missing entry reads as "nothing happened" where the truth
			// is "skipped, and here is why". A FATAL problem never reaches
			// this loop: the analysis refused the run before the build.
			if dep.Problem != nil && !dep.Problem.Fatal {
				skipped := *dep.Problem
				outcomes = append(outcomes, DeployableOutcome{Name: dep.Name, Refusal: &skipped})
			}
			continue
		}

		siteId := rowString(byName[dep.Name], "id")
		hostname := rowString(byName[dep.Name], "hostname")
		created := false
		outcome := DeployableOutcome{Name: dep.Name}
		if siteId == "" {
			placement := req.Placements[dep.Name]
			requested := strings.TrimSpace(placement.Hostname)
			if requested == "" {
				return outcomes, refuseScoped(CodeDeployableBindingMissing, dep.Name,
					"deployable %q has never been deployed and no hostname was chosen for it. The first deploy picks a hostname; later ones remember it.",
					dep.Name)
			}
			siteId, hostname, created, err = d.Publisher.EnsureSite(ctx, EnsureSiteRequest{
				PackageId:      req.PackageId,
				DeployableName: dep.Name,
				Kind:           dep.Kind,
				Hostname:       requested,
				Binding:        dep.Binding,
				OwnerUserId:    rowString(pkg, "ownerUserId"),
			})
			if err != nil {
				return outcomes, err
			}
			if berr := d.Store.bindSiteToPackage(ctx, siteId, req.PackageId, dep.Name); berr != nil {
				return outcomes, berr
			}
			d.place(ctx, siteId, dep.Name, placement, &outcome)
		}

		res, perr := d.Publisher.PublishBundle(ctx, siteId, bundle)
		if perr != nil {
			outcome.SiteId = siteId
			outcome.Refusal = &Problem{Code: "deployable_publish_failed", Message: perr.Error(), Scope: dep.Name, Fatal: true}
			outcomes = append(outcomes, outcome)
			return outcomes, perr
		}
		outcome.SiteId = siteId
		outcome.Hostname = firstNonEmpty(res.Hostname, hostname)
		outcome.BundleRef = res.BundleRef
		outcome.Version = res.Version
		outcome.Created = created
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// place applies the two optional halves of a first-deploy placement (D8) --
// the client the site is FOR and the client's own domain -- and records on
// the outcome what was applied and what was refused.
//
// BOTH RUN UNDER THE CALLER'S ACTOR, UNSTAMPED, and that is the whole
// authorization shape of the feature: they are the SAME two calls the page
// issues (updateSiteAccount, customDomainAdd), so the account write's guard
// and the three custom-domain guards (platform_custom_domain_policy.go)
// decide exactly as they do from the page. The pipeline gains no bypass of
// either; it only saves the person a second click.
//
// A REFUSAL DOES NOT FAIL THE PUBLISH. The site is live at its cluster
// address either way, and a hostname collision or a per-site cap is a fact
// about the domain, not about the deploy -- so it lands on the outcome with
// the server's own sentence, for the Where-it-lives stop to render, and the
// deploy goes on to publish. Recorded rather than logged, because a row is
// what the person reads and a pod log is not.
func (d *Deps) place(ctx context.Context, siteId, name string, p Placement, out *DeployableOutcome) {
	if accountId := strings.TrimSpace(p.AccountId); accountId != "" {
		if err := d.Store.setSiteAccount(ctx, siteId, accountId); err != nil {
			out.AccountRefusal = &Problem{Code: CodeDeployableAccountRefused, Message: err.Error(), Scope: name}
		} else {
			out.AccountId = accountId
		}
	}
	if own := strings.TrimSpace(p.OwnDomain); own != "" {
		if err := d.Store.addCustomDomain(ctx, siteId, own); err != nil {
			out.DomainRefusal = &Problem{Code: CodeDeployableDomainRefused, Message: err.Error(), Scope: name}
		} else {
			out.OwnDomain = own
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// marshalActiveSet renders the pointer document. Sorted keys, so two writers
// producing the same set produce the same bytes and a diff of the document is
// a diff of the deploy.
func marshalActiveSet(set map[string]string) ([]byte, error) {
	if set == nil {
		set = map[string]string{}
	}
	return json.MarshalIndent(set, "", "  ")
}
