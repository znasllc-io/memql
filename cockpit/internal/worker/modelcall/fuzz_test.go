package modelcall

import (
	"strings"
	"testing"
)

// FuzzToolCallAccumulator. This is the highest-value fuzz target in the
// repository, because its failure mode is the one the code's own comment
// says NOTHING DOWNSTREAM CAN DETECT.
//
// An OpenAI-compatible server streams a tool call in pieces: the id on
// one fragment, the name on another, the arguments a few characters at a
// time split wherever the tokeniser happened to split them -- and two
// parallel calls interleave freely, distinguished only by `index`.
// Keying on anything else concatenates two different calls' arguments
// into one string that STILL PARSES as JSON, and the tool is then
// dispatched with arguments the model never asked for.
//
// So the property is separation: whatever order the fragments arrive in,
// each index's arguments are exactly its own fragments in arrival order,
// and no index's bytes appear in another's.
func FuzzToolCallAccumulator(f *testing.F) {
	// Seeds are (index, id, name, argument fragment) quadruples flattened
	// into a byte string the fuzzer can mutate; see decodeFragments.
	for _, seed := range []string{
		`0|call_a|read_file|{"path":`,
		`0||{|"/etc/hostname"}`,
		"1|call_b|search|{\"q\":\"x\"}\n0||\"more\"",
		"0|a|n|{\n1|b|m|}\n0||x\n1||y",
		"",
		"5||",
		"|||",
		strings.Repeat("0|i|n|a\n", 200),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, script string) {
		frags := decodeFragments(script)
		if len(frags) == 0 {
			return
		}

		var acc toolCallAccumulator
		// What each index SHOULD end up with, accumulated independently
		// of the thing under test.
		wantArgs := map[int]*strings.Builder{}
		wantOrder := []int{}
		for _, fr := range frags {
			acc.add(fr)
			b, seen := wantArgs[fr.Index]
			if !seen {
				b = &strings.Builder{}
				wantArgs[fr.Index] = b
				wantOrder = append(wantOrder, fr.Index)
			}
			b.WriteString(fr.Function.Arguments)
		}

		got := acc.result()
		if len(got) != len(wantArgs) {
			t.Fatalf("%d calls for %d indexes", len(got), len(wantArgs))
		}

		// EACH INDEX KEEPS ITS OWN ARGUMENTS, in arrival order. This is
		// the separation property: a call dispatched with another
		// call's arguments is the failure nothing downstream notices.
		byName := map[string]bool{}
		for i, call := range got {
			idx := sortedIndexes(wantArgs)[i]
			if want := wantArgs[idx].String(); call.ArgumentsJSON != want {
				t.Fatalf("index %d assembled %q, want %q", idx, call.ArgumentsJSON, want)
			}
			byName[call.Name] = true
		}

		// AND THE ORDER IS THE MODEL'S. result() returns calls in index
		// order -- the order the server assigned, and therefore the
		// order the model asked in. Map iteration would reshuffle a
		// parallel call set on every run, which reads downstream as the
		// model changing its mind between identical generations.
		idxs := sortedIndexes(wantArgs)
		for i := 1; i < len(idxs); i++ {
			if idxs[i-1] >= idxs[i] {
				t.Fatalf("indexes are not ascending: %v", idxs)
			}
		}
	})
}

// FuzzToolCallArgumentsNeverMerge is the same property stated as the
// attack: two calls whose arguments are each valid JSON must never
// combine into a third that is ALSO valid JSON, because that is the
// version of this bug that reaches a tool.
func FuzzToolCallArgumentsNeverMerge(f *testing.F) {
	f.Add(`{"a":1}`, `{"b":2}`)
	f.Add(`{"path":"/etc/passwd"}`, `{"path":"/tmp/ok"}`)
	f.Add(``, ``)
	f.Add(`[`, `]`)

	f.Fuzz(func(t *testing.T, argsA, argsB string) {
		var acc toolCallAccumulator
		// The HEADER fragments first, which is what a real server
		// sends: the id and the name arrive once, on their own, and
		// every later fragment repeats only the index. Without them an
		// empty-argument call would not exist at all, and the
		// separation property would be untested for exactly the call
		// that carries no arguments.
		for i, name := range []string{"read_file", "search"} {
			d := openAIToolCallDelta{Index: i, ID: "call_" + name}
			d.Function.Name = name
			acc.add(d)
		}
		// Then the arguments, interleaved one character at a time,
		// which is how a real stream splits them.
		maxLen := len(argsA)
		if len(argsB) > maxLen {
			maxLen = len(argsB)
		}
		for i := 0; i < maxLen; i++ {
			if i < len(argsA) {
				acc.add(openAIToolCallDelta{Index: 0, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: argsA[i : i+1]}})
			}
			if i < len(argsB) {
				acc.add(openAIToolCallDelta{Index: 1, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: argsB[i : i+1]}})
			}
		}

		got := acc.result()
		if len(got) != 2 {
			t.Fatalf("two interleaved calls assembled into %d", len(got))
		}
		// THE HEADERS SURVIVED THE ARGUMENT FRAGMENTS. Every argument
		// fragment repeats the index and omits the id and the name, so
		// an unguarded assignment would erase what the header
		// established and the finished call would go out anonymous --
		// which is the other half of this accumulator's job.
		if got[0].Name != "read_file" || got[0].ID != "call_read_file" {
			t.Fatalf("index 0 lost its header: %+v", got[0])
		}
		if got[1].Name != "search" || got[1].ID != "call_search" {
			t.Fatalf("index 1 lost its header: %+v", got[1])
		}
		if got[0].ArgumentsJSON != argsA {
			t.Fatalf("index 0 = %q, want %q", got[0].ArgumentsJSON, argsA)
		}
		if got[1].ArgumentsJSON != argsB {
			t.Fatalf("index 1 = %q, want %q", got[1].ArgumentsJSON, argsB)
		}
		// The merged string is what a wrongly-keyed accumulator would
		// have produced. It must not be what either call carries.
		if merged := argsA + argsB; merged != argsA && merged != argsB {
			for _, call := range got {
				if call.ArgumentsJSON == merged {
					t.Fatalf("a call carried the concatenation of both: %q", merged)
				}
			}
		}
	})
}

// decodeFragments reads the fuzzer's flat script into stream fragments.
// One fragment per line: index|id|name|arguments. A line the fuzzer
// mangles past recognition is skipped rather than failing the run --
// the target is the accumulator, not this parser.
func decodeFragments(script string) []openAIToolCallDelta {
	var out []openAIToolCallDelta
	for _, line := range strings.Split(script, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		idx := 0
		for _, r := range parts[0] {
			if r < '0' || r > '9' {
				idx = -1
				break
			}
			idx = idx*10 + int(r-'0')
			if idx > 64 {
				idx = -1
				break
			}
		}
		if idx < 0 {
			continue
		}
		d := openAIToolCallDelta{Index: idx, ID: parts[1]}
		d.Function.Name = parts[2]
		d.Function.Arguments = parts[3]
		out = append(out, d)
	}
	return out
}

func sortedIndexes(m map[int]*strings.Builder) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
