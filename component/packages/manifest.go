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
// fronts. Only shopify_storefront declares one, and it carries a token REF
// rather than a token: the value names a v1:platform:globalSecret and is
// resolved at serve time by the edge, which is the pattern the site concept's
// own `binding` field already documents.
type ManifestBinding struct {
	StoreDomain        string `yaml:"storeDomain,omitempty"        json:"storeDomain,omitempty"`
	StorefrontTokenRef string `yaml:"storefrontTokenRef,omitempty" json:"storefrontTokenRef,omitempty"`
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
