// Package packages is the offline analysis that decides whether a tree is a
// deployable MemQL package, and what deploying it would do.
//
// Nothing becomes a package by claiming to be one (design D1). Every deploy
// runs this analysis FIRST, offline, with no cluster and no side effects, and
// either produces a report a person can read at the confirm gate or refuses
// with a stable machine-readable code they can act on. The pipeline
// (integrations/packages) calls it; the OS renders what it returns.
//
// The offline half is the point. "This DSL would refuse boot" is an analysis
// refusal here (D12), never a crashlooping node discovered three stages later
// -- so the gates this package runs are literally the gates strict boot runs,
// reached through the memqllint machinery rather than reimplemented.
package packages

import "fmt"

// The refusal catalogue (design section E), stable and machine-readable in the
// sitePublishFromArtifact tradition.
//
// STABILITY IS THE CONTRACT. These strings cross three boundaries -- they land
// on packageDeployment.report, ride the wire to the OS, and are keyed there to
// decide which sentence renders in-surface. Renaming one silently turns a
// known refusal into an unknown one, which the OS renders as the raw server
// message rather than its own copy; that is the designed fallback for a fault
// nobody anticipated, not a place to land a rename.
//
// This package owns the whole catalogue, INCLUDING the two codes it does not
// itself raise (dsl_requires_cluster_owner, package_has_active_deployables).
// The pipeline and the lifecycle law raise those, and they are declared here
// because a catalogue split across three packages is a catalogue that grows a
// fourth ad-hoc string the first time somebody cannot find it.
const (
	// -- manifest (section B) --

	// CodeManifestMissing: no memql-package.yaml at the tree root.
	CodeManifestMissing = "package_manifest_missing"
	// CodeManifestInvalid: unparseable, an unknown key, a missing name, a
	// formatVersion this engine does not know, or a deployable name that is
	// empty or repeated. An unknown version REFUSES rather than best-effort
	// parsing: a format that grew a field whose absence changes behaviour
	// would otherwise deploy something the author did not describe. An
	// unknown KEY refuses for the same reason one step down -- `deployabels:`
	// parses fine and describes a package with nothing in it, which would
	// deploy successfully and do nothing.
	//
	// The name rules live under this code rather than under codes of their
	// own because they are manifest VALIDITY, and section E's catalogue is
	// closed: a rule that arrives without a code in the design gets the
	// nearest catalogued one, never a new string invented at the call site.
	CodeManifestInvalid = "package_manifest_invalid"

	// -- declared deployables (section B) --

	// CodeDeployablePathMissing: a declared path does not exist, or is not a
	// directory, inside the tree.
	CodeDeployablePathMissing = "deployable_path_missing"
	// CodeDeployableKindUnknown: kind is not one of the three live values.
	CodeDeployableKindUnknown = "deployable_kind_unknown"
	// CodeDeployableBindingMissing: a shopify_storefront without its binding.
	CodeDeployableBindingMissing = "deployable_binding_missing"
	// -- discovered DSL (section B) --

	// CodeDslDomainReserved: a discovered dsl/<domain>/ collides with a core
	// engine namespace. The engine's own mount SKIPS such a directory
	// silently (dsl.MountOverlayDomains) -- correct at boot, where the
	// embedded tree must win, and wrong here, where the author believes they
	// shipped that domain. Analysis refuses instead.
	CodeDslDomainReserved = "dsl_domain_reserved"
	// CodeDslRefusesBoot: the package's DSL does not survive the Init-grade
	// gates strict boot runs. Carries the construct-level errors boot would
	// print.
	CodeDslRefusesBoot = "dsl_refuses_boot"

	// -- source form (D1) --

	// CodeSourceTooLarge: the snapshot exceeds the publisher-grade limits.
	CodeSourceTooLarge = "source_too_large"
	// CodeBundlePathInvalid: a zip entry escaping the tree root, absolute, or
	// otherwise not a valid path. Shares its spelling with
	// sitePublishFromArtifact's code for the same condition on purpose: it is
	// the same refusal about the same kind of archive, and one vocabulary
	// across both is what lets the OS render one sentence.
	CodeBundlePathInvalid = "bundle_path_invalid"
	// CodeSourceUnreadable: the fetched snapshot is not an archive this
	// engine can open at all. Spelled in the source_* family beside
	// source_too_large because it is about the SOURCE snapshot rather than a
	// site bundle -- sitePublishFromArtifact's bundle_unreadable is the same
	// condition one layer down, on a different object.
	CodeSourceUnreadable = "source_unreadable"

	// -- reported, not fatal (D3) --

	// CodeGoPackNotDeployable: a bff/ with a go.mod. Reported per-half and
	// NON-fatal: the rest of the package deploys, and the report says where
	// Go delivery actually happens today. Full Go delivery is its own epic.
	CodeGoPackNotDeployable = "go_pack_not_deployable"

	// -- raised outside this package, catalogued here --

	// CodeDslRequiresClusterOwner (D9): raised by the pipeline at deploy
	// start, before any build or stage.
	CodeDslRequiresClusterOwner = "dsl_requires_cluster_owner"
	// CodePackageHasActiveDeployables (D10): raised by the lifecycle law when
	// archiving a package whose sites are not all archived.
	CodePackageHasActiveDeployables = "package_has_active_deployables"
)

// Refusal is an analysis or pipeline failure carrying a stable Code.
//
// It is an error so it can travel the ordinary Go path, and it carries the
// code separately so no caller has to parse a message to key on it.
type Refusal struct {
	// Code is one of the catalogue constants above.
	Code string
	// Detail is the sentence a person reads. The OS renders the server's
	// sentence VERBATIM, in-surface, so this text is the product copy for
	// this failure -- write it for the person who typed the repo URL.
	Detail string
	// Scope names the half the refusal is about when the package has halves:
	// a deployable name, a dsl domain, a path. Empty for package-wide.
	Scope string
}

func (r *Refusal) Error() string {
	if r.Scope == "" {
		return fmt.Sprintf("%s: %s", r.Code, r.Detail)
	}
	return fmt.Sprintf("%s (%s): %s", r.Code, r.Scope, r.Detail)
}

// refuse builds a package-wide Refusal.
func refuse(code, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// refuseScoped builds a Refusal about one half of the package.
func refuseScoped(code, scope, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Scope: scope, Detail: fmt.Sprintf(format, a...)}
}

// RefusalCode reports the stable code carried by err, or "" when err is not a
// Refusal. Exposed so callers -- and tests -- key on the NAME rather than on
// the prose, which is what makes the prose safe to improve.
func RefusalCode(err error) string {
	if r, ok := err.(*Refusal); ok {
		return r.Code
	}
	return ""
}
