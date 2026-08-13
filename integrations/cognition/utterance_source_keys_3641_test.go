package cognition

// utterance_source_keys_3641_test.go is the guard that lets
// v1:cognition:utterance.source be CLOSED (memql#3641).
//
// insertSystemActionUtterance merges an ARBITRARY caller-supplied
// map[string]string into the source object. That writer being open by
// construction was the last thing blocking the nested-block flip: with the
// block closed, a key nobody declared turns a stored row into a REFUSED insert
// -- on a path whose callers treat the error as non-fatal (`Debug` and move
// on), so the action utterance would simply not appear and nothing would say
// why.
//
// Closing the block is still right: the stored key was never readable by
// anything, because nothing that reads the row knows it is there. What makes it
// safe is catching a new key HERE, in a test, instead of at runtime. So this
// enumerates what each call site passes and checks it against the concept.
//
// The table cannot go stale silently: the call-site COUNT is asserted against
// the package's own source, so a fourth caller fails this test until its keys
// are listed and declared.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemActionSourceWriters is every insertSystemActionUtterance call site and
// the extraSource keys it passes. `outputMethod` and `kind` are added by the
// function itself and are checked alongside them.
var systemActionSourceWriters = []struct {
	site string
	keys []string
}{
	{"ai_router.go: unmet-capability notice", []string{"trigger", "severity"}},
	{"cognition_handler.go: reactive dispatch in-flight", []string{"agentName", "topic"}},
	{"cognition_handler.go: reactive dispatch result", []string{"agentName", "topic"}},
}

// alwaysStampedSourceKeys are written by insertSystemActionUtterance itself for
// every call.
var alwaysStampedSourceKeys = []string{"outputMethod", "kind"}

// declaredUtteranceSourceLeaves reads the sub-field names of the `source` block
// on v1:cognition:utterance, from the real tree.
func declaredUtteranceSourceLeaves(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "dsl", "cognition", "concepts.memql")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	leaves := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inUtterance, inSource, sawSource := false, false, false
	depth := 0
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		opens, closes := strings.Count(trimmed, "{"), strings.Count(trimmed, "}")
		switch {
		case !inUtterance:
			if strings.HasPrefix(trimmed, "concept utterance ") || trimmed == "concept utterance {" {
				inUtterance = true
				depth = opens - closes
			}
			continue
		case inSource:
			if closes > 0 && opens == 0 {
				inSource = false
			} else if fields := strings.Fields(trimmed); len(fields) > 0 {
				leaves[fields[0]] = true
			}
		case trimmed == "source {":
			inSource, sawSource = true, true
		}
		depth += opens - closes
		if depth <= 0 {
			break
		}
	}
	if !sawSource || len(leaves) == 0 {
		t.Fatalf("found no `source {` sub-fields on concept utterance in %s; the concept shape "+
			"changed and this guard has silently stopped protecting anything", path)
	}
	return leaves
}

// TestSystemActionUtteranceSourceKeysAreDeclared: every key this package stamps
// onto an utterance source must be declared, or the insert is refused and the
// notice silently does not appear.
func TestSystemActionUtteranceSourceKeysAreDeclared(t *testing.T) {
	declared := declaredUtteranceSourceLeaves(t)

	check := func(site, key string) {
		if declared[key] {
			return
		}
		t.Errorf("%s writes source.%q, which v1:cognition:utterance does not declare. The block is "+
			"CLOSED (memql#3641), so this insert is REFUSED -- and insertSystemActionUtterance's "+
			"callers log at Debug and continue, so the action utterance just never appears. Declare "+
			"the sub-field on the concept.", site, key)
	}
	for _, w := range systemActionSourceWriters {
		for _, k := range w.keys {
			check(w.site, k)
		}
	}
	for _, k := range alwaysStampedSourceKeys {
		check("insertSystemActionUtterance itself", k)
	}
}

// TestSystemActionUtteranceCallSitesAreAllListed keeps the table above honest.
// Without it a fourth call site could pass a brand-new key, be refused at
// runtime, and this file would still be green.
func TestSystemActionUtteranceCallSitesAreAllListed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, line := range strings.Split(string(src), "\n") {
			// The definition and the doc comment mention the name too; only a
			// call carries the receiver.
			if strings.Contains(line, "c.insertSystemActionUtterance(") {
				found++
			}
		}
	}
	if found != len(systemActionSourceWriters) {
		t.Errorf("found %d insertSystemActionUtterance call site(s), the table lists %d. A call site "+
			"that is not listed can pass an undeclared source key, which the closed block refuses at "+
			"runtime on a path that swallows the error -- list it, and declare its keys.",
			found, len(systemActionSourceWriters))
	}
}
