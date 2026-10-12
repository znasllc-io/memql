package memql

import (
	"encoding/json"
	"math"
	"runtime"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A BUILTIN'S ANSWER LEAVES THE ENGINE AS PLAIN JSON.
//
// A builtin hands back memorynodes.MemoryNode values whose Payload is a
// json.RawMessage, bytes the builtin already wrote. encoding/json's default
// writes each "<", ">" and "&" of a raw message as six bytes (<) when it
// marshals the node, a safeguard for JSON that is put inside HTML, which this
// is not, and neither of the two places that marshal a node set needs it: the
// bytes are read again straight away, by json.Unmarshal.
//
//   - ExecuteResult.ToAPIResult, the way a result reaches the gRPC wire, marshals
//     and reads the whole output, node set included;
//   - nodeSetRows, behind MaterializeRows, marshals each node to read it as a row.
//
// So an answer that carries an email (the emailRender preview, a layout, a
// template: 1 to 8 MB of markup, "<" and "&" in every tag) cost a multiple of
// its size at each place for nothing. Measured before the fix, a payload of
// 8.3 MB of "<": 343 MiB in ToAPIResult against 23 MiB for the same size of
// letters.
//
// The tests give the same size of "<" and of letters to each place and hold the
// cost of the first to that of the second, which does not depend on the
// machine, and show that what is read is the same text as ever.

// plainJSONTextBytes is the size of the text in each payload below: large
// enough that its escaping dwarfs everything else the two places allocate, and
// not a power of two, since a buffer that grows by doubling costs one more
// doubling for a payload of exactly that size and a few bytes over.
const plainJSONTextBytes = 3 << 19

// plainJSONNodes is a builtin's node set of one node whose payload is
// {"text": text}, written as the builtin writes it: plain JSON. text must hold
// nothing JSON needs escaped.
func plainJSONNodes(text string) map[string]memorynodes.MemoryNode {
	return map[string]memorynodes.MemoryNode{"answer": {
		ID:         "answer",
		Concept:    "integration:test:answer",
		Type:       memorynodes.NodeTypeObject,
		Payload:    json.RawMessage(`{"text":"` + text + `"}`),
		Schema:     json.RawMessage(`{}`),
		Provenance: json.RawMessage(`{}`),
	}}
}

// bytesAllocatedWhile is the bytes allocated while f runs. It is what a place
// costs the node, and unlike the time it does not depend on the machine. The
// counter is the process's, so whatever else is running (a test before this one
// that left goroutines behind) is in it too: the cost of f is the least of a few
// runs, which only ever have noise added to them.
func bytesAllocatedWhile(f func()) uint64 {
	least := uint64(math.MaxUint64)
	for run := 0; run < allocationRuns; run++ {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least
}

// allocationRuns is how many times a cost is measured.
const allocationRuns = 5

// requireNoMoreThanLetters fails the test when the text of "<" cost more than
// three copies of the text above what the same size of letters cost.
//
// The two are not exactly equal and not always in the same direction: the JSON
// encoder keeps a buffer in a sync.Pool, so whether one run finds it at hand and
// the other allocates it again, which depends on which processor the goroutine is
// on and on what ran before, is the difference of one copy of the text (seen in
// the whole package and not alone). Escaping the markup costs ten copies of the
// text or more (measured: 10 in MaterializeRows and 44 in ToAPIResult), so three
// is a line that neither side crosses by accident.
func requireNoMoreThanLetters(t *testing.T, place string, markup, letters uint64) {
	t.Helper()
	t.Logf("%s: %d bytes for %d bytes of \"<\", %d for the same size of letters", place, markup, plainJSONTextBytes, letters)
	if markup > letters+3*plainJSONTextBytes {
		t.Errorf("%s allocated %d bytes for %d bytes of \"<\" and %d for the same size of letters, more than three copies of the text above it: "+
			"the answer's markup is escaped on the way out", place, markup, plainJSONTextBytes, letters)
	}
}

func TestToAPIResultDoesNotEscapeAnswersMarkup(t *testing.T) {
	read := func(text string) uint64 {
		result := NewResultWithOutput(plainJSONNodes(text))
		return bytesAllocatedWhile(func() {
			if _, _, err := result.ToAPIResult(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Letters first, so that nothing the "<" run left behind is counted against it.
	letters := read(strings.Repeat("a", plainJSONTextBytes))
	markup := read(strings.Repeat("<", plainJSONTextBytes))
	requireNoMoreThanLetters(t, "ToAPIResult", markup, letters)
}

func TestMaterializeRowsDoesNotEscapeAnswersMarkup(t *testing.T) {
	read := func(text string) uint64 {
		nodes := plainJSONNodes(text)
		return bytesAllocatedWhile(func() {
			if rows := MaterializeRows(nodes); len(rows) != 1 {
				t.Fatalf("MaterializeRows read %d rows from a node set of 1", len(rows))
			}
		})
	}
	letters := read(strings.Repeat("a", plainJSONTextBytes))
	markup := read(strings.Repeat("<", plainJSONTextBytes))
	requireNoMoreThanLetters(t, "MaterializeRows", markup, letters)
}

// The text that comes out is the text that went in: leaving the escapes off the
// bytes changes how they are written and nothing a reader reads. The text holds
// every character the encoder treats as markup, a quote and a newline, which
// JSON does need escaped and which stay escaped.
func TestAnswersMarkupReadsBackAsItWasWritten(t *testing.T) {
	const text = "<p class=\"a\">Tom & Jerry</p>\n<b> </b> > 1 & 2"
	body, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]memorynodes.MemoryNode{"answer": {
		ID: "answer", Concept: "integration:test:answer", Type: memorynodes.NodeTypeObject,
		Payload: json.RawMessage(`{"text":` + string(body) + `}`), Schema: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
	}}

	t.Run("ToAPIResult", func(t *testing.T) {
		_, data, err := NewResultWithOutput(nodes).ToAPIResult()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 1 {
			t.Fatalf("ToAPIResult returned %d values, want 1", len(data))
		}
		node := data[0].GetStructValue().GetFields()["answer"].GetStructValue()
		if node == nil {
			t.Fatalf("the node is not in the answer: %v", data[0])
		}
		got := node.GetFields()["payload"].GetStructValue().GetFields()["text"].GetStringValue()
		if got != text {
			t.Errorf("the text read back is %q, want %q", got, text)
		}
		if node.GetFields()["concept"].GetStringValue() != "integration:test:answer" {
			t.Errorf("the node's other columns are gone: %v", node)
		}
	})

	t.Run("MaterializeRows", func(t *testing.T) {
		rows := MaterializeRows(nodes)
		if len(rows) != 1 {
			t.Fatalf("MaterializeRows read %d rows, want 1", len(rows))
		}
		if rows[0]["text"] != text {
			t.Errorf("the text read back is %q, want %q", rows[0]["text"], text)
		}
		payload, _ := rows[0]["payload"].(map[string]any)
		if payload["text"] != text {
			t.Errorf("the nested payload's text is %q, want %q", payload["text"], text)
		}
		if rows[0]["concept"] != "integration:test:answer" {
			t.Errorf("the node's other columns are gone: %v", rows[0])
		}
	})
}
