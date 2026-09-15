package sense

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// A keyword key completes to what the parser accepts: a flag key is written
// bare (`@mode(queued)`, `@rowAuthz(..., clusterOwner)`), every other key
// with its `=`. Inserting `queued=` wrote source the registry refuses as
// annotation_key ("a flag and takes no value"), and @mode is an annotation
// whose every choice but max is a flag (epic memql#5380).

// completeAtLineEnd returns what Complete offers with the cursor at the end
// of the line of src that holds needle.
func completeAtLineEnd(t *testing.T, src, needle, file string) []CompletionItem {
	t.Helper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, needle) {
			return New(nil).Complete(src, i+1, len(line)+1, file)
		}
	}
	t.Fatalf("no line of the source holds %q", needle)
	return nil
}

// insertTextOf returns the insert text of the item labelled label, and
// whether one was offered.
func insertTextOf(items []CompletionItem, label string) (string, bool) {
	for _, it := range items {
		if it.Label == label {
			return it.InsertText, true
		}
	}
	return "", false
}

func TestAnnotationKeyCompletionWritesWhatTheParserAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, src, needle, file, label, want string
	}{
		{
			name: "a @mode flag completes bare",
			src: `@trigger(event="node.created", concept="v1:cluster:node")
@mode(que
automation welcomeNode {
  step raise {
    mutation raiseTicket (title: "A node joined")
  }
}`,
			needle: "@mode(que", file: "x/automations.memql",
			label: "queued", want: "queued",
		},
		{
			name: "a valued @loop key completes with its =",
			src: `@trigger(event="node.created", concept="v1:probe:ticket")
@filter(row => row.status != "done")
@loop(max
automation advanceTicket {
  step advance {
    mutation advanceTicket (id: "t-1", status: "done")
  }
}`,
			needle: "@loop(max", file: "x/automations.memql",
			label: "maxDepth", want: "maxDepth=",
		},
		{
			name: "a @rowAuthz flag completes bare after a valued key",
			src: `@rowAuthz(owner="ownerUserId", clus
concept ticket {
  ownerUserId  string
  title        string
}`,
			needle: "@rowAuthz(", file: "x/concepts.memql",
			label: "clusterOwner", want: "clusterOwner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := completeAtLineEnd(t, tc.src, tc.needle, tc.file)
			got, ok := insertTextOf(items, tc.label)
			if !ok {
				t.Fatalf("%q was not offered; got %d items: %v", tc.label, len(items), labelSet(items))
			}
			if got != tc.want {
				t.Errorf("%q inserts %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

// TestAnnotationKeyInsertTextFollowsTheKeyType: across every annotation that
// takes keyword arguments, a flag key inserts its bare name and every other
// key its name and `=` -- the shape the registry's key check requires.
func TestAnnotationKeyInsertTextFollowsTheKeyType(t *testing.T) {
	s := New(nil)
	flags := 0
	for name, keys := range annotations.KeywordArgs {
		items := s.completeAnnotationArgs(CursorContext{AnnotationName: name})
		for _, k := range keys {
			want := k.Name + "="
			if k.Type == "flag" {
				want = k.Name
				flags++
			}
			got, ok := insertTextOf(items, k.Name)
			if !ok {
				t.Errorf("@%s: key %s was not offered", name, k.Name)
				continue
			}
			if got != want {
				t.Errorf("@%s: key %s (%s) inserts %q, want %q", name, k.Name, k.Type, got, want)
			}
		}
	}
	if flags == 0 {
		t.Fatal("no annotation declares a flag key, so the flag half of this test checked nothing")
	}
}
