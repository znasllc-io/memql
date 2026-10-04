// Static guards over the pipeline TOOLCHAIN image lane (epic memql#5478,
// issue memql#5496): deploy/toolchain-image/ and the dispatch-only workflow
// that builds it, .github/workflows/build-toolchain-image.yml.
//
// # What the image is for
//
// It is the container a pipeline step's command runs in -- Go, Node, protoc,
// git, make and the PostgreSQL client -- and the engine's own pipeline
// manifest (memql-package.yaml) names it BY DIGEST. The workflow's product is
// therefore a digest somebody pins, and every property below is about whether
// that digest can be trusted.
//
// # Why these are tests
//
// Each property fails QUIETLY when it regresses, the shape this package's
// db_image_wiring_test.go and the fixture-mirror guard in scripts/release
// exist for:
//
//   - An automatic trigger publishes digests nothing pins, and invites
//     swapping the digest pin for a floating tag.
//   - GITHUB_TOKEN's `packages: write` works from ANY branch, so without the
//     main-only refusal an unreviewed Dockerfile on a feature branch could
//     publish under the package name memql-package.yaml pins.
//   - `packages: write` at workflow scope is inherited by every job added later.
//   - A mutable action tag runs whatever the tag points at today, holding a
//     registry-write token.
//   - Moving the smoke test after the push reads as tidying, and the workflow
//     stays green on every run where the image happens to be fine.
//   - Go and protoc are written down where the repository pins them and again
//     in the Dockerfile. A bump that lands in one copy builds an image that
//     tests on another Go, or stamps another protoc, than everything else.
//
// TestToolchainImageGuardsFireOnTheShapesTheyExistFor feeds each check a
// broken copy of the real file it reads, so a check that stopped seeing its
// shape fails there rather than passing quietly here.
package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	toolchainWorkflowPath   = ".github/workflows/build-toolchain-image.yml"
	toolchainDockerfilePath = "deploy/toolchain-image/Dockerfile"
	toolchainSmokeScript    = "deploy/toolchain-image/smoke-test.sh"
	// toolchainImage is the package memql-package.yaml pins. The name is a
	// contract with that manifest: a push anywhere else leaves the pin naming
	// a package this workflow no longer publishes.
	toolchainImage = "ghcr.io/znasllc-io/memql-toolchain"
)

// toolchainAllowedActions are the actions the pushing job may run. The job
// holds `packages: write`, so each entry is a party trusted to publish under
// the org: the set build-db-image.yml already trusts for the same work.
var toolchainAllowedActions = map[string]bool{
	"actions/checkout":           true,
	"docker/login-action":        true,
	"docker/setup-buildx-action": true,
	"docker/build-push-action":   true,
}

var (
	toolchainSHA40       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	toolchainDigest      = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	toolchainEnvRef      = regexp.MustCompile(`\$\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	toolchainFromLine    = regexp.MustCompile(`(?mi)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?\s*$`)
	toolchainLeadVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?`)
)

type toolchainWorkflow struct {
	On          any                     `yaml:"on"`
	Permissions map[string]string       `yaml:"permissions"`
	Env         map[string]any          `yaml:"env"`
	Jobs        map[string]toolchainJob `yaml:"jobs"`
}

type toolchainJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []toolchainStep   `yaml:"steps"`
}

type toolchainStep struct {
	Name string         `yaml:"name"`
	ID   string         `yaml:"id"`
	If   string         `yaml:"if"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
	Env  map[string]any `yaml:"env"`
}

// with reads a `with:` input as text; `push: false` is a YAML boolean.
func (s toolchainStep) with(key string) string {
	v, ok := s.With[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// action is the `owner/repo` half of a `uses:` reference.
func (s toolchainStep) action() string {
	owner, _, _ := strings.Cut(s.Uses, "@")
	return owner
}

// text is everything a step says about what it does: its condition, its
// script and its environment's values.
func (s toolchainStep) text() string {
	parts := []string{s.If, s.Run}
	for _, k := range SortedKeys(s.Env) {
		parts = append(parts, fmt.Sprint(s.Env[k]))
	}
	return strings.Join(parts, "\n")
}

// publishes reports whether a step writes to a registry.
func (s toolchainStep) publishes() bool {
	if s.action() == "docker/build-push-action" && s.with("push") == "true" {
		return true
	}
	return strings.Contains(s.Run, "docker push") || strings.Contains(s.Run, "imagetools create")
}

func toolchainRead(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(RepoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func toolchainParse(raw string) (toolchainWorkflow, error) {
	var wf toolchainWorkflow
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		return wf, err
	}
	if len(wf.Jobs) == 0 {
		return wf, fmt.Errorf("it declares no jobs; these guards would pass vacuously")
	}
	return wf, nil
}

func toolchainLoad(t *testing.T) toolchainWorkflow {
	t.Helper()
	wf, err := toolchainParse(toolchainRead(t, toolchainWorkflowPath))
	if err != nil {
		t.Fatalf("parse %s: %v", toolchainWorkflowPath, err)
	}
	return wf
}

// toolchainPushJob is the job holding `packages: write`: the one that pushes.
func toolchainPushJob(wf toolchainWorkflow) (toolchainJob, bool) {
	for _, name := range SortedKeys(wf.Jobs) {
		if wf.Jobs[name].Permissions["packages"] == "write" {
			return wf.Jobs[name], true
		}
	}
	return toolchainJob{}, false
}

// toolchainPushIndex is the position of the job's single publishing step.
func toolchainPushIndex(job toolchainJob) (int, string) {
	at := -1
	count := 0
	for i, s := range job.Steps {
		if s.publishes() {
			count++
			if at < 0 {
				at = i
			}
		}
	}
	if count != 1 {
		return -1, fmt.Sprintf("the pushing job has %d steps that publish an image; it must have exactly one, "+
			"so there is one push for the smoke test to gate", count)
	}
	return at, ""
}

// toolchainTriggerProblems: the workflow runs only when someone dispatches it,
// and the dispatch names the tag it publishes.
func toolchainTriggerProblems(wf toolchainWorkflow) []string {
	names := map[string]any{}
	switch on := wf.On.(type) {
	case map[string]any:
		names = on
	case []any:
		for _, item := range on {
			names[fmt.Sprint(item)] = nil
		}
	case string:
		names[on] = nil
	case nil:
		return []string{"the workflow declares no `on:` triggers at all"}
	default:
		return []string{fmt.Sprintf("the workflow's `on:` has an unexpected shape %T", wf.On)}
	}
	var out []string
	for _, name := range SortedKeys(names) {
		if name != "workflow_dispatch" {
			out = append(out, fmt.Sprintf("the workflow is triggered by %q. It must be workflow_dispatch only: "+
				"an automatic build publishes a digest nothing pins, and invites replacing the pin in "+
				"memql-package.yaml with a floating tag", name))
		}
	}
	dispatch, ok := names["workflow_dispatch"]
	if !ok {
		return append(out, "the workflow has no workflow_dispatch trigger")
	}
	spec, _ := dispatch.(map[string]any)
	inputs, _ := spec["inputs"].(map[string]any)
	version, _ := inputs["version"].(map[string]any)
	if version == nil || version["required"] != true {
		out = append(out, "the dispatch has no required `version` input; the pushed tag is "+
			"memql-toolchain:<version>, so a dispatch must name it")
	}
	return out
}

// toolchainMainOnlyProblems: the pushing job refuses, before anything else,
// to run on a ref other than main.
func toolchainMainOnlyProblems(wf toolchainWorkflow) []string {
	job, ok := toolchainPushJob(wf)
	if !ok {
		return []string{"no job holds `packages: write`, so there is no push to guard"}
	}
	if len(job.Steps) == 0 {
		return []string{"the pushing job has no steps"}
	}
	first := job.Steps[0]
	if !strings.Contains(first.text(), "github.ref") ||
		!strings.Contains(first.Run, "refs/heads/main") ||
		!strings.Contains(first.Run, "exit 1") {
		return []string{fmt.Sprintf("the pushing job's first step (%q) does not refuse a ref other than "+
			"refs/heads/main. A dispatch can run from any branch, and GITHUB_TOKEN's `packages: write` "+
			"works from all of them, so this refusal is the only thing that keeps an unreviewed "+
			"Dockerfile from publishing under the pinned package name", first.Name)}
	}
	return nil
}

// toolchainPermissionProblems: read-only at workflow scope, and
// `packages: write` on exactly the one job that pushes, with nothing else.
func toolchainPermissionProblems(wf toolchainWorkflow) []string {
	var out []string
	if wf.Permissions["contents"] != "read" {
		out = append(out, "the workflow-level permissions must be `contents: read`")
	}
	for _, scope := range SortedKeys(wf.Permissions) {
		if wf.Permissions[scope] == "write" {
			out = append(out, fmt.Sprintf("the workflow grants `%s: write` at workflow scope, where every "+
				"job added to this file later inherits it; grant it on the job that needs it", scope))
		}
	}
	writers := 0
	for _, name := range SortedKeys(wf.Jobs) {
		job := wf.Jobs[name]
		if job.Permissions["contents"] != "read" {
			out = append(out, fmt.Sprintf("job %q must declare `contents: read`, the least-privilege "+
				"convention every workflow here follows", name))
		}
		for _, scope := range SortedKeys(job.Permissions) {
			if job.Permissions[scope] != "write" {
				continue
			}
			if scope != "packages" {
				out = append(out, fmt.Sprintf("job %q holds `%s: write`; pushing to GHCR with the automatic "+
					"token needs `packages: write` and nothing else", name, scope))
				continue
			}
			writers++
		}
	}
	if writers != 1 {
		out = append(out, fmt.Sprintf("expected exactly one job holding `packages: write`, found %d", writers))
	}
	return out
}

// toolchainActionProblems: every action is pinned to a commit and comes from
// the allow-list.
func toolchainActionProblems(wf toolchainWorkflow) []string {
	var out []string
	checked := 0
	for _, name := range SortedKeys(wf.Jobs) {
		for _, s := range wf.Jobs[name].Steps {
			if s.Uses == "" {
				continue
			}
			checked++
			owner, ref, found := strings.Cut(s.Uses, "@")
			if !found || !toolchainSHA40.MatchString(ref) {
				out = append(out, fmt.Sprintf("action %q is not pinned to a 40-hex commit SHA; the job holds "+
					"`packages: write`, and a tag runs whatever it points at today", s.Uses))
			}
			// A well-formed SHA under another owner passes the shape check while
			// running someone else's code with a registry-write token.
			if !toolchainAllowedActions[owner] {
				out = append(out, fmt.Sprintf("action %q is not on this workflow's allow-list %v; every "+
					"addition is a new party trusted to publish under the org", s.Uses,
					SortedKeys(toolchainAllowedActions)))
			}
		}
	}
	if checked == 0 {
		out = append(out, "the workflow runs no `uses:` step at all; this guard would pass vacuously")
	}
	return out
}

// toolchainTargetProblems: the push goes to the pinned package on GHCR, with
// the automatic token.
func toolchainTargetProblems(wf toolchainWorkflow) []string {
	job, ok := toolchainPushJob(wf)
	if !ok {
		return []string{"no job holds `packages: write`, so there is no push to check"}
	}
	var out []string
	sawLogin := false
	for _, s := range job.Steps {
		if s.action() != "docker/login-action" {
			continue
		}
		sawLogin = true
		if s.with("registry") != "ghcr.io" {
			out = append(out, fmt.Sprintf("the login step targets %q; the image is published to ghcr.io", s.with("registry")))
		}
		if !strings.Contains(s.with("password"), "secrets.GITHUB_TOKEN") {
			out = append(out, "the login step must authenticate with the automatic GITHUB_TOKEN; a static "+
				"registry secret is a credential this workflow does not need")
		}
	}
	if !sawLogin {
		out = append(out, "the pushing job never logs in to a registry")
	}
	at, problem := toolchainPushIndex(job)
	if problem != "" {
		return append(out, problem)
	}
	push := job.Steps[at]
	expanded := toolchainEnvRef.ReplaceAllStringFunc(push.with("tags"), func(m string) string {
		name := toolchainEnvRef.FindStringSubmatch(m)[1]
		if v, ok := wf.Env[name]; ok {
			return fmt.Sprint(v)
		}
		return m
	})
	// build-push-action reads `tags` as a newline- or comma-separated list; a
	// tag itself holds `${{ inputs.version }}`, spaces included.
	var tags []string
	for _, tag := range strings.FieldsFunc(expanded, func(r rune) bool { return r == '\n' || r == ',' }) {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		out = append(out, fmt.Sprintf("the push step (%q) names no tags", push.Name))
	}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, toolchainImage+":") || !strings.Contains(tag, "inputs.version") {
			out = append(out, fmt.Sprintf("the push step tags %q; every tag must be %s:<the version input>, "+
				"the package memql-package.yaml pins", tag, toolchainImage))
		}
	}
	return out
}

// toolchainPushOrderProblems: the image is built and LOADED, smoke-tested,
// and only then pushed -- and what is pushed is what was tested.
func toolchainPushOrderProblems(wf toolchainWorkflow) []string {
	job, ok := toolchainPushJob(wf)
	if !ok {
		return []string{"no job holds `packages: write`, so there is no push to order"}
	}
	pushAt, problem := toolchainPushIndex(job)
	if problem != "" {
		return []string{problem}
	}
	smokeAt := -1
	for i, s := range job.Steps {
		if strings.Contains(s.Run, toolchainSmokeScript) {
			smokeAt = i
			break
		}
	}
	if smokeAt < 0 {
		return []string{"the pushing job never runs " + toolchainSmokeScript}
	}
	if smokeAt > pushAt {
		return []string{"the smoke test runs AFTER the push. It must gate the push: an image that " +
			"reaches the registry broken is pinnable, and a broken toolchain shows up as every pipeline " +
			"step failing, not as a failed pull"}
	}
	tested := -1
	for i := smokeAt - 1; i >= 0; i-- {
		if job.Steps[i].action() == "docker/build-push-action" {
			tested = i
			break
		}
	}
	if tested < 0 {
		return []string{"no build step precedes the smoke test, so it exercises no freshly built image"}
	}
	build, push := job.Steps[tested], job.Steps[pushAt]
	var out []string
	if build.with("load") != "true" {
		out = append(out, "the pre-smoke build does not `load: true`, so there is no local image to smoke-test")
	}
	if build.with("push") != "false" {
		out = append(out, "the pre-smoke build does not set `push: false`; the smoke test then gates nothing")
	}
	// The smoke test vouches for the image the first build produced; a push
	// built from another context, file or platform publishes something it never saw.
	for _, key := range []string{"context", "file", "platforms"} {
		if build.with(key) != push.with(key) {
			out = append(out, fmt.Sprintf("the smoke-tested build and the push differ in `%s` (%q vs %q); "+
				"the push would publish an image the smoke test never exercised", key, build.with(key), push.with(key)))
		}
	}
	return out
}

// toolchainSummaryProblems: after the push, the summary names the pushed
// digest and says where to pin it.
func toolchainSummaryProblems(wf toolchainWorkflow) []string {
	job, ok := toolchainPushJob(wf)
	if !ok {
		return []string{"no job holds `packages: write`, so there is no digest to report"}
	}
	pushAt, problem := toolchainPushIndex(job)
	if problem != "" {
		return []string{problem}
	}
	id := job.Steps[pushAt].ID
	if id == "" {
		return []string{"the push step has no `id:`, so no later step can read the digest it produced"}
	}
	ref := "steps." + id + ".outputs.digest"
	for _, s := range job.Steps[pushAt+1:] {
		if strings.Contains(s.Run, "GITHUB_STEP_SUMMARY") &&
			strings.Contains(s.text(), ref) &&
			strings.Contains(s.Run, "memql-package.yaml") {
			return nil
		}
	}
	return []string{"no step after the push writes the pushed digest (" + ref + ") and the instruction to " +
		"pin it in memql-package.yaml into $GITHUB_STEP_SUMMARY; the digest is this workflow's product"}
}

// toolchainDockerfileProblems: every base is pinned by digest, the versions
// are the repository's, the protoc download is verified, and the image runs as
// a numeric non-root user.
func toolchainDockerfileProblems(dockerfile, goWork, protoGen string) []string {
	var out []string
	first := func(re, body string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(body)
		if len(m) < 2 {
			return ""
		}
		return m[1]
	}

	wantGo := first(`(?m)^toolchain\s+go(\S+)\s*$`, goWork)
	if wantGo == "" {
		out = append(out, "could not read the `toolchain` line out of go.work; this guard is no longer "+
			"reading what it claims to")
	}
	wantProtoc := first(`(?m)^readonly PROTOC_VERSION="([^"]+)"`, protoGen)
	if wantProtoc == "" {
		out = append(out, "could not read PROTOC_VERSION out of scripts/dev/proto-gen.sh; this guard is "+
			"no longer reading what it claims to")
	}

	matches := toolchainFromLine.FindAllStringSubmatch(dockerfile, -1)
	if len(matches) == 0 {
		return append(out, "the Dockerfile has no FROM line; this guard is scanning nothing")
	}
	stages := map[string]bool{}
	for _, m := range matches {
		if m[2] != "" {
			stages[strings.ToLower(m[2])] = true
		}
	}
	var goTags []string
	for _, m := range matches {
		ref := m[1]
		if stages[strings.ToLower(ref)] {
			continue // FROM <an earlier stage>
		}
		if !toolchainDigest.MatchString(ref) {
			out = append(out, fmt.Sprintf("FROM %s is not pinned by digest; a tag moves under the build", ref))
		}
		repo, _, _ := strings.Cut(ref, "@")
		tag := ""
		if i := strings.LastIndex(repo, ":"); i >= 0 && !strings.Contains(repo[i:], "/") {
			repo, tag = repo[:i], repo[i+1:]
		}
		if repo == "golang" || strings.HasSuffix(repo, "/golang") {
			goTags = append(goTags, tag)
		}
	}
	if len(goTags) != 1 {
		out = append(out, fmt.Sprintf("expected exactly one FROM golang:<version> stage, found %d", len(goTags)))
	} else if got := toolchainLeadVersion.FindString(goTags[0]); wantGo != "" && got != wantGo {
		out = append(out, fmt.Sprintf("the toolchain image's Go is %q (FROM golang:%s), but go.work's toolchain "+
			"line says go%s.\n"+
			"go.work's line is the Go the workspace runs (it overrides every go.mod), and the GitHub lanes "+
			"install the same version. `toolchain` is a floor, so a newer Go here would be used as found: "+
			"the pipeline would test on a Go nothing else does. Move this FROM line in the same change as "+
			"go.work, then dispatch build-toolchain-image.yml and re-pin memql-package.yaml -- the PINNED "+
			"image is what a pipeline runs, not this file. If the pipeline must deliberately run another Go, "+
			"argue it here.", got, goTags[0], wantGo))
	}

	if got := first(`(?m)^ARG PROTOC_VERSION=(\S+)\s*$`, dockerfile); wantProtoc != "" && got != wantProtoc {
		out = append(out, fmt.Sprintf("the toolchain image's protoc is %q (ARG PROTOC_VERSION), but "+
			"scripts/dev/proto-gen.sh pins %q. The pin exists so every machine writes the same "+
			"`// protoc` stamp into the generated files; move both in the same change.", got, wantProtoc))
	}
	if !strings.Contains(dockerfile, "sha256sum -c") {
		out = append(out, "the protoc download is not checked against a pinned SHA-256 (`sha256sum -c`); "+
			"a fetched binary with no checksum is whatever the URL served that day")
	}

	users := regexp.MustCompile(`(?m)^USER\s+(\S+)\s*$`).FindAllStringSubmatch(dockerfile, -1)
	if len(users) == 0 {
		out = append(out, "the Dockerfile sets no USER, so every step runs as root")
	} else if last := users[len(users)-1][1]; !regexp.MustCompile(`^1000(:1000)?$`).MatchString(last) {
		out = append(out, fmt.Sprintf("the image's final USER is %q; it must be the NUMERIC uid 1000. Steps "+
			"run as the memql user, never root, and Kubernetes can only verify runAsNonRoot against a number", last))
	}
	return out
}

func toolchainReport(t *testing.T, problems []string) {
	t.Helper()
	for _, p := range problems {
		t.Error(p)
	}
}

func TestToolchainImageWorkflowIsDispatchOnly(t *testing.T) {
	toolchainReport(t, toolchainTriggerProblems(toolchainLoad(t)))
}

func TestToolchainImageWorkflowRefusesRefsOtherThanMain(t *testing.T) {
	toolchainReport(t, toolchainMainOnlyProblems(toolchainLoad(t)))
}

func TestToolchainImageWorkflowScopesPackagesWriteToTheJob(t *testing.T) {
	toolchainReport(t, toolchainPermissionProblems(toolchainLoad(t)))
}

func TestToolchainImageWorkflowPinsActionsBySHA(t *testing.T) {
	toolchainReport(t, toolchainActionProblems(toolchainLoad(t)))
}

func TestToolchainImageWorkflowPushesThePinnedPackage(t *testing.T) {
	toolchainReport(t, toolchainTargetProblems(toolchainLoad(t)))
}

func TestToolchainImageSmokeTestGatesThePush(t *testing.T) {
	toolchainReport(t, toolchainPushOrderProblems(toolchainLoad(t)))
}

func TestToolchainImageSummaryNamesTheDigestToPin(t *testing.T) {
	toolchainReport(t, toolchainSummaryProblems(toolchainLoad(t)))
}

func TestToolchainImageVersionsMatchTheRepositoryPins(t *testing.T) {
	toolchainReport(t, toolchainDockerfileProblems(
		toolchainRead(t, toolchainDockerfilePath),
		toolchainRead(t, "go.work"),
		toolchainRead(t, "scripts/dev/proto-gen.sh"),
	))
}

// TestToolchainImageGuardsFireOnTheShapesTheyExistFor is the negative control,
// kept in the suite: each check must report the broken copy of the real file
// built for it, and report it for the RIGHT reason -- every case names a phrase
// its problem must carry, so a check firing on something unrelated does not
// count. A mutation whose anchor is gone fails too, because a mutation that
// changed nothing would prove nothing.
func TestToolchainImageGuardsFireOnTheShapesTheyExistFor(t *testing.T) {
	t.Run("workflow", toolchainWorkflowMutations)
	t.Run("dockerfile", toolchainDockerfileMutations)
}

// toolchainExpectProblem passes when some reported problem carries want.
func toolchainExpectProblem(t *testing.T, got []string, want string) {
	t.Helper()
	for _, p := range got {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Errorf("no reported problem mentions %q, so the check no longer sees the shape it exists for "+
		"(or fires for another reason).\ngot: %q", want, got)
}

func toolchainDockerfileMutations(t *testing.T) {
	real := toolchainRead(t, toolchainDockerfilePath)
	goWork := toolchainRead(t, "go.work")
	protoGen := toolchainRead(t, "scripts/dev/proto-gen.sh")
	for _, tc := range []struct {
		name string
		want string
		edit func(string) string
	}{
		{"another Go", "go.work's toolchain line", func(s string) string {
			return regexp.MustCompile(`(?m)^FROM golang:[0-9.]+`).ReplaceAllString(s, "FROM golang:1.0.0")
		}},
		{"another protoc", "scripts/dev/proto-gen.sh pins", func(s string) string {
			return regexp.MustCompile(`(?m)^ARG PROTOC_VERSION=\S+`).ReplaceAllString(s, "ARG PROTOC_VERSION=0.0.0")
		}},
		{"a base on a tag", "is not pinned by digest", func(s string) string {
			return regexp.MustCompile(`(?m)^(FROM node:\S+?)@sha256:[0-9a-f]{64}`).ReplaceAllString(s, "$1")
		}},
		{"an unverified download", "`sha256sum -c`", func(s string) string {
			return strings.ReplaceAll(s, "sha256sum -c", "true")
		}},
		{"a root user", "NUMERIC uid 1000", func(s string) string {
			return regexp.MustCompile(`(?m)^USER\s+\S+\s*$`).ReplaceAllString(s, "USER root")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.edit(real)
			if edited == real {
				t.Fatal("the mutation changed nothing; its anchor is gone from the Dockerfile")
			}
			toolchainExpectProblem(t, toolchainDockerfileProblems(edited, goWork, protoGen), tc.want)
		})
	}
}

func toolchainWorkflowMutations(t *testing.T) {
	real := toolchainRead(t, toolchainWorkflowPath)
	if _, err := toolchainParse(real); err != nil {
		t.Fatalf("parse %s: %v", toolchainWorkflowPath, err)
	}
	// stepAt finds a step of the pushing job by a predicate, for structural edits.
	stepAt := func(t *testing.T, job toolchainJob, pred func(toolchainStep) bool) int {
		t.Helper()
		for i, s := range job.Steps {
			if pred(s) {
				return i
			}
		}
		t.Fatal("the mutation's anchor step is gone from the workflow; retarget the mutation")
		return -1
	}
	isSmoke := func(s toolchainStep) bool { return strings.Contains(s.Run, toolchainSmokeScript) }
	isPush := func(s toolchainStep) bool { return s.publishes() }
	isTestedBuild := func(s toolchainStep) bool {
		return s.action() == "docker/build-push-action" && s.with("push") == "false"
	}

	cases := []struct {
		name   string
		want   string
		check  func(toolchainWorkflow) []string
		text   func(string) string             // a textual edit, or nil
		mutate func(*testing.T, *toolchainJob) // a structural edit of the pushing job, or nil
	}{
		{
			name:  "a push trigger",
			want:  `triggered by "push"`,
			check: toolchainTriggerProblems,
			text: func(s string) string {
				return strings.Replace(s, "\non:\n", "\non:\n  push:\n    branches: [main]\n", 1)
			},
		},
		{
			name:  "packages: write at workflow scope",
			want:  "`packages: write` at workflow scope",
			check: toolchainPermissionProblems,
			text: func(s string) string {
				return strings.Replace(s, "\npermissions:\n  contents: read\n", "\npermissions:\n  contents: read\n  packages: write\n", 1)
			},
		},
		{
			name:  "an action on a mutable tag",
			want:  `"docker/login-action@v4" is not pinned`,
			check: toolchainActionProblems,
			text: func(s string) string {
				return regexp.MustCompile(`docker/login-action@[0-9a-f]{40}`).ReplaceAllString(s, "docker/login-action@v4")
			},
		},
		{
			name:  "the push aimed at another package",
			want:  "memql-toolchain-renamed:",
			check: toolchainTargetProblems,
			text: func(s string) string {
				return strings.Replace(s, "  IMAGE: "+toolchainImage+"\n", "  IMAGE: "+toolchainImage+"-renamed\n", 1)
			},
		},
		{
			name:  "no main-only refusal",
			want:  "does not refuse a ref other than refs/heads/main",
			check: toolchainMainOnlyProblems,
			mutate: func(t *testing.T, job *toolchainJob) {
				job.Steps = job.Steps[1:]
			},
		},
		{
			name:  "the smoke test moved after the push",
			want:  "AFTER the push",
			check: toolchainPushOrderProblems,
			mutate: func(t *testing.T, job *toolchainJob) {
				smoke := stepAt(t, *job, isSmoke)
				moved := job.Steps[smoke]
				job.Steps = append(job.Steps[:smoke:smoke], job.Steps[smoke+1:]...)
				job.Steps = append(job.Steps, moved)
			},
		},
		{
			name:  "the tested build pushes",
			want:  "steps that publish an image",
			check: toolchainPushOrderProblems,
			mutate: func(t *testing.T, job *toolchainJob) {
				job.Steps[stepAt(t, *job, isTestedBuild)].With["push"] = true
			},
		},
		{
			name:  "the push builds another context",
			want:  "differ in `context`",
			check: toolchainPushOrderProblems,
			mutate: func(t *testing.T, job *toolchainJob) {
				job.Steps[stepAt(t, *job, isPush)].With["context"] = "./deploy/db-image"
			},
		},
		{
			name:  "no summary after the push",
			want:  "no step after the push writes the pushed digest",
			check: toolchainSummaryProblems,
			mutate: func(t *testing.T, job *toolchainJob) {
				job.Steps = job.Steps[:stepAt(t, *job, isPush)+1]
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := real
			if tc.text != nil {
				raw = tc.text(real)
				if raw == real {
					t.Fatal("the textual mutation changed nothing; its anchor is gone from the workflow")
				}
			}
			wf, err := toolchainParse(raw)
			if err != nil {
				t.Fatalf("the mutated workflow does not parse: %v", err)
			}
			if tc.mutate != nil {
				name := ""
				for _, n := range SortedKeys(wf.Jobs) {
					if wf.Jobs[n].Permissions["packages"] == "write" {
						name = n
					}
				}
				if name == "" {
					t.Fatal("no job holds `packages: write` to mutate")
				}
				job := wf.Jobs[name]
				tc.mutate(t, &job)
				wf.Jobs[name] = job
			}
			toolchainExpectProblem(t, tc.check(wf), tc.want)
		})
	}
}
