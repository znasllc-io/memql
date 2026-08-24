package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedTreeIsNotStale regenerates the whole mirror from the RECORDED
// 2026-07 fixture into a temporary directory and compares it, file for file,
// with what is checked in.
//
// This is the gate that makes "the model is generated" true rather than
// aspirational. Without it a hand edit to a generated concept survives review
// -- the file looks like every other file in the tree -- and then vanishes the
// next time anybody regenerates, taking whatever it fixed with it. The
// failure mode is not a broken build; it is a fix that quietly un-happens
// weeks later.
//
// It is also the QUARTERLY BUMP procedure, and the failure message says so:
// a new Admin version is `--record` plus this test's diff, reviewed.
func TestGeneratedTreeIsNotStale(t *testing.T) {
	repo := repoRoot()
	list, err := LoadAllowlist(filepath.Join(repo, "cmd", "shopifyschema", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	schema, err := ReadSchemaFile(filepath.Join(repo, "cmd", "shopifyschema", "testdata", "schema-"+list.APIVersion+".json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	plans, err := NewPlanner(schema, list).Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	tmp := t.TempDir()
	if _, err := generate(tmp, list.APIVersion, list, NewPlanSet(plans)); err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, dir := range []string{dslGeneratedDir, goGeneratedDir} {
		compareTrees(t, filepath.Join(tmp, filepath.FromSlash(dir)), filepath.Join(repo, filepath.FromSlash(dir)))
	}
}

func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	wantFiles := readTree(t, want)
	gotFiles := readTree(t, got)

	for rel, body := range wantFiles {
		have, ok := gotFiles[rel]
		if !ok {
			t.Errorf("%s is missing from the checked-in tree.\n%s", rel, bumpProcedure)
			continue
		}
		if have != body {
			t.Errorf("%s differs from what the generator produces.\n%s\n%s", rel, firstDiff(body, have), bumpProcedure)
		}
	}
	for rel := range gotFiles {
		if _, ok := wantFiles[rel]; !ok {
			t.Errorf("%s is checked in but the generator does not produce it -- an allowlist entry was removed without regenerating.\n%s", rel, bumpProcedure)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// firstDiff names the first differing line, which is the part a reader needs;
// a whole-file dump of a two-thousand-line generated concept is not a diff.
func firstDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return "  first difference at line " + itoa(i+1) + ":\n    generator: " + w[i] + "\n    checked in: " + g[i]
		}
	}
	return "  the files agree line for line up to line " + itoa(min(len(w), len(g))) + ", then one ends"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const bumpProcedure = `
  Regenerate:  go run ./cmd/shopifyschema
  A NEW Admin version is a reviewed regeneration, not an override:
    1. bump apiVersion in cmd/shopifyschema/allowlist.yaml
    2. go run ./cmd/shopifyschema --record     (re-records the fixture)
    3. review the diff -- new fields, removed fields, changed enums
    4. bump the pinned version on every store row and re-run
       EnsureSubscriptions, which re-registers every topic at the new version
  See docs/public/operate/shopify-connector.md, "the quarterly bump".`

// TestRecordedFixtureCoversTheAllowlist fails when the fixture was recorded
// before a type was added to the allowlist. Without it the generator's error
// is "type X is not in the 2026-07 schema", which reads like Shopify removed
// the type rather than like the fixture is behind.
func TestRecordedFixtureCoversTheAllowlist(t *testing.T) {
	repo := repoRoot()
	list, err := LoadAllowlist(filepath.Join(repo, "cmd", "shopifyschema", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	schema, err := ReadSchemaFile(filepath.Join(repo, "cmd", "shopifyschema", "testdata", "schema-"+list.APIVersion+".json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	for _, name := range list.RootTypeNames() {
		if schema.Lookup(name) == nil {
			t.Errorf("allowlist names %q but the recorded %s fixture does not carry it -- re-record with: go run ./cmd/shopifyschema --record", name, list.APIVersion)
		}
	}
	if schema.Lookup("WebhookSubscriptionTopic") == nil {
		t.Error("the fixture lost WebhookSubscriptionTopic, so no topic can be validated against the schema")
	}
}

// TestEverySubscribedTopicIsASchemaEnumValue is the check that would have
// caught a typo in the allowlist's topic lists before it reached a store.
//
// webhookSubscriptionCreate takes the enum, so a misspelled topic is a
// GraphQL error at subscription time -- on a node that has already booted,
// against a store that is already live, in a code path an operator only reads
// when something is already wrong.
func TestEverySubscribedTopicIsASchemaEnumValue(t *testing.T) {
	repo := repoRoot()
	list, err := LoadAllowlist(filepath.Join(repo, "cmd", "shopifyschema", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	schema, err := ReadSchemaFile(filepath.Join(repo, "cmd", "shopifyschema", "testdata", "schema-"+list.APIVersion+".json"))
	if err != nil {
		t.Fatalf("recorded schema: %v", err)
	}
	enum := schema.Lookup("WebhookSubscriptionTopic")
	if enum == nil {
		t.Fatal("WebhookSubscriptionTopic missing from the fixture")
	}
	valid := map[string]bool{}
	for _, v := range enum.EnumValues {
		valid[v.Name] = true
	}
	for i := range list.Types {
		for topic := range list.Types[i].Topics {
			if !valid[topic] {
				t.Errorf("%s subscribes %q, which is not a WebhookSubscriptionTopic value in %s",
					list.Types[i].Type, topic, list.APIVersion)
			}
		}
	}
}
