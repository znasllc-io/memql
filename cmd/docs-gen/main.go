// docs-gen builds the documentation memql.io serves. Two subcommands:
//
//	docs-gen catalog [-out docs/public/reference/_generated]
//	docs-gen bundle  -version X.Y.Z [-root .] [-out DIR] [-tarball PATH]
//	                 [-site-url https://memql.io] [-allowlist PATH] [-check]
//
// catalog writes the concept catalog -- every concept the embedded DSL
// defines, with its fields -- to <out>/concepts.md. It loads the DSL without a
// database (memql.LoadUnifiedConcepts), so the catalog cannot drift from the
// engine it was built from.
//
// bundle builds the release docs bundle, contract v2 (package bundle,
// docs/DOCS_STANDARD.md section 5): the concept catalog rendered in process
// as reference/concepts.md, the selected docs/public pages with their links
// rewritten, manifest.json, memql-docs-version, llms.txt, llms-full.txt and
// sitemap-docs.xml, packed as <root>/docs-<version>.tgz. -check runs the
// selection and the boundary check and writes nothing. It prints one JSON
// summary line on stdout; everything else goes to stderr.
// scripts/docs/build-docs-bundle.sh is the capability script that runs it.
//
// Exit codes: 0 ok | 2 usage (bad flag, version, output dir) | 4 precondition
// (not a git checkout, a shallow clone) | 5 the build failed | 6 the public
// boundary check failed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/cmd/docs-gen/bundle"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Exit codes, stable: scripts/docs/build-docs-bundle.sh passes them through.
const (
	exitOK           = 0
	exitUsage        = 2
	exitPrecondition = 4
	exitFailed       = 5
	exitViolations   = 6
)

// jsonSchema is the slice of a concept's "definition" JSON Schema we render.
type jsonSchema struct {
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

type schemaProp struct {
	Type        any    `json:"type"` // JSON Schema type: string or []string
	Description string `json:"description"`
	Enum        []any  `json:"enum"`
}

func typeString(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " \\| ")
	}
	return ""
}

func cell(s string) string {
	// keep table cells single-line + pipe-safe
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// renderConceptCatalog renders the full concept catalog markdown for the
// given concepts (assumed already loaded). Separated from main for testing.
func renderConceptCatalog(concepts []*memoryNodes.Concept) string {
	sorted := make([]*memoryNodes.Concept, len(concepts))
	copy(sorted, concepts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: Concept Catalog\n")
	b.WriteString("description: Every concept the engine's DSL defines, with each field's type, whether it is required, and its relationships.\n")
	b.WriteString("audience: public\n")
	b.WriteString("status: stable\n")
	b.WriteString("area: reference\n")
	b.WriteString("sinceVersion: 0.9.0\n")
	b.WriteString("owner: znas\n")
	b.WriteString("---\n\n")
	b.WriteString("# Concept Catalog\n\n")
	b.WriteString("Generated from the live DSL by `cmd/docs-gen` -- do not hand-edit.\n")
	b.WriteString("A MemQL node is an instance of one of these concepts; each concept's\n")
	b.WriteString("fields below are its schema.\n\n")
	fmt.Fprintf(&b, "Total: **%d** concepts.\n\n", len(sorted))

	for _, c := range sorted {
		fmt.Fprintf(&b, "## `%s`\n\n", c.Name)
		if c.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(c.Description))
		}
		if def, ok := c.Schemas["definition"]; ok {
			var s jsonSchema
			if err := json.Unmarshal(def, &s); err == nil && len(s.Properties) > 0 {
				req := make(map[string]bool, len(s.Required))
				for _, r := range s.Required {
					req[r] = true
				}
				names := make([]string, 0, len(s.Properties))
				for k := range s.Properties {
					names = append(names, k)
				}
				sort.Strings(names)
				b.WriteString("| Field | Type | Required | Description |\n|---|---|---|---|\n")
				for _, name := range names {
					p := s.Properties[name]
					required := ""
					if req[name] {
						required = "yes"
					}
					desc := p.Description
					if len(p.Enum) > 0 {
						vals := make([]string, 0, len(p.Enum))
						for _, e := range p.Enum {
							vals = append(vals, fmt.Sprintf("%v", e))
						}
						if desc != "" {
							desc += " "
						}
						desc += "(enum: " + strings.Join(vals, ", ") + ")"
					}
					fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", name, typeString(p.Type), required, cell(desc))
				}
				b.WriteString("\n")
			}
		}
		if len(c.Relationships) > 0 {
			rels := make([]string, 0, len(c.Relationships))
			for _, r := range c.Relationships {
				if r.TargetConcept != "" {
					rels = append(rels, fmt.Sprintf("`%s` -> `%s`", r.Type, r.TargetConcept))
				}
			}
			if len(rels) > 0 {
				fmt.Fprintf(&b, "**Relationships:** %s\n\n", strings.Join(rels, ", "))
			}
		}
	}
	return b.String()
}

// loadCatalog loads the embedded DSL and renders the concept catalog.
func loadCatalog() (string, int, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		return "", 0, fmt.Errorf("load concepts: %w", err)
	}
	concepts := memoryNodes.List()
	if len(concepts) == 0 {
		return "", 0, errors.New("no concepts loaded (registry empty)")
	}
	return renderConceptCatalog(concepts), len(concepts), nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: docs-gen <subcommand> [flags]

subcommands:
  catalog   write the concept catalog to <out>/concepts.md
  bundle    build or check the release docs bundle (contract v2)

run 'docs-gen <subcommand> -h' for its flags`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "catalog":
		return runCatalog(args[1:], stdout, stderr)
	case "bundle":
		return runBundle(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "docs-gen: unknown subcommand %q\n\n%s\n", args[0], usage)
		return exitUsage
	}
}

func runCatalog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("docs-gen catalog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", filepath.Join("docs", "public", "reference", "_generated"), "output directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	md, n, err := loadCatalog()
	if err != nil {
		fmt.Fprintln(stderr, "docs-gen:", err)
		return exitFailed
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, "docs-gen:", err)
		return exitFailed
	}
	dest := filepath.Join(*out, "concepts.md")
	if err := os.WriteFile(dest, []byte(md), 0o644); err != nil {
		fmt.Fprintln(stderr, "docs-gen:", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "docs-gen: wrote %s (%d concepts)\n", dest, n)
	return exitOK
}

// bundleSummary is the one JSON line `docs-gen bundle` prints on stdout.
type bundleSummary struct {
	Version     string             `json:"version"`
	Check       bool               `json:"check"`
	Pages       int                `json:"pages"`
	Excluded    int                `json:"excluded"`
	Allowlisted int                `json:"allowlisted"`
	Violations  []bundle.Violation `json:"violations"`
	Tag         *string            `json:"tag,omitempty"`
	Commit      string             `json:"commit,omitempty"`
	Out         string             `json:"out,omitempty"`
	Tarball     string             `json:"tarball,omitempty"`
	Files       int                `json:"files,omitempty"`
	ByRule      map[string]int     `json:"violationsByRule,omitempty"`
}

func runBundle(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("docs-gen bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		version   = fs.String("version", "", "bare X.Y.Z the bundle is labelled with (required)")
		root      = fs.String("root", ".", "repository root")
		out       = fs.String("out", "", "directory to write the bundle tree into (a previous bundle there is replaced)")
		tarPath   = fs.String("tarball", "", "tarball path (default <root>/docs-<version>.tgz)")
		siteURL   = fs.String("site-url", bundle.DefaultSiteURL, "site the absolute URLs in llms.txt and the sitemap point at")
		allowPath = fs.String("allowlist", "", "boundary allowlist (default <root>/"+bundle.DefaultAllowlist+")")
		check     = fs.Bool("check", false, "run the selection and the boundary check only; write nothing")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "docs-gen bundle: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *version == "" {
		fmt.Fprintln(stderr, "docs-gen bundle: -version is required")
		return exitUsage
	}
	if !bundle.ValidVersion(*version) {
		fmt.Fprintf(stderr, "docs-gen bundle: -version %q is not X.Y.Z (bare, no leading v)\n", *version)
		return exitUsage
	}
	allowName := *allowPath
	if allowName == "" {
		// Reported repository-relative, the way its entries name pages.
		allowName = bundle.DefaultAllowlist
		*allowPath = filepath.Join(*root, filepath.FromSlash(bundle.DefaultAllowlist))
	}
	if *tarPath == "" && !*check {
		*tarPath = filepath.Join(*root, "docs-"+*version+".tgz")
	}
	allow, err := bundle.LoadAllowlist(*allowPath)
	if err != nil {
		fmt.Fprintln(stderr, "docs-gen bundle: allowlist:", err)
		return exitFailed
	}
	allow.Path = allowName

	opts := bundle.Options{
		RepoRoot: *root,
		Allow:    allow,
		Check:    *check,
		Meta:     bundle.Meta{Version: *version, SiteURL: *siteURL},
	}
	// The preconditions a build has and a check does not come first, so a
	// shallow clone is refused before the DSL is loaded.
	if !*check {
		dater, err := bundle.NewGitDater(*root, "HEAD")
		if err != nil {
			fmt.Fprintln(stderr, "docs-gen bundle:", err)
			return exitCodeFor(err)
		}
		meta, err := bundle.GitMeta(*root, *version, *siteURL)
		if err != nil {
			fmt.Fprintln(stderr, "docs-gen bundle:", err)
			return exitCodeFor(err)
		}
		opts.Dater, opts.Meta = dater, meta
		opts.Out, opts.Tarball = *out, *tarPath
	}

	catalog, n, err := loadCatalog()
	if err != nil {
		fmt.Fprintln(stderr, "docs-gen bundle: concept catalog:", err)
		return exitFailed
	}
	fmt.Fprintf(stderr, "INFO: concept catalog rendered (%d concepts) as %s\n", n, bundle.ConceptCatalogPath)
	opts.Reference = []bundle.GeneratedPage{{Path: bundle.ConceptCatalogPath, Content: catalog}}

	res, err := bundle.Build(opts)
	summary := bundleSummary{Version: *version, Check: *check, Violations: []bundle.Violation{}}
	if res != nil {
		summary.Pages = res.Selected
		summary.Excluded = len(res.Excluded)
		summary.Allowlisted = res.Allowlisted
		if len(res.Violations) > 0 {
			summary.Violations = res.Violations
			summary.ByRule = map[string]int{}
			for _, v := range res.Violations {
				summary.ByRule[v.Rule]++
			}
		}
	}
	switch {
	case errors.Is(err, bundle.ErrViolations):
		for _, v := range res.Violations {
			fmt.Fprintln(stderr, "ERROR:", v.String())
		}
		fmt.Fprintf(stderr, "ERROR: %d boundary violation(s); nothing was written. A link to a page the site does not publish "+
			"404s there, and a link out of docs/public points readers at files that are not public. Link the public page, "+
			"or say it in prose (docs/DOCS_STANDARD.md section 5).\n", len(res.Violations))
		writeSummary(stdout, summary)
		return exitViolations
	case err != nil:
		fmt.Fprintln(stderr, "docs-gen bundle:", err)
		return exitCodeFor(err)
	}
	if *check {
		fmt.Fprintf(stderr, "INFO: boundary check passed: %d pages selected, %d excluded, %d allowlisted link(s)\n",
			res.Selected, len(res.Excluded), res.Allowlisted)
	} else {
		if res.Manifest.Tag != nil {
			summary.Tag = res.Manifest.Tag
		}
		summary.Commit = res.Manifest.Commit
		summary.Out = opts.Out
		summary.Tarball = opts.Tarball
		summary.Files = len(res.Files)
		fmt.Fprintf(stderr, "INFO: bundled %d pages (%d files) into %s\n", res.Selected, len(res.Files), opts.Tarball)
	}
	writeSummary(stdout, summary)
	return exitOK
}

// exitCodeFor maps a build error to its stable exit code.
func exitCodeFor(err error) int {
	var refused *bundle.RefusedOutError
	switch {
	case errors.Is(err, bundle.ErrBadVersion), errors.As(err, &refused):
		return exitUsage
	case errors.Is(err, bundle.ErrTree), errors.Is(err, bundle.ErrShallowClone), errors.Is(err, bundle.ErrUndated):
		return exitPrecondition
	default:
		return exitFailed
	}
}

func writeSummary(w io.Writer, s bundleSummary) {
	data, err := json.Marshal(s)
	if err != nil {
		fmt.Fprintf(w, "{\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintln(w, string(data))
}
