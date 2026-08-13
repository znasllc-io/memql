package parser

import (
	"reflect"
	"strings"
	"testing"
)

// A colon-named argument whose value is a bare WORD literal (`b:true`)
// silently produced a garbage argument instead of binding the value.
//
// The lexer glues ':' into an identifier so canonical ids scan as one
// token (`concept==v1:cluster:node`, 218 runtime callsites). A value that
// begins with a LETTER is indistinguishable from the next segment of such
// an id, so `backupEligible:true` scanned as the single identifier
// "backupEligible:true"; the parser's punning branch then read that whole
// text as the argument NAME, and the declared argument never existed.
//
// Nothing errored. The mutation rendered a payload with the field simply
// absent, and the caller had no signal at all. That is how a registered
// WebAuthn passkey came to store no backupEligible flag: at login the row
// read false, the authenticator asserted true, and go-webauthn refused the
// assertion with "Backup Eligible flag inconsistency detected during login
// validation" -- passkey sign-in was broken for every synced credential.
//
// #1118 already fixed the DIGIT half of exactly this defect (`:` followed
// by a digit is a separator, so `{tokenBudget:300000}` binds). Word-shaped
// literals were left glued. These tests close the remaining half.
//
// The oracle throughout is the SPACED spelling. `b: true` has always
// bound correctly, so rather than assert a hand-written expectation these
// compare the two spellings and require them to agree -- the language
// defines the answer, not the test.

// argsOf parses a construct call and returns its argument map.
func argsOf(t *testing.T, src string) map[string]any {
	t.Helper()
	expr, err := ParseExpression(src)
	if err != nil {
		t.Fatalf("ParseExpression(%q): %v", src, err)
	}
	fc, ok := expr.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("ParseExpression(%q) = %T, want *FunctionCallExpr", src, expr)
	}
	return fc.Args
}

// TestColonNamedArgBindsWordLiteral is the regression proper: the tight
// spelling must bind exactly what the spaced spelling binds.
func TestColonNamedArgBindsWordLiteral(t *testing.T) {
	for _, literal := range []string{"true", "false", "null", "nil"} {
		t.Run(literal, func(t *testing.T) {
			tight := argsOf(t, `mutation m(flag:`+literal+`)`)
			spaced := argsOf(t, `mutation m(flag: `+literal+`)`)

			if !reflect.DeepEqual(tight, spaced) {
				t.Fatalf("`flag:%s` bound %#v; `flag: %s` bound %#v. The two spellings "+
					"must be the same argument -- whitespace is not semantic.",
					literal, tight, literal, spaced)
			}
			if _, ok := tight["flag"]; !ok {
				t.Fatalf("no argument named \"flag\" in %#v; the declared argument must exist "+
					"or the mutation renders a payload with the field silently absent", tight)
			}
		})
	}
}

// TestColonNamedArgBindsWordLiteralAmongSiblings pins the shape the
// WebAuthn store actually emits: word literals interleaved with the
// string and number arguments that always worked.
func TestColonNamedArgBindsWordLiteralAmongSiblings(t *testing.T) {
	tight := argsOf(t, `mutation createPasskeyIdentity(credentialId:"c1",signCount:0,backupEligible:true,backupState:false,registeredBy:"u1")`)
	spaced := argsOf(t, `mutation createPasskeyIdentity(credentialId: "c1", signCount: 0, backupEligible: true, backupState: false, registeredBy: "u1")`)

	if !reflect.DeepEqual(tight, spaced) {
		t.Fatalf("tight spelling bound %#v, spaced bound %#v", tight, spaced)
	}
	if got, want := tight["backupEligible"], true; got != want {
		t.Errorf("backupEligible = %#v, want %v", got, want)
	}
	if got, want := tight["backupState"], false; got != want {
		t.Errorf("backupState = %#v, want %v", got, want)
	}
}

// TestColonValueLiteralInObjectLiteral covers the same defect in the
// other place a colon separates a key from a value. #1118 fixed the
// numeric case here; the word case failed identically.
func TestColonValueLiteralInObjectLiteral(t *testing.T) {
	tight := argsOf(t, `mutation m(payload:{enabled:true,count:3})`)
	spaced := argsOf(t, `mutation m(payload: {enabled: true, count: 3})`)

	if !reflect.DeepEqual(tight, spaced) {
		t.Fatalf("`{enabled:true}` bound %#v, `{enabled: true}` bound %#v", tight, spaced)
	}
}

// TestCanonicalIdStillScansAsOneIdentifier is the guard on the fix. The
// colon-gluing rule exists for these and must survive: `concept==v1:...`
// is the single most common runtime query shape in the tree.
func TestCanonicalIdStillScansAsOneIdentifier(t *testing.T) {
	for _, id := range []string{
		"v1:cluster:node",
		"v1:identity:user",
		"v1:memql:backend:user",
		// A segment that merely STARTS with a reserved word is not one.
		"v1:crm:trueish",
		// A trailing reserved word is still an id segment once the run has
		// already glued: only the FIRST colon of a run can be a separator.
		"v1:foo:true",
	} {
		t.Run(id, func(t *testing.T) {
			toks, err := NewLexer("concept==" + id).Tokenize()
			if err != nil {
				t.Fatalf("Tokenize: %v", err)
			}
			var idents []string
			for _, tk := range toks {
				if tk.Type == TokenIdentifier {
					idents = append(idents, tk.Literal)
				}
			}
			want := []string{"concept", id}
			if !reflect.DeepEqual(idents, want) {
				t.Fatalf("identifiers = %q, want %q -- a canonical id must scan as ONE token",
					idents, want)
			}
		})
	}
}

// TestConstructCallArgNameCannotContainColon closes what the lexer fix
// cannot reach. `b:someIdent` is not a literal, so it still scans glued,
// and the punning branch would still accept the whole text as a name.
// An argument name can never contain a colon, so this is unambiguously a
// mistake -- and it is the exact shape that shipped a broken passkey row
// with no error. Fail loudly instead.
func TestConstructCallArgNameCannotContainColon(t *testing.T) {
	for _, src := range []string{
		`mutation m(b:someIdent)`,
		`mutation m(a:"s",b:someIdent)`,
		`mutation m(b:v1:crm:lead)`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			if err == nil {
				t.Fatalf("ParseExpression(%q) succeeded; an argument name containing ':' "+
					"must be rejected rather than silently bound as a punned name", src)
			}
			if !strings.Contains(err.Error(), "ONE identifier") {
				t.Errorf("error %q should explain that the token was scanned as one identifier, "+
					"which is the part an author cannot see for themselves", err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// the two places the FIRST guard did not reach
// -----------------------------------------------------------------------------
//
// The lexer fix above is universal -- it splits `k:true` wherever it appears.
// The parser guard was not: it sat only in the punning branch, which is reached
// only by a KIND-PREFIXED call. Two other positions take a glued token and bind
// it, and both were still silent afterwards:
//
//   - a BARE call, where the glued token falls through to the positional
//     branch and becomes args["0"];
//   - an OBJECT LITERAL key.
//
// Shipping a guard narrower than the defect is the exact shape of memql#1118,
// which fixed the digit half of this and left the word half live for months.
// Doing it again in the fix FOR #1118's omission would be a poor joke.

// TestBareCallArgNameCannotContainColon: the bare-call half.
//
// `f(a:"s",b:someIdent)` is the dangerous spelling -- `a` binds correctly and
// only `b` is lost, so the call looks like it works.
func TestBareCallArgNameCannotContainColon(t *testing.T) {
	for _, src := range []string{
		`f(a:someIdent)`,
		`f(a:"s",b:someIdent)`,
		`script(script:"install.verifyProviderKey",key-file:args.keyFile)`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			if err == nil {
				t.Fatalf("ParseExpression(%q) succeeded; the glued token binds as a POSITIONAL "+
					"argument and the declared one vanishes with no error", src)
			}
			if !strings.Contains(err.Error(), "ONE identifier") {
				t.Errorf("error %q should explain that the token was scanned as one identifier", err)
			}
		})
	}
}

// TestObjectLiteralKeyCannotContainColon: the object-literal half, and the
// most deceptive member of the family.
//
// `{userId:args.userId}` glues into a token CONTAINING A DOT, so the dotted-path
// shorthand splits it on the dot and yields the expected-looking key `userId`
// whose value is the raw text "userId:args.userId". A reader of the map sees
// the right key and a plausible string, which is why this one survives review.
func TestObjectLiteralKeyCannotContainColon(t *testing.T) {
	for _, src := range []string{
		`mutation m(payload:{userId:args.userId})`,
		`mutation m(payload:{a:b})`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := ParseExpression(src)
			if err == nil {
				t.Fatalf("ParseExpression(%q) succeeded; an object key cannot contain ':' and "+
					"the value bound here is the glued text, not the reference that was written", src)
			}
		})
	}
}

// TestWordLiteralsBindInEveryCallShape is the counterpart guard: the LEXER fix
// must reach all three positions, so none of these may be refused.
func TestWordLiteralsBindInEveryCallShape(t *testing.T) {
	cases := map[string]string{
		"bare call":          `f(a:true)`,
		"kind-prefixed call": `mutation m(a:true)`,
		"object literal":     `mutation m(payload:{enabled:true})`,
	}
	for shape, src := range cases {
		t.Run(shape, func(t *testing.T) {
			if _, err := ParseExpression(src); err != nil {
				t.Fatalf("ParseExpression(%q): %v -- a word literal is split by the lexer and "+
					"must bind in every position, not just the guarded one", src, err)
			}
		})
	}
}

// TestEqualsNamedArgNameCannotContainColon: the fourth and last position.
//
// `m(b:c = 1)` binds an argument named "b:c"; `b` never exists. The `=` form
// is the least-used of the four, which is exactly why it was the one still
// standing after two passes at this defect.
func TestEqualsNamedArgNameCannotContainColon(t *testing.T) {
	for _, src := range []string{
		`mutation m(b:c = 1)`,
		`mutation m(a: 1, b:c = 2)`,
		`f(b:c = 1)`,
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := ParseExpression(src); err == nil {
				t.Fatalf("ParseExpression(%q) succeeded; the argument the author named is silently absent", src)
			}
		})
	}
}

// TestEveryArgumentPositionRefusesAColonName is the completeness assertion.
//
// Four positions bind an argument name. Two passes at this defect each fixed
// a subset, so what is pinned here is the SET, not another instance: if a
// fifth position is added, this test is where the omission should show up.
func TestEveryArgumentPositionRefusesAColonName(t *testing.T) {
	positions := map[string]string{
		"colon-named, kind-prefixed": `mutation m(b:someIdent)`,
		"equals-named":               `mutation m(b:c = 1)`,
		"punned/positional, bare":    `f(b:someIdent)`,
		"object-literal key":         `mutation m(p:{b:someIdent})`,
	}
	for position, src := range positions {
		t.Run(position, func(t *testing.T) {
			if _, err := ParseExpression(src); err == nil {
				t.Fatalf("%s still binds a colon-bearing argument name: %q", position, src)
			}
		})
	}
}
