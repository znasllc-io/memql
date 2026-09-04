package workbench

import (
	"context"
	"strings"
	"testing"
)

// TestTheBuildDirectoryIsSafeInsideAPathList pins that a build's working
// directory can appear inside a colon-separated list without being split.
//
// The build directory used to be "deployment:<id>-<app>" under
// MEMQL_WORKBENCH_ROOT. `npm run` composes the script PATH by joining every
// ancestor's node_modules/.bin with ":", so a colon INSIDE the directory name
// split that entry in two garbage halves and every locally installed bin was
// "not found": on aks-memql at v0.20.18 the storefront's `npm ci` reported
// "added 236 packages in 3s" and `npm run build` answered
// "sh: 1: astro: not found" with node_modules/.bin/astro sitting right there.
// The same tree in a directory without a colon built in under a second. The
// key is also the plan_id slot of the forward request, which has no opinion
// about its shape, and a directory name, which has exactly one: no ':'.
func TestTheBuildDirectoryIsSafeInsideAPathList(t *testing.T) {
	req := buildRequest(t, "true", webFixture())
	if key := buildKey(req); strings.ContainsAny(key, ":") {
		t.Fatalf("the build key %q carries a ':' and would be split by anything that joins PATH entries with one", key)
	}

	// The reachable positive, end to end: the command sees its working
	// directory, refuses if a ':' is in it, and otherwise builds. This is what
	// `npm run <bin>` needs of the directory, without needing npm in the test.
	integ, _ := buildIntegration(t)
	res := integ.RunBuild(context.Background(), buildRequest(t,
		`case "$PWD" in *:*) echo "colon in $PWD" >&2; exit 1;; esac; mkdir -p dist && printf 'ok' > dist/index.html`,
		webFixture()), "")
	if !res.OK {
		t.Fatalf("the build must run in a colon-free directory: %s: %s\n%s", res.ErrorCode, res.ErrorMessage, res.LogTail)
	}
	if got := unpack(t, res.Output.Inline)["dist/index.html"]; got != "ok" {
		t.Fatalf("got %q", got)
	}
}
