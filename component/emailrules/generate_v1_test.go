package emailrules

// generate_v1_test.go -- the rule condition as an edition-2026 predicate over
// `row` (epic memql#5363, memql#5368): both stored spellings generate one
// automation, the generated construct parses and compiles as a v1 automation
// and loads today, and every refusal speaks to the person who typed it.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func ruleWith(condition string) Rule {
	r := baseRule()
	r.Condition = condition
	return r
}

func generate(t *testing.T, condition string) string {
	t.Helper()
	src, err := GenerateAutomation(ruleWith(condition))
	if err != nil {
		t.Fatalf("GenerateAutomation(%q): %v", condition, err)
	}
	return src
}

// TestLegacyAndV1ConditionsGenerateTheSameAutomation: a stored `payload.`
// condition and its `row.` spelling generate one automation, byte for byte,
// its filter the canonical v1 condition as a lambda's body.
func TestLegacyAndV1ConditionsGenerateTheSameAutomation(t *testing.T) {
	for _, c := range []struct{ legacy, v1, filter string }{
		{`payload.role == "admin"`, `row.role == "admin"`, `row => row.role == "admin"`},
		{`payload.role == "admin" && payload.active == true`, `row.role=="admin"&&row.active==true`, `row => row.role == "admin" && row.active == true`},
		{`payload.tier in ["a", "b"]`, `row.tier in ["a","b"]`, `row => row.tier in ["a", "b"]`},
		{`payload.deletedAt == null`, `row.deletedAt == nil`, `row => row.deletedAt == nil`},
		{`payload.plan != "free" || (payload.seats > 10)`, `row.plan != "free" || row.seats > 10`, `row => row.plan != "free" || row.seats > 10`},
	} {
		legacy, v1 := generate(t, c.legacy), generate(t, c.v1)
		if legacy != v1 {
			t.Errorf("%q and %q generate different automations:\n%s\n---\n%s", c.legacy, c.v1, legacy, v1)
		}
		// memqlmigrate:keep -- builds the expected v1 filter; not a fixture.
		if want := "@filter(" + c.filter + ")\n"; !strings.Contains(v1, want) {
			t.Errorf("%q generated no %s:\n%s", c.v1, strings.TrimSpace(want), v1)
		}
	}
}

// TestGeneratedFilterIsWhatTheCodemodWrites: a legacy condition's automation
// is what `memqlmigrate --rewrite=expressions` makes of the construct the
// generator used to write, so a stored rule and a migrated hand-written
// automation cannot differ.
func TestGeneratedFilterIsWhatTheCodemodWrites(t *testing.T) {
	const legacy = `payload.role == "admin" && payload.active == true`
	src := generate(t, legacy)
	// memqlmigrate:keep -- rebuilds the LEGACY construct on purpose.
	old := strings.Replace(src, `@filter(row => row.role == "admin" && row.active == true)`, "@filter("+legacy+")", 1)
	if old == src {
		t.Fatalf("the generated filter is not the expected v1 lambda:\n%s", src)
	}
	migrated, err := langparser.RewriteExpressions([]byte(old), nil)
	if err != nil {
		t.Fatalf("the codemod refused the legacy construct: %v", err)
	}
	if string(migrated) != src {
		t.Fatalf("the codemod's output differs from the generator's:\n%s\n---\n%s", migrated, src)
	}
}

// TestV1ConditionsAccepted: the condition is a full v1 predicate over the
// record -- membership, prefixes, methods with lambdas, the clock, catalog
// functions, the event envelope and the declared id.
func TestV1ConditionsAccepted(t *testing.T) {
	for cond, filter := range map[string]string{
		`row.status in ["active", "trial"] && row.plan != "free"`: `row.status in ["active", "trial"] && row.plan != "free"`,
		`row.name startsWith "Acme"`:                              `row.name startsWith "Acme"`,
		`row.tags.any(t => t == "vip")`:                           `row.tags.any(t => t == "vip")`,
		`row.expiresAt < addDuration(now, "P7D")`:                 `row.expiresAt < addDuration(now, "P7D")`,
		`lower(row.name) == "ada"`:                                `lower(row.name) == "ada"`,
		`event.kind == "node.created" && row.score >= 3`:          `event.kind == "node.created" && row.score >= 3`,
		`id != "" && row.id == id`:                                `id != "" && row.id == id`,
		`args.id == row.id`:                                       `args.id == row.id`,
		`row.region == "eu" ? row.vat != nil : true`:              `row.region == "eu" ? row.vat != nil : true`,
	} {
		if src := generate(t, cond); !strings.Contains(src, "@filter(row => "+filter+")\n") {
			t.Errorf("%q generated no filter %q:\n%s", cond, filter, src)
		}
	}
}

// TestConditionRefusals: each refusal, and the words it is refused with. The
// message is shown to the person who typed the condition, so each names the
// problem in plain words and the fix in one sentence.
func TestConditionRefusals(t *testing.T) {
	for _, c := range []struct{ cond, want string }{
		{`row.role == "admin"; drop`, "has to fit on one line"},
		{"row.role == \"admin\"\n@disabled", "has to fit on one line"},
		{`row.role == "admin" } automation evil {`, "can't contain braces, @ or semicolons"},
		{`row.role ==`, "The condition isn't valid:"},
		{`1 == 1`, "has to test a field of the record that changed"},
		{`event.kind == "node.created"`, "has to test a field of the record that changed"},
		{`row.role == admin`, `if admin is a value, put it in quotes: "admin"`},
		{`actor.role == "admin" && row.active == true`, "nobody is signed in when a rule fires"},
		{`args.email == "x" && row.active == true`, "not args.email"},
		{`query everyone().count() > 0 && row.active == true`, "can't run a query, mutation or other construct"},
		{`isAdmin(row) && row.active == true`, "isn't a function a condition can call"},
		{`payload.role == admin`, `Put the text admin in quotes, like "admin"`},
	} {
		_, err := GenerateAutomation(ruleWith(c.cond))
		var ce *ConditionError
		if !errors.As(err, &ce) {
			t.Errorf("%q: want a ConditionError, got %v", c.cond, err)
			continue
		}
		if !strings.Contains(ce.Message, c.want) {
			t.Errorf("%q: refused with %q, want it to say %q", c.cond, ce.Message, c.want)
		}
		if strings.Contains(ce.Message, "emailrules:") {
			t.Errorf("%q: the message carries the package name, which means nothing to the person reading it: %q", c.cond, ce.Message)
		}
		// Validate gives the same answer, so the form is refused before
		// anything is written.
		if verr := ruleWith(c.cond).Validate(); verr == nil || verr.Error() != ce.Message {
			t.Errorf("%q: Validate says %v, GenerateAutomation says %q", c.cond, verr, ce.Message)
		}
	}
}

// TestGeneratedAutomationIsV1: the construct parses with the edition-2026
// grammar on and compiles as a v1 automation -- the trigger filter a lambda,
// the step's arguments expression leaves the run's scope resolves -- which the
// automations runtime prepares without refusal.
func TestGeneratedAutomationIsV1(t *testing.T) {
	src := generate(t, `payload.role == "admin" && payload.active == true`)
	normalised, err := langparser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	file, err := langparser.ParseFileWithOptions(normalised, langparser.Options{ExpressionsV1: true})
	if err != nil {
		t.Fatalf("the generated construct does not parse with ExpressionsV1 on: %v\n%s", err, src)
	}
	var def *langparser.FunctionDef
	for _, d := range file.Definitions {
		if fn, ok := d.(*langparser.FunctionDef); ok {
			def = fn
		}
	}
	body, ok := def.Body.(*langparser.AutomationDef)
	if !ok || !body.ExpressionsV1 || body.Trigger == nil || body.Trigger.FilterLambda == nil {
		t.Fatalf("parsed %T %+v: want a v1 automation with a lambda trigger filter", def.Body, def.Body)
	}

	res, err := compiler.NewDefault().CompileFile(file)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	compiled := res.Automations[0].JSON
	if compiled["expressions"] != "v1" {
		t.Fatalf(`compiled "expressions" = %#v, want "v1"`, compiled["expressions"])
	}
	b, err := json.Marshal(compiled)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"filter":"row =\u003e row.role == \"admin\" \u0026\u0026 row.active == true"`,
		`"nodeId":{"$expr":"id"}`,
		`"event":{"$expr":"event"}`,
		`"emailRuleId":"v1:campaigns:emailRule:ab12cd34"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("compiled JSON lacks %s:\n%s", want, b)
		}
	}

	var a automations.Automation
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := automations.PrepareExpressions(&a); err != nil {
		t.Fatalf("the runtime refused the compiled construct: %v", err)
	}
	if !a.IsV1() || a.Trigger.FilterLambda == nil {
		t.Fatal("the runtime did not load the construct as v1 with its lambda filter")
	}
}

// TestGeneratedAutomationLoadsBeforeTheFlip: today the authoring pipeline
// parses with the legacy grammar, which accepts the lambda filter (a pushdown
// position). The construct loads as a legacy automation whose filter is still
// evaluated as the lambda it is.
func TestGeneratedAutomationLoadsBeforeTheFlip(t *testing.T) {
	src := generate(t, `row.role == "admin"`)
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "authored:emailrules")
	if err != nil {
		t.Fatalf("the legacy load path refused the construct: %v\n%s", err, src)
	}
	if a.IsV1() || a.Trigger == nil || a.Trigger.FilterLambda == nil {
		t.Fatalf("v1=%v trigger=%+v: want a legacy automation whose lambda filter was parsed", a.IsV1(), a.Trigger)
	}
}
