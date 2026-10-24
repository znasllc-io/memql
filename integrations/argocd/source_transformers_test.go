package argocd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sourceTransformFixture() map[string]sourceFixtureFile {
	files := sourceFilesFixture()
	root := files["overlay/kustomization.yaml"]
	root.body = strings.Replace(root.body, "transformers: [namespace.yaml]", "transformers: [namespace.yaml, scope.yaml, file-patch.yaml]", 1)
	root.body += `openapi: {path: openapi.json}
replacements:
 - source: {kind: ConfigMap, name: settings, fieldPath: data.memory}
   targets:
    - select: {kind: Deployment, name: engine}
      fieldPaths: ['spec.template.metadata.annotations.[memql.io/policy]']
      options: {delimiter: ':', index: 1, create: true}
 - path: replacements.yaml
`
	files["overlay/kustomization.yaml"] = root
	files["overlay/openapi.json"] = sourceFixtureFile{"100644", `{"definitions":{"fixture":{"type":"object"}}}`}
	files["overlay/replacements.yaml"] = sourceFixtureFile{"100644", `- source: {kind: ConfigMap, name: settings, fieldPath: data.memory}
  targets:
   - select: {kind: Deployment}
     reject: [{name: other}]
     fieldPaths: [spec.replicas]
`}
	files["overlay/scope.yaml"] = sourceFixtureFile{"100644", `apiVersion: builtin
kind: PatchTransformer
metadata: {name: cluster-scope}
target: {group: kueue.x-k8s.io, kind: WorkloadPriorityClass}
patch: |-
 - op: add
   path: /metadata/namespace
   value: ''
 - op: remove
   path: /metadata/namespace
`}
	files["overlay/file-patch.yaml"] = sourceFixtureFile{"100644", `apiVersion: builtin
kind: PatchTransformer
metadata: {name: resources}
target: {kind: Deployment, name: engine}
path: transformer-patch.yaml
options: {allowNameChange: false}
`}
	files["overlay/transformer-patch.yaml"] = sourceFixtureFile{"100644", "- op: replace\n  path: /spec/replicas\n  value: 4\n"}
	return files
}
func TestSourceClosureIncludesReplacementsOpenAPIAndPatchTransformer(t *testing.T) {
	files := sourceTransformFixture()
	objects, spec := sourceObjectsFixture(t, files)
	closed, err := VerifySourceClosure(context.Background(), objects, spec)
	require.NoError(t, err)
	require.Len(t, closed.Files(), len(files)-1)
	for _, p := range []string{"overlay/openapi.json", "overlay/replacements.yaml", "overlay/transformer-patch.yaml"} {
		changed := sourceTransformFixture()
		f := changed[p]
		f.body += "\n"
		changed[p] = f
		objects, spec := sourceObjectsFixture(t, changed)
		next, err := VerifySourceClosure(context.Background(), objects, spec)
		require.NoError(t, err)
		require.NotEqual(t, closed.Digest(), next.Digest(), p)
	}
}
func TestSourceTransformersRefuseUnclosedAndUnsupportedInputs(t *testing.T) {
	for name, change := range map[string]func(map[string]sourceFixtureFile){
		"missing schema":            func(f map[string]sourceFixtureFile) { delete(f, "overlay/openapi.json") },
		"missing replacement":       func(f map[string]sourceFixtureFile) { delete(f, "overlay/replacements.yaml") },
		"missing transformer patch": func(f map[string]sourceFixtureFile) { delete(f, "overlay/transformer-patch.yaml") },
		"remote schema": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "path: openapi.json", "path: https://example.invalid/schema.json", 1)
			f["overlay/kustomization.yaml"] = p
		},
		"nested replacement file": func(f map[string]sourceFixtureFile) {
			f["overlay/replacements.yaml"] = sourceFixtureFile{"100644", "- path: another.yaml\n"}
		},
		"external transformer": func(f map[string]sourceFixtureFile) {
			p := f["overlay/scope.yaml"]
			p.body = strings.Replace(p.body, "apiVersion: builtin", "apiVersion: external/v1", 1)
			f["overlay/scope.yaml"] = p
		},
		"patch missing target": func(f map[string]sourceFixtureFile) {
			p := f["overlay/file-patch.yaml"]
			p.body = strings.Replace(p.body, "target: {kind: Deployment, name: engine}\n", "", 1)
			f["overlay/file-patch.yaml"] = p
		},
		"patch and path": func(f map[string]sourceFixtureFile) {
			p := f["overlay/file-patch.yaml"]
			p.body += "patch: '[]'\n"
			f["overlay/file-patch.yaml"] = p
		},
		"unknown replacement option": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "create: true", "encoding: base64", 1)
			f["overlay/kustomization.yaml"] = p
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := sourceTransformFixture()
			change(files)
			objects, spec := sourceObjectsFixture(t, files)
			closed, err := VerifySourceClosure(context.Background(), objects, spec)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
}
