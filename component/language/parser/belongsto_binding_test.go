package parser

import (
	"errors"
	"strings"
	"testing"
)

// belongsToAttr parses one annotation line above a stub concept and returns
// the @belongsTo attribute, exactly as the concept translator receives it.
//
// The stub declares `accountId` and `goalId` so the same source also builds
// (component/database/memory-nodes holds the field half); nothing here reads
// the body, because ParseBelongsTo decides only which combinations of KEYS mean
// something.
func belongsToAttr(t *testing.T, line string) *Attribute {
	t.Helper()
	src := line + "\nconcept probe {\n  accountId  string\n  goalId     string\n}\n"
	for _, a := range parseConceptAttrs(t, src) {
		if a.Name == BelongsToAnnotation {
			return a
		}
	}
	t.Fatalf("no @%s attribute parsed from %q", BelongsToAnnotation, line)
	return nil
}

// Every accepted spelling parses to exactly one declaration.
func TestParseBelongsToAcceptsEachSpelling(t *testing.T) {
	cases := []struct {
		line string
		want BelongsToDecl
	}{
		{`@belongsTo(account)`, BelongsToDecl{Scope: BelongsToAccount}},
		{`@belongsTo(account, via="goalId")`, BelongsToDecl{Scope: BelongsToAccount, Via: "goalId"}},
		{`@belongsTo(account, via="storeId", parent="v1:shopify:store")`,
			BelongsToDecl{Scope: BelongsToAccount, Via: "storeId", Parent: "v1:shopify:store"}},
		{`@belongsTo(account, shared)`, BelongsToDecl{Scope: BelongsToAccount, Shared: true}},
		{`@belongsTo(account, stated)`, BelongsToDecl{Scope: BelongsToAccount, Stated: true}},
		{`@belongsTo(account, shared, stated)`, BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true}},
		{`@belongsTo(cluster)`, BelongsToDecl{Scope: BelongsToCluster}},
		// Keys are a set: the order an author writes them in changes nothing.
		{`@belongsTo(shared, account)`, BelongsToDecl{Scope: BelongsToAccount, Shared: true}},
		{`@belongsTo(stated, shared, account)`, BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true}},
		{`@belongsTo(parent="v1:shopify:store", via="storeId", account)`,
			BelongsToDecl{Scope: BelongsToAccount, Via: "storeId", Parent: "v1:shopify:store"}},
		// A value is trimmed, so a padded field name is the field.
		{`@belongsTo(account, via=" goalId ")`, BelongsToDecl{Scope: BelongsToAccount, Via: "goalId"}},
		// `stays` sits beside account in any of its forms.
		{`@belongsTo(account, stays="signin")`, BelongsToDecl{Scope: BelongsToAccount, Stays: BelongsToStaysSignin}},
		{`@belongsTo(account, via="siteId", stays="transit")`,
			BelongsToDecl{Scope: BelongsToAccount, Via: "siteId", Stays: BelongsToStaysTransit}},
		{`@belongsTo(account, shared, stated, stays="machine")`,
			BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true, Stays: BelongsToStaysMachine}},
		{`@belongsTo(account, stays=" signin ")`, BelongsToDecl{Scope: BelongsToAccount, Stays: BelongsToStaysSignin}},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			got, err := ParseBelongsTo(belongsToAttr(t, tc.line))
			if err != nil {
				t.Fatalf("ParseBelongsTo(%s): unexpected error: %v", tc.line, err)
			}
			if got == nil {
				t.Fatalf("ParseBelongsTo(%s): nil declaration and nil error", tc.line)
			}
			if *got != tc.want {
				t.Fatalf("ParseBelongsTo(%s) = %+v, want %+v", tc.line, *got, tc.want)
			}
		})
	}
}

// Every combination that means nothing is refused with its own stable code,
// the refusal's text ends with that code in brackets (the convention every
// coded load refusal follows, so a wrapper that prefixes context leaves the
// code findable), and the text says what to write instead.
func TestParseBelongsToRefusesWhatMeansNothing(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantCode string
		wantHint string
	}{
		{"both scopes", `@belongsTo(account, cluster)`, CodeBelongsToConflict, "names both"},
		{"neither scope", `@belongsTo(shared)`, CodeBelongsToConflict, "names neither `account` nor `cluster`"},
		{"only stated", `@belongsTo(stated)`, CodeBelongsToConflict, "names neither `account` nor `cluster`"},
		{"only via", `@belongsTo(via="goalId")`, CodeBelongsToConflict, "names neither `account` nor `cluster`"},
		{"shared beside cluster", `@belongsTo(cluster, shared)`, CodeBelongsToConflict, "@belongsTo(cluster) takes nothing else"},
		{"stated beside cluster", `@belongsTo(cluster, stated)`, CodeBelongsToConflict, "@belongsTo(cluster) takes nothing else"},
		{"via beside cluster", `@belongsTo(cluster, via="goalId")`, CodeBelongsToConflict, "@belongsTo(cluster) takes nothing else"},
		{"parent beside cluster", `@belongsTo(cluster, parent="v1:x:y")`, CodeBelongsToConflict, "@belongsTo(cluster) takes nothing else"},
		{"parent with no via", `@belongsTo(account, parent="v1:x:y")`, CodeBelongsToConflict, "has no `via`"},
		{"shared beside via", `@belongsTo(account, shared, via="goalId")`, CodeBelongsToConflict, "`shared` is declared on the parent concept only"},
		{"stated beside via", `@belongsTo(account, stated, via="goalId")`, CodeBelongsToConflict, "`stated` leaves the account to the concept's own writers"},
		{"stated beside via and parent", `@belongsTo(account, stated, via="storeId", parent="v1:shopify:store")`, CodeBelongsToConflict, "`stated` leaves the account to the concept's own writers"},
		{"an empty via", `@belongsTo(account, via="")`, CodeBelongsToViaUnknown, "names no field"},
		{"a blank via", `@belongsTo(account, via="  ")`, CodeBelongsToViaUnknown, "names no field"},
		{"an empty parent", `@belongsTo(account, via="storeId", parent="")`, CodeBelongsToParentUnknown, "names no concept"},
		{"stays beside cluster", `@belongsTo(cluster, stays="signin")`, CodeBelongsToConflict, "@belongsTo(cluster) takes nothing else"},
		{"a stays reason outside the three", `@belongsTo(account, stays="forever")`, CodeBelongsToStaysUnknown, "signin, transit or machine"},
		{"an empty stays", `@belongsTo(account, stays="")`, CodeBelongsToStaysUnknown, "signin, transit or machine"},
		{"a stays reason in another case", `@belongsTo(account, stays="Signin")`, CodeBelongsToStaysUnknown, "signin, transit or machine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := ParseBelongsTo(belongsToAttr(t, tc.line))
			if err == nil {
				t.Fatalf("ParseBelongsTo(%s) = %+v, want a refusal", tc.line, decl)
			}
			if decl != nil {
				t.Errorf("ParseBelongsTo(%s) returned a declaration beside its refusal: %+v", tc.line, *decl)
			}
			var refusal *BelongsToError
			if !errors.As(err, &refusal) {
				t.Fatalf("ParseBelongsTo(%s) error is %T, want *BelongsToError: %v", tc.line, err, err)
			}
			if refusal.Code != tc.wantCode || refusal.RuleCode() != tc.wantCode {
				t.Errorf("ParseBelongsTo(%s) code = %q (RuleCode %q), want %q", tc.line, refusal.Code, refusal.RuleCode(), tc.wantCode)
			}
			if !strings.HasSuffix(err.Error(), " ["+tc.wantCode+"]") {
				t.Errorf("ParseBelongsTo(%s) error does not end with its code in brackets: %q", tc.line, err)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("ParseBelongsTo(%s) error = %q, want it to contain %q", tc.line, err, tc.wantHint)
			}
		})
	}
}

// The two shapes the annotation registry refuses before this function is ever
// reached still draw a coded refusal here, because ParseBelongsTo is exported
// and a caller that skipped the registry must not be handed a declaration: a
// bare `@belongsTo` and a positional value both say nothing about what a row
// belongs to.
func TestParseBelongsToRefusesAnAttributeWithNoKeys(t *testing.T) {
	for _, line := range []string{`@belongsTo`, `@belongsTo()`, `@belongsTo("account")`} {
		t.Run(line, func(t *testing.T) {
			decl, err := ParseBelongsTo(belongsToAttr(t, line))
			if err == nil {
				t.Fatalf("ParseBelongsTo(%s) = %+v, want a refusal", line, decl)
			}
			var refusal *BelongsToError
			if !errors.As(err, &refusal) || refusal.Code != CodeBelongsToConflict {
				t.Fatalf("ParseBelongsTo(%s) error = %v, want code %s", line, err, CodeBelongsToConflict)
			}
			if !strings.Contains(err.Error(), "@belongsTo(account)") || !strings.Contains(err.Error(), "@belongsTo(cluster)") {
				t.Errorf("ParseBelongsTo(%s) error does not show the spellings to write: %q", line, err)
			}
		})
	}
}

// A nil attribute and an attribute of another name are programming errors, not
// authoring errors: they carry no rule code, so nothing reports them to an
// author as a refusal of something they wrote.
func TestParseBelongsToRejectsTheWrongAttribute(t *testing.T) {
	if _, err := ParseBelongsTo(nil); err == nil {
		t.Error("ParseBelongsTo(nil): want an error")
	}
	_, err := ParseBelongsTo(&Attribute{Name: RowAuthzAnnotation, Args: map[string]any{"account": true}})
	if err == nil {
		t.Fatal("ParseBelongsTo on a @rowAuthz attribute: want an error")
	}
	var refusal *BelongsToError
	if errors.As(err, &refusal) {
		t.Errorf("a misrouted attribute drew an authoring refusal (%s); it is a caller's defect", refusal.Code)
	}
}

// FormatBelongsTo is the only emitter of a declaration (the codemod that
// declares a tree writes through it) and ParseBelongsTo is the only reader. If
// the two ever disagree, a codemod run writes a tree the loader refuses. Every
// declaration ParseBelongsTo can return is rendered and read back here, and the
// rendering is pinned too: the keys come out in one order -- account, shared,
// stated, via, parent, stays -- so two runs over one tree produce one diff.
func TestBelongsToFormatParseRoundTrip(t *testing.T) {
	cases := []struct {
		decl BelongsToDecl
		want string
	}{
		{BelongsToDecl{Scope: BelongsToCluster}, `@belongsTo(cluster)`},
		{BelongsToDecl{Scope: BelongsToAccount}, `@belongsTo(account)`},
		{BelongsToDecl{Scope: BelongsToAccount, Shared: true}, `@belongsTo(account, shared)`},
		{BelongsToDecl{Scope: BelongsToAccount, Stated: true}, `@belongsTo(account, stated)`},
		{BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true}, `@belongsTo(account, shared, stated)`},
		{BelongsToDecl{Scope: BelongsToAccount, Via: "goalId"}, `@belongsTo(account, via="goalId")`},
		{BelongsToDecl{Scope: BelongsToAccount, Via: "storeId", Parent: "v1:shopify:store"},
			`@belongsTo(account, via="storeId", parent="v1:shopify:store")`},
		{BelongsToDecl{Scope: BelongsToAccount, Stays: BelongsToStaysSignin}, `@belongsTo(account, stays="signin")`},
		{BelongsToDecl{Scope: BelongsToAccount, Via: "siteId", Stays: BelongsToStaysTransit},
			`@belongsTo(account, via="siteId", stays="transit")`},
		{BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true, Stays: BelongsToStaysMachine},
			`@belongsTo(account, shared, stated, stays="machine")`},
		{BelongsToDecl{Scope: BelongsToAccount, Via: "storeId", Parent: "v1:shopify:store", Stays: BelongsToStaysTransit},
			`@belongsTo(account, via="storeId", parent="v1:shopify:store", stays="transit")`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			line := FormatBelongsTo(tc.decl)
			if line != tc.want {
				t.Fatalf("FormatBelongsTo(%+v) = %s, want %s", tc.decl, line, tc.want)
			}
			got, err := ParseBelongsTo(belongsToAttr(t, line))
			if err != nil {
				t.Fatalf("ParseBelongsTo(%s) after FormatBelongsTo(%+v): %v", line, tc.decl, err)
			}
			if *got != tc.decl {
				t.Fatalf("round trip of %+v via %q = %+v", tc.decl, line, *got)
			}
		})
	}
}

// The two questions every reader of a declaration asks, over every declaration
// there is -- including none at all, which is a nil pointer and must answer
// both without a caller having to check first.
func TestBelongsToDeclAnswersScopeAndManagement(t *testing.T) {
	cases := []struct {
		name          string
		decl          *BelongsToDecl
		accountScoped bool
		engineManaged bool
	}{
		{"undeclared", nil, false, false},
		{"cluster", &BelongsToDecl{Scope: BelongsToCluster}, false, false},
		{"account", &BelongsToDecl{Scope: BelongsToAccount}, true, true},
		{"account via a parent", &BelongsToDecl{Scope: BelongsToAccount, Via: "goalId"}, true, true},
		{"account, shared", &BelongsToDecl{Scope: BelongsToAccount, Shared: true}, true, true},
		{"account, stated", &BelongsToDecl{Scope: BelongsToAccount, Stated: true}, true, false},
		{"account, shared, stated", &BelongsToDecl{Scope: BelongsToAccount, Shared: true, Stated: true}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.decl.AccountScoped(); got != tc.accountScoped {
				t.Errorf("AccountScoped() = %v, want %v", got, tc.accountScoped)
			}
			if got := tc.decl.EngineManaged(); got != tc.engineManaged {
				t.Errorf("EngineManaged() = %v, want %v", got, tc.engineManaged)
			}
		})
	}
}

// The reasons a kind stays are a closed list, in the order the file's report
// and the documentation print them.
func TestBelongsToStaysReasonsAreTheThree(t *testing.T) {
	got := BelongsToStaysReasons()
	want := []string{BelongsToStaysSignin, BelongsToStaysTransit, BelongsToStaysMachine}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("BelongsToStaysReasons() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if BelongsToStaysReasons()[0] != BelongsToStaysSignin {
		t.Fatal("BelongsToStaysReasons() hands out its own slice; a caller changed the list")
	}
}
