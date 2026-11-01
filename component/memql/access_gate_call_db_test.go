package memql

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// access_gate_call_db_test.go -- the text AccessGateEntry.Call renders, run
// through a REAL engine and a REAL Postgres.
//
// The handler renders a gated construct's call as namespace.name, because the
// engine resolves a bare name cluster-wide and refuses an ambiguous one. That is
// a claim about the engine, so it is proved against the engine: two namespaces
// declare a query and a mutation of the SAME names, a third declares them
// disabled, and each gated call must reach its own construct, as a query and as
// a mutation, while the bare spelling of either fails. The calls are the ones
// Call() renders, not strings written beside it.
//
// It mounts packs and boots a private engine (readMergeTestEngine's rule), and
// puts the process-wide registries back afterwards.

// gateCallConcept is the concept every namespace in the test reads and writes.
// Its source field is stamped by the mutation with the namespace that wrote the
// row, so a row says which construct ran and not only which concept it landed in.
const gateCallConcept = `
/// A note a gated call writes.
@rowAuthz(owner="ownerUserId")
concept gateCallNote {
  ownerUserId  string
  accessCodeId string
  source       string
  title        string
}
`

// gateCallConstructs is the query and the mutation a namespace declares. The
// names are the same in every namespace, which is the point.
func gateCallConstructs(source, header string) string {
	return header + `
@actor
query gateCallNote gateCallBriefs {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
}

` + header + `
@actor
mutation gateCallNote gateCallRecord {
  args {
    noteId       string!
    accessCodeId string!
    title        string!
  }
  insert {
    accept { title }
    stamp {
      id:           args.noteId
      accessCodeId: args.accessCodeId
      source:       "` + source + `"
      ownerUserId:  actor.userId
    }
  }
}
`
}

// gateCallText renders what the access handler renders: the entry's Call() and
// the arguments as key: JSON value, in a fixed order.
func gateCallText(t *testing.T, entry AccessGateEntry, args map[string]any) string {
	t.Helper()
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, err := json.Marshal(args[key])
		require.NoError(t, err)
		parts = append(parts, key+": "+string(value))
	}
	return entry.Call() + "(" + strings.Join(parts, ", ") + ")"
}

func TestAccessGateCallsReachTheirOwnConstructsThroughTheEngine(t *testing.T) {
	// The namespaces. A and B are gated for two deployables; B's constructs are
	// written in gatecallbext and bound to gatecallb's concept, so its
	// namespace and its domain differ, which is what a call must get right.
	// C declares the same names disabled: the sibling a bare call would trip on.
	trees := map[string]fstest.MapFS{
		"gatecalla": {
			"concepts.memql":   {Data: []byte(gateCallConcept)},
			"constructs.memql": {Data: []byte(gateCallConstructs("gatecalla", `@accessGate(deployable="gate-call-a", gate="dashboard")`))},
		},
		"gatecallb": {
			"concepts.memql": {Data: []byte(gateCallConcept)},
		},
		"gatecallbext": {
			"constructs.memql": {Data: []byte("use gatecallb.concepts.{ gateCallNote }\n\n" +
				gateCallConstructs("gatecallbext", `@accessGate(deployable="gate-call-b", gate="dashboard")`))},
		},
		"gatecallc": {
			"concepts.memql":   {Data: []byte(gateCallConcept)},
			"constructs.memql": {Data: []byte(gateCallConstructs("gatecallc", "@disabled"))},
		},
	}
	before := memorynodes.All()
	for domain, tree := range trees {
		memqldsl.RegisterTree(domain, withLanguageLine(tree))
	}
	t.Cleanup(func() {
		for domain := range trees {
			memqldsl.UnregisterTree(domain)
		}
		memorynodes.ReplaceAll(before)
	})
	restoreAccessGatesAfter(t)

	// Booting publishes the registry, as a node does.
	eng, db, base := readMergeTestEngine(t)
	run := id.NewShortId()
	ctx := auth.ContextWithUserActor(base, "user-gatecall-"+run)
	code := "code-" + run

	lookup := func(deployable, construct, domain string) AccessGateEntry {
		t.Helper()
		got := AccessGateFor(deployable, "dashboard", construct, []string{domain})
		require.NotNil(t, got, "%s/%s of %s is not served after the engine booted", deployable, construct, domain)
		return *got
	}
	queryA, mutationA := lookup("gate-call-a", "gateCallBriefs", "gatecalla"), lookup("gate-call-a", "gateCallRecord", "gatecalla")
	queryB, mutationB := lookup("gate-call-b", "gateCallBriefs", "gatecallb"), lookup("gate-call-b", "gateCallRecord", "gatecallb")

	// The call each entry renders: namespace.name, the file's namespace and not
	// the concept's domain.
	require.Equal(t, "query gatecalla.gateCallBriefs", queryA.Call())
	require.Equal(t, "gatecalla.gateCallRecord", mutationA.Call())
	require.Equal(t, "gatecallb", queryB.Domain)
	require.Equal(t, "query gatecallbext.gateCallBriefs", queryB.Call())
	require.Equal(t, "gatecallbext.gateCallRecord", mutationB.Call())

	// The premise: with the same names in three namespaces, the bare spelling
	// resolves to none of them. A gate that rendered it would be broken by any
	// same-named construct anywhere, the disabled one included.
	for _, bare := range []string{
		`query gateCallBriefs(accessCodeId: "` + code + `")`,
		`gateCallRecord(noteId: "bare-` + run + `", accessCodeId: "` + code + `", title: "bare")`,
		`mutation gateCallRecord(noteId: "bare-` + run + `", accessCodeId: "` + code + `", title: "bare")`,
	} {
		_, err := eng.Execute(ctx, bare)
		require.Error(t, err, "a bare call resolved with the same name in three namespaces: %s", bare)
		require.Contains(t, err.Error(), "not found", bare)
	}
	// And the sibling is really there, and really disabled.
	sibling, found := eng.functions.Lookup("gatecallc.gateCallRecord")
	require.True(t, found, "the disabled sibling did not load")
	require.False(t, sibling.Enabled)
	_, err := eng.Execute(ctx, `query gatecallc.gateCallBriefs(accessCodeId: "`+code+`")`)
	require.ErrorContains(t, err, "disabled")

	// The mutations: each qualified call writes through its own construct.
	write := func(entry AccessGateEntry, noteID, title string) string {
		t.Helper()
		res, err := eng.Execute(ctx, gateCallText(t, entry, map[string]any{
			"noteId": noteID, "accessCodeId": code, "title": title,
		}))
		require.NoError(t, err, "the call %s must run", entry.Call())
		require.Len(t, res.Bundle.Nodes, 1)
		return res.Bundle.Nodes[0].Id
	}
	noteA := write(mutationA, "a-"+run, "written through A")
	noteB := write(mutationB, "b-"+run, "written through B")

	payloadA := latestPayload(t, ctx, db, "v1:gatecalla:gateCallNote", noteA)
	require.Equal(t, "gatecalla", payloadA["source"], "the mutation of gatecalla did not run")
	require.Equal(t, "written through A", payloadA["title"])
	payloadB := latestPayload(t, ctx, db, "v1:gatecallb:gateCallNote", noteB)
	require.Equal(t, "gatecallbext", payloadB["source"], "the mutation of gatecallbext did not run")
	require.Equal(t, "written through B", payloadB["title"])

	// The queries: each qualified call reads through its own construct, and
	// sees the row only its own namespace wrote.
	read := func(entry AccessGateEntry) []string {
		t.Helper()
		res, err := eng.Execute(ctx, gateCallText(t, entry, map[string]any{"accessCodeId": code}))
		require.NoError(t, err, "the call %s must run", entry.Call())
		ids := make([]string, 0, len(res.Bundle.Nodes))
		for _, node := range res.Bundle.Nodes {
			ids = append(ids, node.Id)
		}
		return ids
	}
	require.Equal(t, []string{noteA}, read(queryA), "the query of gatecalla did not read its own row")
	require.Equal(t, []string{noteB}, read(queryB), "the query of gatecallbext did not read its own row")
}
