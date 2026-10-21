// Toolchain source and smoke-stage invariants for native image publication.
// Trigger, platform, resource and unpublished-artifact contracts are compiled in
// memql_pipeline_images_test.go. Registry effects are guarded by the native
// release publisher and its durable reservation/readback tests.
package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const toolchainDockerfilePath = "deploy/toolchain-image/Dockerfile"

var (
	toolchainSHA256      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	toolchainDigest      = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	toolchainFromLine    = regexp.MustCompile(`(?mi)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?\s*$`)
	toolchainLeadVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?`)
	toolchainMinor       = regexp.MustCompile(`^v?([0-9]+\.[0-9]+)\.`)
)

func toolchainRead(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(RepoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func toolchainDockerfileProblems(dockerfile, goWork, protoGen, k3dUp string) []string {
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
	var goTags, kubectlTags []string
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
		if repo == "kubectl" || strings.HasSuffix(repo, "/kubectl") {
			kubectlTags = append(kubectlTags, tag)
		}
	}
	if len(goTags) != 1 {
		out = append(out, fmt.Sprintf("expected exactly one FROM golang:<version> stage, found %d", len(goTags)))
	} else if got := toolchainLeadVersion.FindString(goTags[0]); wantGo != "" && got != wantGo {
		out = append(out, fmt.Sprintf("the toolchain image's Go is %q (FROM golang:%s), but go.work's toolchain "+
			"line says go%s.\n"+
			"go.work's line is the Go the workspace runs (it overrides every go.mod), and the native lanes "+
			"install the same version. `toolchain` is a floor, so a newer Go here would be used as found: "+
			"the pipeline would test on a Go nothing else does. Move this FROM line in the same change as "+
			"go.work, then publish the native build-tools.toolchain artifact and re-pin memql-package.yaml -- the PINNED "+
			"image is what a pipeline runs, not this file. If the pipeline must deliberately run another Go, "+
			"argue it here.", got, goTags[0], wantGo))
	}

	// kubectl renders the overlays the deploy target applies, so its minor is
	// the Kubernetes minor that target runs. The patch may differ: kubectl and
	// k3s patch releases do not land together.
	wantK8s := first(`(?m)^K3S_VERSION="\$\{[A-Z0-9_]+:-v([0-9]+\.[0-9]+)\.`, k3dUp)
	if wantK8s == "" {
		out = append(out, "could not read the K3S_VERSION default out of scripts/k3d/up.sh; this guard is "+
			"no longer reading what it claims to")
	}
	if len(kubectlTags) != 1 {
		out = append(out, fmt.Sprintf("expected exactly one FROM <registry>/kubectl:<version> stage, found %d: the "+
			"deploy render tests skip without kubectl", len(kubectlTags)))
	} else if m := toolchainMinor.FindStringSubmatch(kubectlTags[0]); m == nil {
		out = append(out, fmt.Sprintf("the kubectl stage's tag %q is not a version this guard can read", kubectlTags[0]))
	} else if wantK8s != "" && m[1] != wantK8s {
		out = append(out, fmt.Sprintf("the toolchain image's kubectl is %s, minor %s, but scripts/k3d/up.sh's "+
			"K3S_VERSION runs Kubernetes %s.\n"+
			"kubectl's embedded kustomize renders the overlays in the deploy render tests, and up.sh holds the "+
			"deploy target at %s on purpose (its comment: ArgoCD's schema). A bump of the kubectl FROM line alone "+
			"tests the overlays against a kustomize that target never runs. Move K3S_VERSION and the kubectl FROM "+
			"line in one change.", kubectlTags[0], m[1], wantK8s, wantK8s))
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
	// `sha256sum -c` is only as good as what it is fed. Each architecture's zip
	// has its own digest, so both are pinned, each a SHA-256, and never equal.
	sums := map[string]string{}
	for _, arch := range []string{"AMD64", "ARM64"} {
		name := "PROTOC_SHA256_" + arch
		sums[arch] = first(`(?m)^ARG `+name+`=(\S*)\s*$`, dockerfile)
		if !toolchainSHA256.MatchString(sums[arch]) {
			out = append(out, fmt.Sprintf("ARG %s is %q; it must be the 64-hex SHA-256 of that architecture's "+
				"protoc zip, or the download it checks is checked against nothing", name, sums[arch]))
		}
	}
	if sums["AMD64"] != "" && sums["AMD64"] == sums["ARM64"] {
		out = append(out, "ARG PROTOC_SHA256_AMD64 and ARG PROTOC_SHA256_ARM64 are the same value; each "+
			"architecture's zip has its own digest, so one of them is a copy that verifies the wrong file")
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

func TestToolchainImageVersionsMatchTheRepositoryPins(t *testing.T) {
	toolchainReport(t, toolchainDockerfileProblems(
		toolchainRead(t, toolchainDockerfilePath),
		toolchainRead(t, "go.work"),
		toolchainRead(t, "scripts/dev/proto-gen.sh"),
		toolchainRead(t, "scripts/k3d/up.sh"),
	))
}

func TestToolchainImageGuardsFireOnTheShapesTheyExistFor(t *testing.T) {
	t.Run("dockerfile", toolchainDockerfileMutations)
}

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
	k3dUp := toolchainRead(t, "scripts/k3d/up.sh")
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
		{"a protoc checksum missing", `ARG PROTOC_SHA256_ARM64 is ""`, func(s string) string {
			return regexp.MustCompile(`(?m)^ARG PROTOC_SHA256_ARM64=\S+\n`).ReplaceAllString(s, "")
		}},
		{"a protoc checksum that is not a SHA-256", "ARG PROTOC_SHA256_AMD64 is", func(s string) string {
			return regexp.MustCompile(`(?m)^(ARG PROTOC_SHA256_AMD64=[0-9a-f]{8})[0-9a-f]+`).ReplaceAllString(s, "$1")
		}},
		{"the two protoc checksums equal", "are the same value", func(s string) string {
			amd := regexp.MustCompile(`(?m)^ARG PROTOC_SHA256_AMD64=(\S+)`).FindStringSubmatch(s)
			if amd == nil {
				return s // anchor gone: the harness reports a mutation that changed nothing
			}
			return regexp.MustCompile(`(?m)^ARG PROTOC_SHA256_ARM64=\S+`).ReplaceAllString(s, "ARG PROTOC_SHA256_ARM64="+amd[1])
		}},
		{"a root user", "NUMERIC uid 1000", func(s string) string {
			return regexp.MustCompile(`(?m)^USER\s+\S+\s*$`).ReplaceAllString(s, "USER root")
		}},
		{"kubectl past the deploy target's minor", "K3S_VERSION runs Kubernetes", func(s string) string {
			return regexp.MustCompile(`(?m)^(FROM \S+/kubectl:v)[0-9]+\.[0-9]+`).ReplaceAllString(s, "${1}1.99")
		}},
		{"no kubectl stage", "kubectl:<version> stage, found 0", func(s string) string {
			return regexp.MustCompile(`(?m)^FROM \S+/kubectl:\S+ AS \S+\n`).ReplaceAllString(s, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.edit(real)
			if edited == real {
				t.Fatal("the mutation changed nothing; its anchor is gone from the Dockerfile")
			}
			toolchainExpectProblem(t, toolchainDockerfileProblems(edited, goWork, protoGen, k3dUp), tc.want)
		})
	}
}
