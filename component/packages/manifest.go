package packages

import (
	"errors"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestName is the one file that makes a tree a package (design D2). At the
// tree root, and at the ZIP root for the archive form -- the two source forms
// are the same tree, so they are the same rule.
const ManifestName = "memql-package.yaml"

// ManifestFormatVersion is the only format this engine reads.
//
// The field exists so the format can grow; refusing an unknown value is what
// makes that growth safe. A future engine reading a `2` it understands is the
// intended path, and THIS engine reading a `2` refuses by name rather than
// parsing the subset it recognizes and deploying a package described by rules
// it never saw.
const ManifestFormatVersion = 1

// Deployable kinds, mirroring v1:platform:site.kind exactly. The manifest
// cannot offer a kind the site row cannot hold, so this list is not
// independently maintained -- it is that enum, and
// TestManifestKindsMatchSiteConcept pins the two together.
const (
	KindSPA        = "spa"
	KindStatic     = "static"
	KindStorefront = "shopify_storefront"
)

// UnofferedTarget is a kind the target model has written down and not
// registered (design section B, epic memql#4885 D9): what a person sees it
// called, and where the design's table says it will go.
type UnofferedTarget struct {
	// Display is the name the not-offered sentence is built from -- "iOS",
	// never the manifest's `ios`.
	Display string
	// Address is the address stop the target will carry once it is offered,
	// so the sentence can say where the app is headed rather than only that
	// it is not going anywhere today.
	Address string
}

// KnownUnofferedKinds are the manifest kinds the engine KNOWS and does not
// OFFER. They are deliberately NOT in v1:platform:site.kind
// (TestSiteKindEnumIsExactlyThreeValues): a site is a hostname the edge
// resolves, and a store listing is not one, so an ios site row would be a
// value that never resolves. The three live here instead so the analysis can
// tell the truth about them -- "not offered yet", scoped to the app and not
// fatal to the package -- rather than filing `ios` beside `banana` as a kind
// nobody has heard of, which would tell an author their roadmap item is a typo.
var KnownUnofferedKinds = map[string]UnofferedTarget{
	"ios":     {Display: "iOS", Address: "a bundle id and an App Store Connect app"},
	"android": {Display: "Android", Address: "an application id and a Play listing"},
	"macos":   {Display: "macOS", Address: "a bundle id, and a notarized disk image or the Mac App Store"},
}

// Manifest is memql-package.yaml.
//
// It describes the SOFTWARE and never its placement: there is no hostname and
// no slug here (D2). A hostname is chosen once, at first deploy, and
// remembered on the site row -- so the same manifest deploys to a staging
// instance and a production one without an edit, and a person renaming their
// site does not have to send a pull request to the package to keep deploying.
type Manifest struct {
	FormatVersion int                  `yaml:"formatVersion" json:"formatVersion"`
	Name          string               `yaml:"name"          json:"name"`
	Deployables   []ManifestDeployable `yaml:"deployables"   json:"deployables"`
}

// ManifestDeployable is one declared web surface inside the package.
//
// DECLARED, not discovered, and that asymmetry with DSL domains is deliberate
// (D2): a directory holding a package.json could be a site, a component
// library, or tooling, and its KIND -- whether a mistyped path 404s or falls
// back to index.html -- and its storefront BINDING are facts no walk can
// recover. DSL domains carry both facts in their own layout, so they are
// discovered exactly as the engine's own MEMQL_DSL_PATH mount discovers them.
type ManifestDeployable struct {
	Name    string           `yaml:"name"    json:"name"`
	Path    string           `yaml:"path"    json:"path"`
	Kind    string           `yaml:"kind"    json:"kind"`
	Build   *ManifestBuild   `yaml:"build,omitempty"   json:"build,omitempty"`
	Binding *ManifestBinding `yaml:"binding,omitempty" json:"binding,omitempty"`
	// Assets are immutable files imported after build, at paths relative to
	// the published site. Analysis reads these declarations, never the bytes.
	Assets []ManifestAsset `yaml:"assets,omitempty" json:"assets,omitempty"`
	// ResolutionTail is what the edge answers for a path matching no file in
	// this deployable's built output: "fallback" (index.html) or "not_found"
	// (404). OMITTED means the kind decides, which is every manifest written
	// before memql#5535 and the overwhelming majority after.
	//
	// IT IS HERE BECAUSE OTHERWISE THE CHOICE IS UNREACHABLE. A package
	// deploy is the only way most sites are created, so a site field no
	// manifest can declare is a field the product cannot use -- and the
	// product this exists for is a multi-page prerendered storefront that
	// declared `kind: static` precisely to get the 404 back, giving up the
	// store binding and the policy that admits Shopify along with it.
	//
	// SET ON CREATE ONLY, like Kind: EnsureSite finds an existing site by
	// (packageId, deployableName) and returns it untouched, so changing this
	// in the manifest does not rewrite a deployed site's row.
	// updateSiteResolutionTail is how an existing one is changed.
	//
	// BINDING IS NO LONGER IN THIS SENTENCE (epic memql#5530). A redeploy
	// re-points a storefront whose manifest now names a different store,
	// because the store is what the source is ABOUT rather than a property of
	// a site somebody deployed -- a manifest saying one store while the site
	// serves another is a storefront quietly talking to the wrong merchant.
	ResolutionTail string `yaml:"resolutionTail,omitempty" json:"resolutionTail,omitempty"`
}

// ManifestBuild overrides the defaults. The zero value IS the default pair, so
// a deployable that omits the block entirely gets `npm ci && npm run build`
// into `dist` -- which is what the overwhelming majority of trees want, and
// what makes the block optional rather than ceremonial.
type ManifestBuild struct {
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
	Output  string `yaml:"output,omitempty"  json:"output,omitempty"`
}

// DefaultBuildCommand and DefaultBuildOutput are the D4 defaults.
const (
	DefaultBuildCommand = "npm ci && npm run build"
	DefaultBuildOutput  = "dist"
)

// ManifestBinding is the per-kind connection to the system a deployable
// fronts. Only shopify_storefront declares one, and since epic memql#5530 it
// NAMES the store rather than describing it: `store` is a v1:shopify:store
// row's myshopify.com domain, which the pipeline resolves to a row id at
// deploy and writes onto the site as {storeId}.
//
// WHY THE DOMAIN AND NOT THE ROW ID. A manifest is committed to a product's
// repository and read by whoever deploys it; a row id is a fact about one
// cluster's database and means nothing in another. The myshopify.com domain is
// the one identifier Shopify never changes and the one an operator can check
// by eye. It is not a hostname in the sense the manifest refuses -- that rule
// is about THIS cluster's addresses, which are chosen at deploy.
//
// It still carries no secret. The Storefront token reference moved to the
// store row with the domain; the manifest names neither.
type ManifestBinding struct {
	Store string `yaml:"store,omitempty" json:"store,omitempty"`
}

// ReadManifest reads and validates the manifest at the root of tree.
//
// Every failure is a *Refusal carrying a catalogued code, so the caller never
// has to distinguish "no manifest" from "bad manifest" by inspecting an error
// string -- the OS keys on the code and renders its own sentence for each.
func ReadManifest(tree fs.FS) (*Manifest, error) {
	raw, err := fs.ReadFile(tree, ManifestName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, refuse(CodeManifestMissing,
				"no %s at the root of this source. A MemQL package is a tree with a manifest at its root describing what to deploy; add one and try again.",
				ManifestName)
		}
		return nil, refuse(CodeManifestInvalid, "%s could not be read: %v", ManifestName, err)
	}

	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// KnownFields turns a typo into a refusal instead of a silent omission.
	// `deployabels:` parses cleanly as an unknown key, leaves Deployables
	// empty, and describes a package that deploys nothing while reporting
	// success -- the exact silent-success shape this repo refuses everywhere.
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, refuse(CodeManifestInvalid, "%s is not valid: %v", ManifestName, err)
	}

	if m.FormatVersion != ManifestFormatVersion {
		if m.FormatVersion == 0 {
			return nil, refuse(CodeManifestInvalid,
				"%s does not declare a formatVersion. Add `formatVersion: %d`.",
				ManifestName, ManifestFormatVersion)
		}
		return nil, refuse(CodeManifestInvalid,
			"%s declares formatVersion %d, which this cluster does not read (it reads %d). Upgrade the cluster, or write the manifest in the format it knows.",
			ManifestName, m.FormatVersion, ManifestFormatVersion)
	}

	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return nil, refuse(CodeManifestInvalid,
			"%s does not declare a name. The name is how this package is listed and found after it is deployed.",
			ManifestName)
	}

	seen := make(map[string]struct{}, len(m.Deployables))
	for i := range m.Deployables {
		d := &m.Deployables[i]
		d.Name = strings.TrimSpace(d.Name)
		d.Path = strings.TrimSpace(d.Path)
		d.Kind = strings.TrimSpace(d.Kind)
		d.ResolutionTail = strings.TrimSpace(d.ResolutionTail)
		if d.Name == "" {
			return nil, refuse(CodeManifestInvalid,
				"deployable #%d in %s has no name. Names identify a deployable across deploys -- they are how a redeploy finds the site it published last time.",
				i+1, ManifestName)
		}
		if _, dup := seen[d.Name]; dup {
			return nil, refuse(CodeManifestInvalid,
				"%s declares two deployables named %q. A name identifies the site a redeploy republishes, so two deployables cannot share one.",
				ManifestName, d.Name)
		}
		seen[d.Name] = struct{}{}
		if err := validateAssets(d.Assets); err != nil {
			return nil, refuse(CodeManifestInvalid, "deployable %q: %v", d.Name, err)
		}
		// REFUSED HERE RATHER THAN IGNORED AT SERVE TIME. The edge reads an
		// unrecognised tail as absent, deliberately -- a typo must not take a
		// live site's every client-side route dark. But that is the rule for a
		// row already written; a manifest is read BEFORE anything is created,
		// and an author who wrote `resolutionTail: 404` deserves to be told so
		// rather than to deploy a site that quietly falls back for the life of
		// the package.
		if !ValidResolutionTail(d.ResolutionTail) {
			return nil, refuse(CodeManifestInvalid,
				"deployable %q in %s declares resolutionTail %q. It is %q (serve index.html), %q (answer 404), or omitted, which lets the deployable's kind decide.",
				d.Name, ManifestName, d.ResolutionTail, ResolutionTailFallback, ResolutionTailNotFound)
		}
	}

	return &m, nil
}

// BuildPlanFor reports the command and output directory a deployable builds
// with, applying the D4 defaults for anything the manifest leaves out.
func (d ManifestDeployable) BuildPlanFor() (command, output string) {
	command, output = DefaultBuildCommand, DefaultBuildOutput
	if d.Build != nil {
		if c := strings.TrimSpace(d.Build.Command); c != "" {
			command = c
		}
		if o := strings.TrimSpace(d.Build.Output); o != "" {
			output = o
		}
	}
	return command, output
}

// ValidKind reports whether kind is one of the three live site kinds.
func ValidKind(kind string) bool {
	switch kind {
	case KindSPA, KindStatic, KindStorefront:
		return true
	}
	return false
}

// Resolution tails a manifest may name, mirroring v1:platform:site.resolutionTail
// exactly. The manifest cannot offer a tail the site row cannot hold.
const (
	ResolutionTailFallback = "fallback"
	ResolutionTailNotFound = "not_found"
)

// ValidResolutionTail reports whether tail is one a site row can hold. THE
// EMPTY STRING IS VALID and is the ordinary state: it means the kind decides,
// which is what every manifest written before memql#5535 says by saying
// nothing.
func ValidResolutionTail(tail string) bool {
	switch tail {
	case "", ResolutionTailFallback, ResolutionTailNotFound:
		return true
	}
	return false
}

// ResolutionTailIsSet reports whether tail names a tail at all, as opposed to
// leaving the decision to the kind. The predicate exists so the one caller
// that has to decide whether to write the argument reads as what it means,
// rather than as a bare `!= ""` that a later reader could invert.
func ResolutionTailIsSet(tail string) bool {
	return tail == ResolutionTailFallback || tail == ResolutionTailNotFound
}
