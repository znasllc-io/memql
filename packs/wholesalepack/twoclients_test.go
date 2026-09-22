package wholesalepack_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// twoclients_test.go -- THE DESIGN'S TEST, RUN AS A TEST (epic memql#5533,
// issue memql#5560).
//
// The design record's section 7 puts ONE question to every concept in this
// pack, before it lands:
//
//	Could two clients with different approval processes both express
//	theirs without editing the pack?
//
// That question is answered here by a test rather than by review, because
// review is how it gets answered "yes" by everybody who already knows the
// answer they want. Two fixture client domains sit over one UNEDITED pack:
//
//   - testdata/clients/northwind approves on a single owner's say, and
//     collects an EIN as a concept of its OWN with an @relationship to the
//     pack's application;
//   - testdata/clients/contoso routes through a v1:commerce:approvalChain
//     with a threshold and notifies a v1:commerce:salesRep.
//
// The test fails if either fixture needs a change under packs/wholesalepack
// -- which is asserted literally, by fingerprinting the pack's own tree
// before and after both fixtures are mounted and validated.
//
// IT IS ALSO THE GATE A THIRD STOREFRONT PACK IS HELD TO. A pack whose
// nouns cannot carry two processes is not a pack; it is one client's
// workflow with a namespace.

const (
	packDomainUnderTest = "wholesale"
	clientNorthwind     = "northwind"
	clientContoso       = "contoso"
)

// mountFixtureClients registers the pack and both fixture domains, and
// unregisters them afterwards.
func mountFixtureClients(t *testing.T) {
	t.Helper()
	// THE SAME ONE-SHOT REGISTRATION THE LIVE HARNESS USES. RegisterTree
	// panics on a second registration of one namespace, and a boot registers
	// once -- so both suites in this package go through the same guard
	// rather than each assuming it is the only test in the process.
	registerPackOnce.Do(func() { wholesalepack.Register(packDomainUnderTest) })
	for _, client := range []string{clientNorthwind, clientContoso} {
		dir := filepath.Join("testdata", "clients", client)
		memqldsl.RegisterTree(client, os.DirFS(dir))
		t.Cleanup(func() { memqldsl.UnregisterTree(client) })
	}
}

// TestTwoClientsExpressDifferentProcessesOverOnePack is the section 7
// question.
func TestTwoClientsExpressDifferentProcessesOverOnePack(t *testing.T) {
	before := fingerprintPackTree(t)

	mountFixtureClients(t)

	// BOTH FIXTURES MUST PARSE. A fixture that cannot parse proves nothing
	// about the pack, so this failure is reported as its own thing rather
	// than folded into the load below.
	for _, client := range []string{clientNorthwind, clientContoso} {
		parseTree(t, client, os.DirFS(filepath.Join("testdata", "clients", client)))
	}

	// AND BOTH MUST LOAD ALONGSIDE THE PACK. This is what proves
	// `use wholesale.concepts.{ application }` resolves from outside the
	// pack, that an @relationship into the pack's namespace binds, and that
	// the pack's concepts are reachable by a domain that is not it.
	if _, err := memql.LoadUnifiedConcepts(slog.Default()); err != nil {
		t.Fatalf("the pack and two client domains must load together: %v", err)
	}
	for _, id := range []string{
		"v1:wholesale:application",
		"v1:northwind:northwindTaxDetail",
		"v1:contoso:contosoReview",
	} {
		if _, err := memorynodes.DefaultRegistry().Get(id); err != nil {
			t.Fatalf("%q must be registered once both clients are mounted: %v", id, err)
		}
	}

	// THE PACK IS UNCHANGED. This is the assertion the issue actually asks
	// for, and it is the reason the test fingerprints rather than eyeballs:
	// "neither fixture needed a change under the pack's directory" is easy
	// to believe and easy to be wrong about.
	if after := fingerprintPackTree(t); after != before {
		t.Fatalf("the pack's own tree changed while two clients were laid over it\n"+
			"  before: %s\n  after:  %s\n"+
			"A pack that has to change to accommodate a client is not a pack.", before, after)
	}
}

// TestTheTwoProcessesAreActuallyDifferent keeps the fixture honest.
//
// WITHOUT THIS, THE TEST ABOVE PASSES WITH TWO COPIES OF ONE PROCESS, which
// would answer the section 7 question with a tautology. The two fixtures
// have to disagree about something a pack could plausibly have hard-coded:
// how many steps there are between an application and an entitlement, and
// whether anybody but the owner is involved.
func TestTheTwoProcessesAreActuallyDifferent(t *testing.T) {
	northwind := readClientSource(t, clientNorthwind)
	contoso := readClientSource(t, clientContoso)

	northwindAutomations := countPrefix(northwind, "automation ")
	contosoAutomations := countPrefix(contoso, "automation ")
	if northwindAutomations == contosoAutomations {
		t.Errorf("both clients declare %d automations; the fixtures are meant to express "+
			"DIFFERENT processes, and two identical shapes prove nothing",
			northwindAutomations)
	}

	// Northwind approves on one owner's say: no chain, nobody notified.
	for _, mustNotAppear := range []string{"approvalChain", "salesRep"} {
		if strings.Contains(northwind, mustNotAppear) {
			t.Errorf("northwind is the fixture with NO chain and nobody to notify, "+
				"but its source mentions %q", mustNotAppear)
		}
	}
	// Contoso routes through a chain and names a rep.
	for _, mustAppear := range []string{"approvalChain", "salesRep"} {
		if !strings.Contains(contoso, mustAppear) {
			t.Errorf("contoso is the fixture that routes through a chain and notifies a "+
				"rep, but its source never mentions %q", mustAppear)
		}
	}

	// ONE OF THEM CARRIES A CLIENT-SPECIFIC FIELD AND THE OTHER DOES NOT.
	// That asymmetry is the whole argument for the EIN being a related
	// concept: if both clients collected one, a field on the pack would
	// have looked reasonable.
	if !strings.Contains(northwind, "ein") {
		t.Error("northwind is the fixture that collects an EIN; its source does not")
	}
	if strings.Contains(contoso, "ein") {
		t.Error("contoso is the fixture that collects NO tax identifier -- it is the " +
			"client whose existence is the argument for keeping the EIN out of the pack")
	}
}

// TestClientFieldsAreRelatedConceptsNotPackFields pins the mechanism.
func TestClientFieldsAreRelatedConceptsNotPackFields(t *testing.T) {
	northwind := readClientSource(t, clientNorthwind)
	if !strings.Contains(northwind, "@relationship(") {
		t.Fatal("northwind's EIN concept must declare an @relationship to the pack's " +
			"application; a client concept that merely holds an applicationId string is " +
			"not a typed relationship and the graph cannot traverse it")
	}
	if !strings.Contains(northwind, "use wholesale.concepts.{ application }") {
		t.Fatal("northwind must import the pack's application to relate to it, which is " +
			"the import that proves the pack's concepts are public surface")
	}
	packSource := readPackSource(t)
	if strings.Contains(packSource, "northwind") || strings.Contains(packSource, "contoso") {
		t.Fatal("the pack names one of its fixture clients. A pack that knows which clients " +
			"exist is the failure this whole test is here to catch")
	}
}

// TestBothClientsReachOnlyTheDeclaredSurface is the narrower half.
//
// A fixture could "not edit the pack" and still depend on something the
// pack never meant to publish. Every wholesale.* name either fixture
// imports has to be a construct the pack actually declares.
func TestBothClientsReachOnlyTheDeclaredSurface(t *testing.T) {
	declared := declaredPackNames(t)
	for _, client := range []string{clientNorthwind, clientContoso} {
		for _, name := range importedWholesaleNames(t, client) {
			if _, ok := declared[name]; !ok {
				t.Errorf("%s imports wholesale.%s, which the pack does not declare",
					client, name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fingerprintPackTree hashes every file in the pack's embedded tree.
//
// SORTED, so the hash is over the tree's content and not over walk order.
func fingerprintPackTree(t *testing.T) string {
	t.Helper()
	tree := wholesalepack.Tree()
	var paths []string
	if err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk pack tree: %v", err)
	}
	sort.Strings(paths)
	sum := sha256.New()
	for _, path := range paths {
		raw, err := fs.ReadFile(tree, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sum.Write([]byte(path))
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func parseTree(t *testing.T, label string, tree fs.FS) {
	t.Helper()
	if err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return walkErr
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Errorf("%s/%s: read: %v", label, path, readErr)
			return nil
		}
		rewritten, rewriteErr := parser.NormaliseAll(string(raw))
		if rewriteErr != nil {
			t.Errorf("%s/%s: rewrite: %v", label, path, rewriteErr)
			return nil
		}
		if _, parseErr := parser.ParseFile(rewritten); parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
			t.Errorf("%s/%s: parse: %v", label, path, parseErr)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", label, err)
	}
}

func readClientSource(t *testing.T, client string) string {
	t.Helper()
	return readAllMemql(t, os.DirFS(filepath.Join("testdata", "clients", client)))
}

func readPackSource(t *testing.T) string {
	t.Helper()
	return readAllMemql(t, wholesalepack.Tree())
}

// readAllMemql concatenates every .memql file in a tree, COMMENTS STRIPPED.
//
// The stripping matters: this pack's files talk about its fixture clients
// at length in comments, and a test that searched the raw text would find
// "northwind" in a sentence explaining why the pack must not know about it.
func readAllMemql(t *testing.T, tree fs.FS) string {
	t.Helper()
	var b strings.Builder
	if err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return err
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "///") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		return nil
	}); err != nil {
		t.Fatalf("read tree: %v", err)
	}
	return b.String()
}

func countPrefix(source, prefix string) int {
	n := 0
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			n++
		}
	}
	return n
}

// declaredPackNames is every construct name the pack declares.
func declaredPackNames(t *testing.T) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for _, line := range strings.Split(readPackSource(t), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "concept", "builtin", "tool":
			out[strings.TrimSuffix(fields[1], "{")] = struct{}{}
		case "shape", "query", "mutation":
			// `shape <concept> <name>` / `query <concept> <name>` /
			// `mutation <concept> <name>` -- the NAME is the third field.
			if len(fields) >= 3 {
				out[strings.TrimSuffix(fields[2], "{")] = struct{}{}
			}
		}
	}
	return out
}

// importedWholesaleNames is every wholesale.* name a client imports.
func importedWholesaleNames(t *testing.T, client string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(readClientSource(t, client), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "use wholesale.") {
			continue
		}
		open := strings.Index(trimmed, "{")
		closeIdx := strings.LastIndex(trimmed, "}")
		if open < 0 || closeIdx < open {
			continue
		}
		for _, name := range strings.Split(trimmed[open+1:closeIdx], ",") {
			if n := strings.TrimSpace(name); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// TestAClientCollectsItsOwnFieldWithoutEditingThePack is the OTHER HALF of
// the section 7 question, and it could not be asked until the shopper-form
// extension existed (design record 2026-09-21).
//
// The test above asks whether two clients can express different PROCESSES.
// It passed while neither fixture could collect a single FIELD from a
// shopper -- northwind declared a concept carrying an EIN, with the
// relationship the design prescribes, and nothing in the engine could ever
// write a row of it. That is the gap this asserts is closed.
func TestAClientCollectsItsOwnFieldWithoutEditingThePack(t *testing.T) {
	before := fingerprintPackTree(t)

	mountFixtureClients(t)

	// THE ANNOTATION HAS TO BE LEGAL DSL IN A REAL CLIENT TREE. parseTree
	// runs the same parse a bundle mounted at MEMQL_DSL_PATH gets, so an
	// annotation the registry refused on a mutation receiver would fail
	// here rather than at somebody's boot.
	parseTree(t, clientNorthwind, os.DirFS(filepath.Join("testdata", "clients", clientNorthwind)))

	// AND THE FIELD IT CARRIES HAS TO BE THE ONE THE CONCEPT DECLARES.
	// northwindTaxDetail.ein existed with nothing able to write it; this is
	// the assertion that a path now exists from a shopper's form post to
	// that field.
	northwind := readClientSource(t, clientNorthwind)
	if !strings.Contains(northwind, `@shopperFormExtension(pack="wholesale", form="application")`) {
		t.Error("northwind does not extend the pack's own application form")
	}
	if !strings.Contains(northwind, "ein") {
		t.Error("northwind's extension does not carry the EIN its concept declares")
	}

	if after := fingerprintPackTree(t); after != before {
		t.Fatalf("the pack's tree changed to let a client collect its own field\n"+
			"  before: %s\n  after:  %s", before, after)
	}
}

// AND CONTOSO STILL DECLARES NONE, which is what keeps the fixture honest.
// A seam every client must use is not a seam, it is a required step; the
// second client collects no tax identifier and says so by declaring
// nothing at all.
func TestTheSecondClientDeclaresNoExtension(t *testing.T) {
	contoso := readClientSource(t, clientContoso)
	if strings.Contains(contoso, "shopperFormExtension") {
		t.Error("contoso declares a shopper-form extension; it is the fixture that collects " +
			"NO client-specific field, and the argument for keeping the EIN out of the pack")
	}
	northwind := readClientSource(t, clientNorthwind)
	if !strings.Contains(northwind, "shopperFormExtension") {
		t.Error("northwind declares no shopper-form extension, so nothing writes the EIN its " +
			"own concept carries -- which is the gap the extension seam closed")
	}
}
