package compose

import (
	"archive/zip"
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// Materializing a DEPLOYABLE (epic issue memql#4980, design D8): the
// output is a package source zip the existing Deployables pipeline
// deploys unchanged at sourceKind="artifact".
//
// NOTHING NEW IS BUILT IN THE DEPLOY PATH, and that is the design. This
// package produces a zip; `component/packages` reads it exactly as it
// reads one somebody uploaded, and the hand-off is the Deployables
// compose flow opened at Source with the artifact already chosen. The
// alternative -- the Materializer calling packageDeploy itself -- would
// put a second deploy entry point in the product with its own ideas
// about addresses, accounts and confirmations, beside a rail the
// Deployables epic spent two passes making the one place a deploy is
// composed.
//
// THE MANIFEST IS THE CONTRACT, AND IT IS WRITTEN FROM THE ENGINE'S OWN
// CONSTANTS. `packages.ManifestName` is "memql-package.yaml" and the
// manifest must sit at the ROOT of the zip -- a package whose manifest
// is one directory down is refused `package_manifest_missing`. This
// writer does not import component/packages (that would make this pure
// module depend on the pipeline it feeds); it carries the two facts and
// a test in integrations/compose holds them equal to the real ones.

// manifestName is component/packages.ManifestName. Held here rather than
// imported so this module stays a leaf; deployable_parity_test.go in
// integrations/compose fails the build if the two ever disagree.
const manifestName = "memql-package.yaml"

// DeployableKind is the site kind a materialized package produces. The
// set is v1:platform:site.kind, which the Deployables app's
// OFFERED_KINDS is already held equal to by
// component/memql/site_kind_os_parity_test.go -- so this is the third
// reading of one list and the parity test is what stops it drifting.
type DeployableKind string

const (
	DeployableSPA        DeployableKind = "spa"
	DeployableStatic     DeployableKind = "static"
	DeployableStorefront DeployableKind = "shopify_storefront"
)

// DeployableKinds is every kind the Materializer offers, in the order
// the Target column presents them.
func DeployableKinds() []DeployableKind {
	return []DeployableKind{DeployableSPA, DeployableStatic, DeployableStorefront}
}

// ParseDeployableKind resolves a caller-supplied kind. An unknown one is
// an ERROR rather than a default, for ParseFormat's reason: defaulting
// to `static` would answer a request for a storefront with a folder of
// files and call it success.
func ParseDeployableKind(s string) (DeployableKind, error) {
	want := DeployableKind(strings.ToLower(strings.TrimSpace(s)))
	for _, k := range DeployableKinds() {
		if k == want {
			return k, nil
		}
	}
	if want == "ios" || want == "android" || want == "macos" {
		return "", fmt.Errorf("compose: %q is a distribution target rather than a hostname-resolved web surface, so this cluster has no site kind for it -- see docs/public/operate/deployables.md", want)
	}
	return "", fmt.Errorf("compose: unknown deployable kind %q (offered: spa, static, shopify_storefront)", s)
}

// DeployableFile is one file going into the package source zip.
type DeployableFile struct {
	// Path is relative to the deployable's own directory, with forward
	// slashes. A leading slash, a "." segment or any ".." is refused.
	Path string
	// Body is the file's bytes.
	Body []byte
}

// Deployable is one app inside the package.
type Deployable struct {
	// Name is the manifest name -- what the Deployables app calls this
	// app, and what a person types to confirm a destructive act on it.
	Name string
	Kind DeployableKind
	// Files are the app's own files, relative to its directory.
	Files []DeployableFile
	// BuildCommand and BuildOutput are optional. A materialized
	// deployable is normally PREBUILT -- this app composes finished
	// files rather than a source tree with a toolchain -- so leaving
	// them empty is the common case and draws Build as skipped, "its
	// built output is in the source", which is a reading the Deployables
	// rail already has and already explains.
	BuildCommand string
	BuildOutput  string
}

// PackageSource is what BuildPackageSource writes.
type PackageSource struct {
	// Name is the package's own name in the manifest.
	Name string
	// Deployables are the apps it declares, at least one.
	Deployables []Deployable
}

// BuildPackageSource writes a package source zip the Deployables
// pipeline reads unchanged.
//
// The provenance goes in TWO places and neither is the manifest: a
// PROVENANCE.md at the root, which is a file a person opens, and the
// zip's own comment, which survives being unpacked and re-zipped by
// nothing at all but costs one line. The manifest is a CONTRACT the
// pipeline parses, and adding unrecognised keys to a document another
// component validates is how a deploy starts failing on a field nobody
// remembers adding.
func BuildPackageSource(src PackageSource, p Provenance) (Result, error) {
	if strings.TrimSpace(src.Name) == "" {
		return Result{}, fmt.Errorf("compose: a package source needs a name")
	}
	if len(src.Deployables) == 0 {
		return Result{}, fmt.Errorf("compose: a package source needs at least one deployable -- an empty one is refused by the pipeline as having nothing to deploy")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	manifest, err := renderPackageManifest(src)
	if err != nil {
		return Result{}, err
	}
	// THE MANIFEST SITS AT THE ROOT. One directory down and the package
	// is refused package_manifest_missing -- the exact failure
	// component/packages/source.go's tarball comment records.
	if err := writeZipEntry(zw, manifestName, []byte(manifest)); err != nil {
		return Result{}, err
	}
	if err := writeZipEntry(zw, "PROVENANCE.md", packageProvenanceDoc(src, p)); err != nil {
		return Result{}, err
	}

	seen := map[string]bool{manifestName: true, "PROVENANCE.md": true}
	for _, d := range src.Deployables {
		dir, err := deployableDir(d.Name)
		if err != nil {
			return Result{}, err
		}
		for _, f := range d.Files {
			rel, err := safeRelPath(f.Path)
			if err != nil {
				return Result{}, fmt.Errorf("compose: %s: %w", d.Name, err)
			}
			full := dir + "/" + rel
			if seen[full] {
				// A duplicate entry produces a zip whose contents
				// depend on which one the reader picks, and readers
				// disagree. Refusing is the only answer that is the
				// same everywhere.
				return Result{}, fmt.Errorf("compose: %s appears twice in the package source", full)
			}
			seen[full] = true
			if err := writeZipEntry(zw, full, f.Body); err != nil {
				return Result{}, err
			}
		}
	}

	if err := zw.SetComment("Materialized by MemQL. Composition " + p.CompositionId + "."); err != nil {
		return Result{}, fmt.Errorf("compose: writing zip comment: %w", err)
	}
	if err := zw.Close(); err != nil {
		return Result{}, fmt.Errorf("compose: closing package source: %w", err)
	}

	// A ZIP HAS A METADATA CHANNEL AND THIS ONE USES IT, so the
	// Embedded flag is true -- but the note names the two places rather
	// than the format's usual one, because somebody looking for
	// provenance in a zip opens the file, not the archive header.
	return Result{
		Bytes:    buf.Bytes(),
		Embedded: true,
		Note:     "Provenance is in PROVENANCE.md at the root of the package and in the archive's own comment.",
	}, nil
}

// renderPackageManifest writes memql-package.yaml.
//
// HAND-WRITTEN RATHER THAN MARSHALLED, so this module needs no YAML
// dependency and stays a leaf. The document is four keys deep and every
// value is either a validated identifier or a quoted string, which is
// the case where hand-writing YAML is safe -- and quoting is
// unconditional so a name holding a colon cannot produce a document that
// parses as something else.
func renderPackageManifest(src PackageSource) (string, error) {
	var b strings.Builder
	b.WriteString("# Written by the MemQL Materializer. Deployed unchanged by the\n")
	b.WriteString("# Deployables pipeline as a sourceKind=artifact package.\n")
	b.WriteString("formatVersion: 1\n")
	b.WriteString("name: " + quoteYAML(src.Name) + "\n")
	b.WriteString("deployables:\n")
	for _, d := range src.Deployables {
		dir, err := deployableDir(d.Name)
		if err != nil {
			return "", err
		}
		if _, err := ParseDeployableKind(string(d.Kind)); err != nil {
			return "", fmt.Errorf("compose: deployable %q: %w", d.Name, err)
		}
		b.WriteString("  - name: " + quoteYAML(d.Name) + "\n")
		b.WriteString("    path: " + quoteYAML(dir) + "\n")
		b.WriteString("    kind: " + quoteYAML(string(d.Kind)) + "\n")
		if strings.TrimSpace(d.BuildCommand) != "" || strings.TrimSpace(d.BuildOutput) != "" {
			b.WriteString("    build:\n")
			if cmd := strings.TrimSpace(d.BuildCommand); cmd != "" {
				b.WriteString("      command: " + quoteYAML(cmd) + "\n")
			}
			if out := strings.TrimSpace(d.BuildOutput); out != "" {
				b.WriteString("      output: " + quoteYAML(out) + "\n")
			}
		}
	}
	return b.String(), nil
}

func packageProvenanceDoc(src PackageSource, p Provenance) []byte {
	var b strings.Builder
	b.WriteString("# " + src.Name + "\n\n")
	b.WriteString("Materialized by MemQL. This package was composed from rows in a memory\n")
	b.WriteString("graph rather than written by hand, and the record below says from what.\n\n")
	writeFact(&b, "Asked for", p.Statement)
	writeFact(&b, "By", p.AuthorName)
	writeFact(&b, "Instance", p.Instance)
	writeFact(&b, "Composition", p.CompositionId)
	writeFact(&b, "Goal", p.GoalId)
	writeFact(&b, "Template", p.TemplateName)
	writeFact(&b, "Sources", p.SourceSummary())
	writeFact(&b, "Models", p.ModelSummary())
	writeFact(&b, "Made", p.CreatedAt.UTC().Format(time.RFC3339))
	b.WriteString("\n## Apps in this package\n\n")
	for _, d := range src.Deployables {
		dir, _ := deployableDir(d.Name)
		b.WriteString("- **" + d.Name + "** (" + string(d.Kind) + ") in `" + dir + "/`\n")
	}
	return []byte(b.String())
}

func writeFact(b *strings.Builder, label, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString("- **" + label + ":** " + value + "\n")
}

// deployableDir is the directory an app's files live in, derived from
// its manifest name. Derived rather than caller-supplied so the manifest
// `path` and the zip entries cannot disagree -- a package whose manifest
// points at a directory the zip does not contain fails at build time
// with a message about a missing path rather than about the mistake.
func deployableDir(name string) (string, error) {
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf("compose: deployable name %q has no characters a directory name can use", name)
	}
	return "apps/" + slug, nil
}

func slugify(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// safeRelPath refuses every path shape that would let a package source
// write outside its own directory when unpacked.
//
// THIS IS A ZIP SLIP GUARD, and the line it draws is between a path
// that means something ELSE after cleaning and one that means the SAME
// thing. An escape is REFUSED, never sanitised: silently rewriting
// "../../etc/passwd" to "etc/passwd" produces a package that deploys
// something nobody asked for, while refusing produces an error naming
// the file. A redundant separator ("a//b.html") is NORMALISED, because
// cleaning it changes nothing about which file lands where -- and the
// one hazard it does carry, two spellings of one entry, is caught by
// the caller's duplicate check, which compares the CLEANED paths.
func safeRelPath(p string) (string, error) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", fmt.Errorf("a file with no path")
	}
	if strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("path %q uses backslashes; zip entries are forward-slashed", p)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path %q is absolute", p)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the package", p)
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" {
			return "", fmt.Errorf("path %q has an empty segment", p)
		}
	}
	return cleaned, nil
}

// writeZipEntry writes one entry with a FIXED modification time.
//
// THE FIXED TIME IS WHAT MAKES A RE-RUN BYTE-IDENTICAL. A zip stamped
// with time.Now() differs on every run, so two runs of one recipe over
// unchanged sources would produce two different digests -- and the whole
// claim this product makes about replay is that the second run costs
// nothing and produces the same thing. Deployables' own content
// addressing keys on the digest, so a moving one would re-stage bytes
// that had not changed.
func writeZipEntry(zw *zip.Writer, name string, body []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("compose: package entry %s: %w", name, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("compose: package entry %s: %w", name, err)
	}
	return nil
}

// SortDeployables orders a package's apps by name so a re-run writes
// entries in the same order. Callers hand us whatever order a browser's
// form produced, and zip entry order is part of the bytes.
func SortDeployables(ds []Deployable) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
}
