// Command shopifyschema generates the Shopify mirror from the Admin GraphQL
// schema.
//
// The model is generated, pinned to an API version, for an explicit allowlist
// of root types (design D1 of
// docs/superpowers/specs/2026-08-22-shopify-connector-complete-mirror-design.md).
// A curated subset of hand-written concepts is the "limited functionality"
// the requirement refused, and it lags every quarter; generating the whole
// schema is 3,552 types of mutation inputs and payloads. So a person reviews
// the LIST and the schema supplies the SHAPE.
//
// Usage:
//
//	go run ./cmd/shopifyschema                       # regenerate from the recorded fixture
//	go run ./cmd/shopifyschema --live                # ...from the live proxy instead
//	go run ./cmd/shopifyschema --record              # re-record the fixture from the live proxy
//	go run ./cmd/shopifyschema --out /tmp/x --quiet   # generate elsewhere (the drift gate)
//
// Flags:
//
//	--version <v>    Admin API version. Default: MEMQL_SHOPIFY_API_VERSION, else the allowlist's.
//	--allowlist <p>  Allowlist path. Default: cmd/shopifyschema/allowlist.yaml.
//	--schema <p>     Recorded introspection. Default: cmd/shopifyschema/testdata/schema-<version>.json.
//	--live           Introspect the proxy instead of reading the fixture.
//	--record         Introspect the proxy, prune, and write the fixture.
//	--out <dir>      Repository root to write into. Default: the repo this file lives in.
//	--quiet          Print nothing on success.
//
// No store, app or access token is needed: the schema proxy is public.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	dslGeneratedDir = "dsl/shopify/generated"
	goGeneratedDir  = "integrations/shopify/generated"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "shopifyschema:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		version   = flag.String("version", strings.TrimSpace(os.Getenv("MEMQL_SHOPIFY_API_VERSION")), "Admin API version")
		alPath    = flag.String("allowlist", "", "allowlist path")
		schemaArg = flag.String("schema", "", "recorded introspection path")
		live      = flag.Bool("live", false, "introspect the live proxy")
		record    = flag.Bool("record", false, "re-record the fixture from the live proxy")
		outRoot   = flag.String("out", "", "repository root to write into")
		quiet     = flag.Bool("quiet", false, "print nothing on success")
	)
	flag.Parse()

	repo := repoRoot()
	if *alPath == "" {
		*alPath = filepath.Join(repo, "cmd", "shopifyschema", "allowlist.yaml")
	}
	list, err := LoadAllowlist(*alPath)
	if err != nil {
		return err
	}
	if *version == "" {
		*version = list.APIVersion
	}
	if *version != list.APIVersion {
		return fmt.Errorf("version %s does not match the allowlist's pin %s -- a bump changes apiVersion in %s and is a reviewed regeneration, not a flag",
			*version, list.APIVersion, filepath.Base(*alPath))
	}
	if *schemaArg == "" {
		*schemaArg = filepath.Join(repo, "cmd", "shopifyschema", "testdata", "schema-"+*version+".json")
	}
	if *outRoot == "" {
		*outRoot = repo
	}

	var schema *Schema
	switch {
	case *record || *live:
		schema, err = FetchSchema(*version, 3*time.Minute)
		if err != nil {
			return err
		}
		schema.Version = *version
		schema = schema.Prune(list.RootTypeNames(), "WebhookSubscriptionTopic", "BulkOperation")
		if *record {
			if err := WriteSchemaFile(*schemaArg, schema); err != nil {
				return err
			}
			if !*quiet {
				fmt.Printf("recorded %s (%d types)\n", *schemaArg, len(schema.Types))
			}
		}
	default:
		schema, err = ReadSchemaFile(*schemaArg)
		if err != nil {
			return fmt.Errorf("%w\n\nRecord one with: go run ./cmd/shopifyschema --record", err)
		}
		if schema.Version == "" {
			schema.Version = *version
		}
	}
	if schema.Version != *version {
		return fmt.Errorf("the recorded schema is %s but the allowlist pins %s", schema.Version, *version)
	}

	plans, err := NewPlanner(schema, list).Plan()
	if err != nil {
		return err
	}
	set := NewPlanSet(plans)

	written, err := generate(*outRoot, *version, list, set)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Printf("shopifyschema: %d types, %d files, Admin GraphQL %s\n", len(plans), written, *version)
	}
	return nil
}

// generate writes the whole tree and removes anything stale under the two
// generated directories, so a type dropped from the allowlist leaves no
// orphan concept behind for the loader to keep registering.
func generate(root, version string, list *Allowlist, set *PlanSet) (int, error) {
	dslDir := filepath.Join(root, filepath.FromSlash(dslGeneratedDir))
	goDir := filepath.Join(root, filepath.FromSlash(goGeneratedDir))
	for _, d := range []string{
		dslDir,
		filepath.Join(dslDir, "selections"),
		filepath.Join(dslDir, "bulk"),
		goDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}

	keep := map[string]bool{}
	count := 0
	write := func(path, body string) error {
		keep[path] = true
		count++
		return os.WriteFile(path, []byte(body), 0o644)
	}

	// The namespace pin. dsl/shopify/generated/ is a SUBDIRECTORY of the
	// shopify domain, and the loader derives a concept's namespace from the
	// whole directory path -- so without this the concepts would register as
	// v1:shopify/generated:order. The pin is the supported way to say "this
	// subdirectory's concepts belong to the parent namespace" (#2614).
	if err := write(filepath.Join(dslDir, "namespace.pin"), "shopify\n"); err != nil {
		return 0, err
	}

	docs := map[string]string{}
	bulkDocs := map[string]string{}
	bulkOps := map[string][]string{}

	for _, p := range set.All() {
		if err := write(filepath.Join(dslDir, p.Concept+".memql"), EmitConceptFile(version, p)); err != nil {
			return 0, err
		}
		doc := set.EmitSelectionDocument(version, p)
		docs[p.Concept] = doc
		if err := write(filepath.Join(dslDir, "selections", p.GraphQLType+".graphql"), doc); err != nil {
			return 0, err
		}
		bulk, ops := set.EmitBulkDocuments(version, p)
		if bulk != "" {
			bulkDocs[p.Concept] = bulk
			bulkOps[p.Concept] = ops
			if err := write(filepath.Join(dslDir, "bulk", p.GraphQLType+".graphql"), bulk); err != nil {
				return 0, err
			}
		}
	}

	if err := write(filepath.Join(goDir, "topics.go"), set.EmitTopicsFile(version, list)); err != nil {
		return 0, err
	}
	if err := write(filepath.Join(goDir, "model.go"), set.EmitModelFile(version, list, docs, bulkDocs, bulkOps)); err != nil {
		return 0, err
	}

	if err := pruneStale(dslDir, keep); err != nil {
		return 0, err
	}
	if err := pruneStale(goDir, keep); err != nil {
		return 0, err
	}
	return count, nil
}

// pruneStale removes generated files this run did not write. Only files whose
// name this generator owns are considered, so a hand-written neighbour in the
// same directory is never touched.
func pruneStale(dir string, keep map[string]bool) error {
	var stale []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".memql", ".graphql", ".go", ".pin":
			if !keep[path] {
				stale = append(stale, path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(stale)
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// repoRoot resolves the repository from this source file's own location, so
// the tool works from any working directory.
func repoRoot() string {
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Clean(filepath.Join(filepath.Dir(this), "..", ".."))
}
