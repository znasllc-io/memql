package packages

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// autodeploy.go is the per-package switch that makes the click automatic when
// nothing about the plan changed (epic memql#4900, task memql#4903).
//
// ===========================================================================
// THE SWITCH DOES NOT SKIP THE GATE. IT ANSWERS IT.
// ===========================================================================
// D12's confirm gate is always present, and this does not remove it: an
// auto-run parks at `awaiting_confirm` exactly like a person's, and is then
// confirmed by the engine ONLY when the new report plans exactly what the last
// confirmed run planned. Anything else -- a new app, a new DSL domain, a
// changed build command, a problem -- stays parked, the OS's waiting mark
// lights, and a person answers it.
//
// That is the whole safety argument, and it is worth being precise about what
// it buys: the gate exists so nobody deploys something they have not seen.
// A plan a person has already seen and said yes to, arriving again unchanged
// against new source, is a thing they have seen.
//
// ===========================================================================
// WHAT COUNTS AS "THE SAME PLAN"
// ===========================================================================
// Not the same SOURCE -- the source moved, that is why we are here. The plan
// is what deploying WOULD DO: which apps, of which kind, from which path,
// built how, into which output; which DSL domains; and whether every app
// already has an address. A change to any of those is a change a person would
// want to see, and the ones that are easy to miss are the dangerous ones -- a
// build command edited in the manifest is somebody else's shell command
// arriving on your cluster.
//
// The comparison is a STRING, deliberately: a fingerprint of the plan, built
// in one place, so "the same plan" has exactly one definition and a new field
// on the report is a compile-time decision about whether it belongs in it.

// PlanFingerprint is what a person said yes to, as one comparable value.
//
// SORTED and explicit, never a JSON dump of the report: a dump would fold in
// sourceVersion (which always changes), problem messages (which carry
// timestamps), and every field a future epic adds -- so it would answer "the
// plan changed" every time, and the switch would silently never fire.
func PlanFingerprint(rep *Report) string {
	if rep == nil {
		return ""
	}
	// Three parts, always: apps, dsl, ok. Sized by what is APPENDED rather
	// than by the report's own lengths -- summing two caller-supplied lengths
	// to pre-size an allocation is the shape that overflows, and it was
	// sizing for the wrong thing anyway.
	parts := make([]string, 0, 3)
	apps := make([]string, 0, len(rep.Deployables))
	for _, d := range rep.Deployables {
		problem := ""
		if d.Problem != nil {
			// The CODE, not the message: a message can name a path or a count
			// and is written for a reader, while the code is the fact. Two
			// runs whose app is broken in the same way have the same plan --
			// and it is a plan that parks either way.
			problem = d.Problem.Code
		}
		apps = append(apps, strings.Join([]string{
			d.Name, d.Kind, d.Path, d.Command, d.Output,
			boolWord(d.Prebuilt, "prebuilt", "builds"),
			bindingWord(d.Binding),
			problem,
		}, "|"))
	}
	sort.Strings(apps)
	parts = append(parts, "apps="+strings.Join(apps, ";"))

	domains := make([]string, 0, len(rep.DslDomains))
	for _, dd := range rep.DslDomains {
		// The domain NAME and whether it is reserved. Not the construct
		// counts: a domain that gained a query is a change to what the
		// cluster can do, and it is the same DOMAIN being staged -- the
		// cluster-owner gate that governs DSL already ran, and re-parking on
		// every construct edit would make the switch useless for exactly the
		// packages that ship DSL.
		domains = append(domains, dd.Domain+"|"+boolWord(dd.Reserved, "reserved", "ok"))
	}
	sort.Strings(domains)
	parts = append(parts, "dsl="+strings.Join(domains, ";"))
	parts = append(parts, "ok="+boolWord(rep.OK, "yes", "no"))
	return strings.Join(parts, " ")
}

func boolWord(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func bindingWord(b *ManifestBinding) string {
	if b == nil {
		return ""
	}
	// The store domain and the token NAME. The name is not a secret and it is
	// the thing that changed if a storefront was re-pointed at another
	// credential, which is a change worth stopping for.
	return b.StoreDomain + ">" + b.StorefrontTokenRef
}

// autoConfirm decides whether a parked auto-run may confirm itself.
//
// It compares against the last CONFIRMED run -- the newest one that actually
// succeeded -- rather than against the last run of any kind. A run that was
// refused, failed or abandoned was never a plan anybody approved, so treating
// it as the baseline would let a package auto-confirm a plan whose only
// previous outing was a failure.
func (d *Deps) autoConfirm(ctx context.Context, req DeployRequest, rep *Report) (bool, string) {
	prior, err := d.Store.lastSucceededDeployment(ctx, req.PackageId)
	if err != nil {
		return false, "this cluster could not read what the last successful deploy of this source planned, so the run is waiting for you"
	}
	if prior == nil {
		return false, "this source has never finished a deploy, so its first one is yours to confirm"
	}
	before := PlanFingerprint(reportFromRow(prior))
	if before == "" {
		return false, "the last successful deploy of this source recorded no plan to compare against, so the run is waiting for you"
	}
	if before != PlanFingerprint(rep) {
		return false, "this push changes what deploying would do, so the run is waiting for you"
	}
	return true, ""
}

// startAutoRun is what the update feeds call when a source they watch moves
// and its switch is on.
//
// ===========================================================================
// UNDER THE OWNER, AND NEVER MORE THAN ONE
// ===========================================================================
// The run is REQUESTED BY the package's owner, because it is their deploy:
// the sites it publishes are theirs, the credential it fetches under is
// theirs, and the audit trail should name them rather than a synthetic actor
// nobody can ask about. The feeds run on a schedule with nobody attached, so
// the actor is borrowed the same way openDeployment borrows it -- off a
// package row this cluster already resolved.
//
// The one-live-run rule is not an optimisation either. Two pushes a second
// apart would otherwise open two runs against the same source, and the second
// would publish over the first mid-flight: the D6 order protects a site from a
// FAILED deploy, not from a second successful one racing it.
func (d *Deps) startAutoRun(ctx context.Context, pkg map[string]any, version string) (started bool, err error) {
	packageId := rowString(pkg, "id")
	owner := rowString(pkg, "ownerUserId")
	if packageId == "" || !rowBool(pkg, "autoDeploy") {
		return false, nil
	}
	if owner == "" {
		// A cluster-owned package has nobody to run as. It keeps the update
		// cue and waits for a click, which is the behaviour it had before the
		// switch existed.
		return false, nil
	}
	// EVERY read and write from here runs as the OWNER, including the
	// live-run check: the composite tier would answer zero rows under the
	// schedule's own blank actor, and "no run is live" is exactly the wrong
	// thing to conclude from a read that could not see one.
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	live, lerr := d.Store.liveDeploymentsForPackage(ownerCtx, packageId)
	if lerr != nil {
		return false, lerr
	}
	if len(live) > 0 {
		d.log().Info("packages: an auto-deploy was skipped because a run is already live",
			"component", "packages.autodeploy", "package", packageId, "live", len(live))
		return false, nil
	}
	out, derr := Deploy(ownerCtx, d, DeployRequest{
		PackageId: packageId,
		// The DEPLOYMENT ID IS DERIVED FROM THE VERSION, so two feeds noticing
		// the same push -- the webhook and the ten-minute poll -- open one run
		// rather than two. The second call finds a row at that id and the
		// append-only guard refuses to reopen it, which is a refusal that
		// means "already handled".
		DeploymentId: autoDeploymentId(packageId, version),
		Actor:        Actor{UserId: owner},
		Automatic:    true,
	})
	if derr != nil {
		return false, derr
	}
	if out != nil && out.AwaitingConfirm {
		d.log().Info("packages: an auto-deploy parked at the confirm gate because the plan changed",
			"component", "packages.autodeploy", "package", packageId, "deployment", out.DeploymentId)
	}
	return true, nil
}

// autoDeploymentId is the id an auto-run opens at.
//
// Derived rather than minted, which is what makes "never more than one
// auto-run per push" true without a lock: both feeds compose the same id for
// the same version, and the second attempt lands on a row that already exists.
func autoDeploymentId(packageId, version string) string {
	return fmt.Sprintf("v1:platform:packageDeployment:auto-%s-%s", shortId(packageId), safeVersion(version))
}

// safeVersion bounds a version into an id segment. A tag is a person's string
// and can hold anything; an id is not.
func safeVersion(version string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(version) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
