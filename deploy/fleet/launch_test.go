package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The launch gates (epic memql#3852, task memql#3857).
//
// # What a launch checklist is for, and why this file exists next to one
//
// docs/public/operate/memql-cloud-launch.md is the checklist. Most of its items
// are things a person confirms: DNS is delegated, the Stripe account is out of
// test mode, somebody is on call.
//
// Three of them are not opinions. "No unbounded-spend path exists", "the
// pricing page and the price list agree", and "no public copy calls MemQL a
// database" are claims about the repository that can be checked, and a checklist
// item that can be checked and is not is an item somebody ticks from memory at
// the end of a long day.
//
// So they are checked here. The checklist points at this file, and this file
// points back -- neither is the source of truth alone.
//
// The third is already gated repo-wide by TestNoDatabaseProductClaims
// (database_positioning_test.go), which sweeps every tracked file outside
// docs/internal/, so it is named in the checklist and not duplicated here.

// tierSeed is one row of the price list, parsed from seeds.memql.
type tierSeed struct {
	tier     string
	fields   map[string]string
	numbers  map[string]float64
	rawBlock string
}

var (
	seedDecl  = regexp.MustCompile(`(?m)^seed\s+tierSpec\s+(\w+)\s*\{`)
	seedField = regexp.MustCompile(`(?m)^\s*(\w+):\s*(.+?)\s*$`)
)

// parseTierSeeds reads the price list out of seeds.memql.
//
// The SEEDS are the source of truth for every number in this business -- the
// pricing page, Orbit's upgrade picker and the allowance enforcement all read
// the rows these produce. So the gates below compare everything else against
// this, never the other way round.
func parseTierSeeds(t *testing.T) map[string]tierSeed {
	t.Helper()
	src := fleetFile(t, "seeds.memql")

	out := map[string]tierSeed{}
	locs := seedDecl.FindAllStringSubmatchIndex(src, -1)
	if len(locs) == 0 {
		t.Fatal("parsed no tier seeds -- either seeds.memql moved or this parse stopped matching, and either way every gate in this file is watching nothing")
	}

	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := src[loc[0]:end]

		seed := tierSeed{
			fields:   map[string]string{},
			numbers:  map[string]float64{},
			rawBlock: block,
		}
		for _, m := range seedField.FindAllStringSubmatch(block, -1) {
			key, val := m[1], strings.TrimSuffix(strings.TrimSpace(m[2]), ",")
			unquoted := strings.Trim(val, `"`)
			seed.fields[key] = unquoted
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				seed.numbers[key] = f
			}
		}
		seed.tier = seed.fields["tier"]
		if seed.tier == "" {
			t.Errorf("a tierSpec seed declares no `tier`:\n%s", block)
			continue
		}
		out[seed.tier] = seed
	}
	return out
}

// TestTheTrialCannotMeterOverage is the launch gate, reduced to its one
// load-bearing row.
//
// A trial's card has been authorized and never charged. `overagePolicy: meter`
// on that tier is an unbounded, uncollectable bill available to anyone who can
// complete a checkout form -- which is the single most expensive thing that can
// go wrong with this product, and the first thing somebody would look for.
//
// This is a one-word edit away at all times, it reads as a harmless
// consistency tidy ("why is trial different from every other tier?"), and the
// answer is in this test rather than only in a comment somebody may not open.
func TestTheTrialCannotMeterOverage(t *testing.T) {
	seeds := parseTierSeeds(t)

	trial, ok := seeds["trial"]
	if !ok {
		t.Fatal("there is no trial tier in the price list")
	}
	if got := trial.fields["overagePolicy"]; got != "throttle" {
		t.Errorf("the trial tier's overagePolicy is %q, and it must be \"throttle\". A metered trial is an unbounded bill against a card that has been authorized and never charged.", got)
	}
	if got := trial.numbers["monthlyPriceUsd"]; got != 0 {
		t.Errorf("the trial tier is priced at %g; a trial that charges is not a trial", got)
	}
	if trial.fields["haAvailable"] != "false" {
		t.Error("the trial tier offers HA. A pooled trial instance has nothing to fail over to, so this is a promise the infrastructure cannot keep.")
	}

	// And the reachable positive: every PAID tier meters. Without this the test
	// above would still pass if somebody set every tier to throttle, which
	// would bound spend by refusing service to paying customers.
	var metered int
	for name, seed := range seeds {
		if name == "trial" || seed.numbers["monthlyPriceUsd"] == 0 {
			continue
		}
		if seed.fields["overagePolicy"] != "meter" {
			t.Errorf("paid tier %q has overagePolicy %q; a paid tier that throttles at its allowance refuses service to a customer we could bill", name, seed.fields["overagePolicy"])
		}
		metered++
	}
	if metered == 0 {
		t.Fatal("no paid tier was checked, so this gate compared nothing")
	}
}

// TestEveryTierAllowanceIsBoundedAndPriced.
//
// Two failure shapes, and neither announces itself:
//
//	an allowance of zero on a tier that charges -- a customer who paid $949 and
//	is throttled or billed from their first message;
//
//	an overage rate of zero on a tier that METERS -- consumption past the
//	allowance recorded as free. That one is the quiet version of "no unbounded
//	spend path exists" being false: the meter runs, the ledger balances, and
//	every unit past the line costs us and earns nothing.
func TestEveryTierAllowanceIsBoundedAndPriced(t *testing.T) {
	for name, seed := range parseTierSeeds(t) {
		price := seed.numbers["monthlyPriceUsd"]
		policy := seed.fields["overagePolicy"]

		// Enterprise is quoted rather than listed: a zero price and zero
		// allowances are correct, because the real numbers come from a contract
		// and land on the subscription's meters at period open.
		if seed.fields["status"] == "hidden" {
			continue
		}

		if price > 0 && seed.numbers["messageCredits"] == 0 {
			t.Errorf("tier %q charges $%g and includes 0 message credits; a customer is throttled or billed from their first message", name, price)
		}
		if policy == "meter" && seed.numbers["overageMessagesUsdPer1k"] == 0 {
			t.Errorf("tier %q meters overage at $0 per 1,000 credits. The meter runs, the ledger balances, and every message past the allowance costs us and earns nothing -- which is 'no unbounded spend path' being false in its quiet form.", name)
		}
		// There was a second leak check here, for voice overage on a metering
		// tier. Voice went with the conversational product (epic memql#4988) and
		// no tier includes, meters or prices a voice minute, so the check had no
		// subject left.
		//
		// What it demonstrated is worth carrying to the next metered unit: the
		// rule it enforced had to be guarded on `policy == "meter"`, because a
		// throttling tier has no overage to price and demanding a rate there
		// would be demanding a number for something that cannot happen. The one
		// tier that throttles is the trial, where a non-zero rate would be a bill
		// against a card we never charge. An allowance rule stated one way for
		// one unit and another way for the next is exactly where that bites.
	}
}

// TestThePublishedPriceTableMatchesTheSeeds.
//
// The task's own requirement is "keep prices/config in one place so tier
// changes don't require copy hunts". The one place is seeds.memql, and the
// PRICING PAGE genuinely does read it live -- `publicTiers` is @public, so the
// site fetches the rows and there is no copy to hunt.
//
// The RUNBOOK is the leak. docs/public/operate/memql-cloud.md prints the tier
// table for an operator, because a runbook nobody can read without querying a
// cluster is a runbook nobody reads. That copy is the one that goes stale, and
// it goes stale in the direction where an operator quotes a customer a price we
// do not charge.
func TestThePublishedPriceTableMatchesTheSeeds(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(self)))
	b, err := os.ReadFile(filepath.Join(repo, "docs", "public", "operate", "memql-cloud.md"))
	if err != nil {
		t.Fatalf("read the fleet runbook: %v", err)
	}
	doc := string(b)

	seeds := parseTierSeeds(t)
	var checked int
	for name, seed := range seeds {
		price := seed.numbers["monthlyPriceUsd"]
		if price == 0 {
			continue // trial and enterprise print no figure
		}
		checked++

		// Rendered with thousands separators in the table, e.g. "$2,999/mo".
		want := "$" + withThousands(int64(price)) + "/mo"
		if !strings.Contains(doc, want) {
			t.Errorf("tier %q is priced at %s in seeds.memql, and that string does not appear in the runbook's tier table. An operator reading it would quote a price we do not charge.", name, want)
		}

		// The allowances too -- the numbers a support conversation turns on.
		for _, f := range []struct{ field, label string }{
			{"messageCredits", "message credits"},
		} {
			v := seed.numbers[f.field]
			if v == 0 {
				continue
			}
			if !strings.Contains(doc, withThousands(int64(v))) {
				t.Errorf("tier %q includes %g %s and that figure does not appear in the runbook", name, v, f.label)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no priced tier was compared against the runbook, so this gate checked nothing")
	}
}

// withThousands renders an integer with comma separators, matching how the
// runbook's table prints a price.
func withThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
