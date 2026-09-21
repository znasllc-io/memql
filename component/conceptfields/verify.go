package conceptfields

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Result is one evaluation of the tree against the committed snapshot.
type Result struct {
	// Snapshot is what should be committed: the current tree's concepts plus
	// the committed ledger, carried forward.
	Snapshot Snapshot
	// Committed and Wanted are the bytes on disk and the bytes that should be
	// on disk. ONE rendering produces both, so `--check` can never report drift
	// that regenerating does not fix.
	Committed []byte
	Wanted    []byte
	// Unrecorded is every narrowing the ledger does not cover.
	Unrecorded []Narrowing
	// Ledger is every problem with the ledger itself, in either direction.
	Ledger []string
}

// Verify reads the tree and the committed snapshot and answers everything both
// the generator and the drift test need.
//
// ONE FUNCTION, TWO CALLERS, and that is the point: cmd/conceptsnapshot and
// TestConceptFieldSnapshotIsNotStale ask the same question, and a gate whose
// answer differed from the fix command's would be a gate nobody could satisfy.
func Verify(dslRoot, snapshotPath, migrationsDir string) (Result, error) {
	committedSnap, err := Load(snapshotPath)
	if err != nil {
		return Result{}, err
	}
	current, err := scanFrom(dslRoot)
	if err != nil {
		return Result{}, err
	}
	next, unrecorded := Reconcile(committedSnap, current)

	wanted, err := Marshal(next)
	if err != nil {
		return Result{}, err
	}
	committedBytes, err := os.ReadFile(snapshotPath)
	if os.IsNotExist(err) {
		committedBytes = nil
	} else if err != nil {
		return Result{}, err
	}

	return Result{
		Snapshot:   next,
		Committed:  committedBytes,
		Wanted:     wanted,
		Unrecorded: unrecorded,
		Ledger:     VerifyLedger(next, migrationsDir),
	}, nil
}

// Problem renders everything wrong, or "" when nothing is.
//
// The message is the whole user interface of this gate. Somebody meets it
// mid-way through a branch that felt finished, so it says what changed, why it
// matters, and the exact line to add -- rather than naming a rule and leaving
// them to find the format.
func (r Result) Problem() string {
	if len(r.Unrecorded) == 0 && len(r.Ledger) == 0 {
		return ""
	}
	var b strings.Builder

	if len(r.Unrecorded) > 0 {
		fmt.Fprintf(&b, "%s\n", header(len(r.Unrecorded)))
		b.WriteString(
			"\nRemoving a field from a concept BRICKS every stored row that still carries it: the\n" +
				"concept's schema sets additionalProperties=false and a mutation's read-merge validates\n" +
				"the MERGED payload, so the next write to such a row is refused with\n" +
				"`additionalProperties '<field>' not allowed`. CI cannot see it -- the db-tests lane runs\n" +
				"against a fresh database whose boot seed writes clean rows -- so it appears first on a\n" +
				"real installation, as a write that fails on every boot.\n\n" +
				"Each line below needs a `retired` entry in the snapshot, carrying EITHER the migration\n" +
				"that repairs the rows OR a waiver saying why none is needed:\n\n")
		for _, n := range r.Unrecorded {
			fmt.Fprintf(&b, "  %s\n", n.String())
			fmt.Fprintf(&b, "      %s\n\n", ledgerTemplate(n))
		}
		b.WriteString(
			"A migration strips the key SCOPED TO ITS CONCEPT, never by key name --\n" +
				"`UPDATE \"MemoryNodes\" SET payload = payload - '<field>'\n" +
				"   WHERE concept = '<concept>' AND payload ? '<field>'` --\n" +
				"and every version, because a row's history is its versions.\n" +
				"20260908010000_cluster_status_retired.up.sql is the worked example.\n\n" +
				"A waiver is for a retirement that genuinely needs no migration: nothing ever wrote the\n" +
				"field, or the concept itself is gone. Write the reason; a no-op migration in the\n" +
				"migrations directory would be a lie told to whoever reads it next.\n")
	}

	if len(r.Ledger) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "The retirement ledger does not hold up (%d problem(s)):\n\n", len(r.Ledger))
		problems := append([]string(nil), r.Ledger...)
		sort.Strings(problems)
		for _, p := range problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func header(n int) string {
	if n == 1 {
		return "This branch takes something away from a concept that the snapshot still records, and the ledger does not say what happened to the rows:"
	}
	return fmt.Sprintf(
		"This branch takes %d things away from concepts that the snapshot still records, and the ledger does not say what happened to the rows:", n)
}

// ledgerTemplate renders the JSON line to paste, filled in as far as the gate
// can fill it. The two things it cannot know -- which migration, or why none is
// needed -- are left as placeholders rather than guessed.
func ledgerTemplate(n Narrowing) string {
	fields := []string{fmt.Sprintf("%q: %q", "concept", n.Concept)}
	if n.Field != "" {
		fields = append(fields, fmt.Sprintf("%q: %q", "field", n.Field))
	}
	fields = append(fields, fmt.Sprintf("%q: %q", "kind", n.Kind))
	if n.Value != "" {
		fields = append(fields, fmt.Sprintf("%q: %q", "value", n.Value))
	}
	switch n.Kind {
	case KindConcept:
		fields = append(fields, `"waiver": "<why the rows need nothing -- e.g. the concept was renamed and nothing writes the old id>"`)
	default:
		fields = append(fields, `"migration": "<NNNNNNNNNNNNNN_name.up.sql>"`)
	}
	fields = append(fields, `"note": "<the issue that took it away>"`)
	return "{ " + strings.Join(fields, ", ") + " }"
}

// scanFrom scans the named core root PLUS every pack tree beside it.
//
// The parameter stays a single root so every caller and every flag reads as
// it did; the packs are added here, at the one place that decides what the
// committed snapshot covers. A caller naming a non-default root -- a test
// with a fixture tree -- gets exactly that root and no packs, which is what
// a fixture means.
func scanFrom(dslRoot string) (Snapshot, error) {
	if dslRoot != DefaultDSLRoot {
		return Scan(dslRoot)
	}
	return ScanRoots(DefaultRoots()...)
}
