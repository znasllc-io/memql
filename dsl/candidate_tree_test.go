package dsl

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCandidateTreeReplacesOnlyRuntimeDomainsInItsPrivateView(t *testing.T) {
	registerTree("candidateowned", fstest.MapFS{"concepts.memql": {Data: []byte("installed")}}, true)
	t.Cleanup(func() { UnregisterTree("candidateowned") })
	RegisterTree("candidatecompiled", fstest.MapFS{"concepts.memql": {Data: []byte("compiled")}})
	t.Cleanup(func() { UnregisterTree("candidatecompiled") })

	for _, body := range []string{"first candidate", "second candidate"} {
		view, domains, reserved, err := CandidateTree(fstest.MapFS{
			"candidateowned/concepts.memql": {Data: []byte(body)},
			"identity/concepts.memql":       {Data: []byte("must never replace core")},
		})
		if err != nil || !reflect.DeepEqual(domains, []string{"candidateowned"}) || !reflect.DeepEqual(reserved, []string{"identity"}) {
			t.Fatalf("candidate view: domains=%v reserved=%v err=%v", domains, reserved, err)
		}
		for _, tc := range []struct {
			tree fs.FS
			path string
			want string
		}{
			{view, "candidateowned/concepts.memql", body},
			{Tree(), "candidateowned/concepts.memql", "installed"},
			{view, "candidatecompiled/concepts.memql", "compiled"},
		} {
			got, err := fs.ReadFile(tc.tree, tc.path)
			if err != nil || string(got) != tc.want {
				t.Errorf("%s: got %q, %v; want %q", tc.path, got, err, tc.want)
			}
		}
	}
	if _, _, _, err := CandidateTree(fstest.MapFS{
		"candidatecompiled/concepts.memql": {Data: []byte("replacement")},
	}); err == nil || !strings.Contains(err.Error(), "compiled-in pack") {
		t.Fatalf("candidate accepted a replacement boot cannot mount: %v", err)
	}
	if got, err := fs.ReadFile(Tree(), "candidatecompiled/concepts.memql"); err != nil || string(got) != "compiled" {
		t.Fatalf("refused candidate changed compiled domain: %q, %v", got, err)
	}
}
