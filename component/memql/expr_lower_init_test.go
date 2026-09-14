package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// expr_lower_init_test.go -- the one lowering at boot (memql#5366, D11: "Lower
// runs in MemQLEngine.Init for every P-position expression; a node that does
// not lower is a load refusal"). Each test mounts a small edition-2026 domain
// over the embedded tree and runs the engine's real Init, so what is asserted
// is what a product bundle mounted at MEMQL_DSL_PATH would get at boot.

const lowerInitConcepts = `@version("1.0.0")
@namespace("lowerinit")
@description("A ticket the lowering boot tests read.")
concept ticket {
  status    string!   @description("Workflow state.")
  title     string    @description("Title.")
  priority  int       @description("Priority.")
  tags      []string  @description("Tags.")
  overage   float     @description("Accrued overage.")
  reported  float     @description("Overage already reported.")
}
`

const lowerInitSpecs = `use lowerinit.concepts.{ ticket }
use common.shapes.{ actorEnvelope }

/// Matches open tickets.
spec ticket isOpenTicket = row => row.status == "open"

/// Matches tickets that owe a report: the two-field comparison the fleet spec needed.
spec ticket owesReport = row => row.overage > row.reported

/// Matches tickets tagged urgent, on any concept that has tags.
trait isUrgentTicket = row => "urgent" in row.tags

/// The caller holds the owner role.
spec actorEnvelope callerIsOwner = actor => actor.role == "owner"
`

// bootLowerTree mounts files as the `lowerinit` domain over the embedded tree
// and runs Init. Cleanup restores the embedded-only tree and registry.
func bootLowerTree(t *testing.T, files map[string]string) (*MemQLEngine, error) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	root := fstest.MapFS{}
	for name, body := range files {
		root["lowerinit/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	_, _, unmount := memqldsl.MountOverlayDomains(logger, root)
	t.Cleanup(func() {
		unmount()
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(logger)
	})
	_, err := LoadUnifiedConcepts(logger)
	require.NoError(t, err)
	eng, err := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	require.NoError(t, err)
	eng.Logger = logger
	return eng, eng.Init(concept.DefaultRegistry())
}

// lowerInitSkips is the load report's entries for the fixture domain.
func lowerInitSkips(eng *MemQLEngine) []string {
	var out []string
	if eng.loadReport == nil {
		return nil
	}
	for _, s := range eng.loadReport.Skipped {
		if strings.Contains(s.File, "lowerinit") {
			out = append(out, s.Keyword+" "+s.Name+" ("+s.Phase+"): "+s.Err)
		}
	}
	return out
}

func TestInit_LowersEditionTwentySixFiltersSpecsAndTraits(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts,
		"specs.memql":    lowerInitSpecs,
		"queries.memql": `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ isOpenTicket, owesReport, isUrgentTicket, callerIsOwner }

/// Open tickets at or above a priority floor, owing a report, not urgent.
@actor
query ticket openAbove {
  args {
    floor  int!
  }
  filter   row => isOpenTicket(row) && row.priority >= args.floor && owesReport(row) && !isUrgentTicket(row) || callerIsOwner(actor)
  sort     "row.createdAt", "desc"
  paginate 20
}

/// Tickets whose title mentions a word, refined in process over the page.
query ticket titled {
  args {
    word  string!
  }
  filter   row => row.status != nil
  sort     "row.createdAt", "desc"
  paginate 20
  refine   row => lower(row.title).includes(lower(args.word)) && isOpenTicket(row)
}
`,
	})
	require.NoError(t, err, "a tree of edition-2026 filters, specs and traits boots")
	require.Empty(t, lowerInitSkips(eng))

	fn, err := eng.Functions().Get("openAbove")
	require.NoError(t, err)
	require.NotNil(t, fn.V1Filter, "the filter as written is kept for the Init pass's second lowering")
	// The references come back QUALIFIED: resolveConstructReferences, which
	// runs after the lowering, qualifies a lowered spec reference exactly as it
	// qualifies a legacy one -- the lowering emits the same IR node.
	require.Equal(t,
		`AND(OR(AND(!(spec(lowerinit.isurgentticket)),payload.priority>=&{floor},spec(lowerinit.isopenticket),spec(lowerinit.owesreport)),spec(lowerinit.callerisowner)),concept=="v1:lowerinit:ticket")`,
		canonicalExpression(unwrapToFilter(fn.Expr)),
		"the v1 filter is lowered and joined onto the query's resolved concept binding")

	open, err := eng.specs.Get("isOpenTicket")
	require.NoError(t, err)
	require.Equal(t, SpecKindRow, open.Kind)
	require.Equal(t, `payload.status=="open"`, canonicalExpression(open.Expr), "the spec body is lowered at Init, against its concept")
	owes, err := eng.specs.Get("owesReport")
	require.NoError(t, err)
	require.Equal(t, `payload.overage>field(payload.reported)`, canonicalExpression(owes.Expr))
	urgent, err := eng.specs.Get("isUrgentTicket")
	require.NoError(t, err)
	require.Equal(t, `payload.tagshas"urgent"`, canonicalExpression(urgent.Expr))
	owner, err := eng.specs.Get("callerIsOwner")
	require.NoError(t, err)
	require.Equal(t, SpecKindContext, owner.Kind)
	require.Equal(t, `actor.role=="owner"`, canonicalExpression(owner.Expr), "a context spec reads the envelope through its @actor shape")

	titled, err := eng.Functions().Get("titled")
	require.NoError(t, err)
	require.NotNil(t, refineIn(titled.Expr), "the refine clause is part of the registered query")
}

func TestInit_RefusesAnInProcessFunctionOverTheRowNamingTheFix(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts,
		"queries.memql": `use lowerinit.concepts.{ ticket }

/// Refused at boot: lower() runs in process and reads the row.
query ticket byLoweredTitle {
  args {
    q  string!
  }
  filter   row => lower(row.title) == args.q
  paginate 20
}
`,
	})
	require.Error(t, err, "strict boot refuses a filter that does not lower")
	require.Contains(t, err.Error(), "strict DSL boot refused")
	skips := strings.Join(lowerInitSkips(eng), "\n")
	require.Contains(t, skips, "byLoweredTitle")
	require.Contains(t, skips, "`lower(row.title)` does not lower in a query filter: `lower` runs in process and reads the row")
	require.Contains(t, skips, "Compare against a computed value instead: `row.title == lower(args.q)`")
}

func TestInit_RefusesWhatOnlyTheRegistryCanRefuse(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts,
		"specs.memql":    lowerInitSpecs,
		"legacy.memql": `use lowerinit.concepts.{ ticket }

/// A pre-2026 body: EvalExpr has no v1 body to apply in a refine.
spec ticket isLegacyOpen {
  return status == "open"
}
`,
		"queries.memql": `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ callerIsOwner, isOpenTicket }
use lowerinit.legacy.{ isLegacyOpen }

/// Refused at Init: a context spec applied to the row.
query ticket misKinded {
  filter   row => callerIsOwner(row)
  paginate 20
}

/// Refused at Init: a refine applying a pre-2026 spec.
query ticket refinesALegacySpec {
  filter   row => isOpenTicket(row)
  paginate 20
  refine   row => isLegacyOpen(row)
}

/// Refused at Init: a refine whose nested scans exceed the static cost bound.
query ticket refinesTooMuch {
  filter   row => isOpenTicket(row)
  paginate 20
  refine   row => row.tags.any(a => row.tags.any(b => row.tags.any(c => a == b && b == c)))
}
`,
	})
	require.Error(t, err)
	skips := strings.Join(lowerInitSkips(eng), "\n")
	require.Contains(t, skips, "misKinded")
	require.Contains(t, skips, "`callerIsOwner(row)` does not lower in a query filter: `callerIsOwner` is a context spec over the actor")
	require.Contains(t, skips, "`callerIsOwner(actor)`")
	require.Contains(t, skips, "refinesALegacySpec")
	require.Contains(t, skips, "does not lower in a refine clause: `isLegacyOpen` has a pre-2026 body")
	require.Contains(t, skips, "refinesTooMuch")
	require.Contains(t, skips, "above tiers.MaxStaticCost")
}

func TestInit_RefusesASpecBodyThatDoesNotLower(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts,
		"specs.memql": `use lowerinit.concepts.{ ticket }

/// Refused at Init: title is a string, and a spec body is a condition.
spec ticket titleAsCondition = row => row.title

/// Refused at Init: a field the concept does not declare.
spec ticket readsAStranger = row => row.statuss == "open"
`,
	})
	require.Error(t, err)
	skips := strings.Join(lowerInitSkips(eng), "\n")
	require.Contains(t, skips, "titleAsCondition")
	require.Contains(t, skips, "`row.title` does not lower in a spec or trait body: `row.title` is a string, and a condition must be boolean")
	require.Contains(t, skips, "readsAStranger")
	require.Contains(t, skips, "Did you mean `row.status`?")
}
