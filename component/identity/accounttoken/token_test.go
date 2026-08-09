package accounttoken

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The three properties the mint has to have, and one it must NOT.

func TestMintProducesAPrefixedHighEntropyTokenAndItsDigest(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("minted token %q does not carry %q. The prefix is how a "+
			"credential's family is readable off the string -- an operator "+
			"holding a pasted secret has nothing else to go on.", plain, TokenPrefix)
	}
	// 32 random bytes, base64url no padding -> 43 characters.
	if body := strings.TrimPrefix(plain, TokenPrefix); len(body) != 43 {
		t.Errorf("token body is %d chars, want 43 (32 bytes base64url-no-pad). "+
			"A shorter body is less entropy than pat / workertoken carry.", len(body))
	}
	if hash != Hash(plain) {
		t.Errorf("Mint returned a digest that is not Hash(plain): %q vs %q", hash, Hash(plain))
	}
	if strings.Contains(hash, plain) || strings.Contains(plain, hash) {
		t.Errorf("the digest and the plaintext overlap, which would make the " +
			"stored value a partial disclosure of the secret")
	}
}

func TestMintIsNotDeterministic(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[plain] {
			t.Fatalf("Mint returned a duplicate token after %d draws", i)
		}
		seen[plain] = true
	}
}

// Hash("") must not produce the digest of the empty string. A caller
// that hashes a missing token would otherwise get a value that could
// match a stored row -- the empty-credential-matches bug class.
func TestHashOfEmptyIsEmpty(t *testing.T) {
	if got := Hash(""); got != "" {
		t.Errorf("Hash(\"\") = %q, want \"\"", got)
	}
}

func TestIsAccountTokenRecognisesOnlyItsOwnFamily(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  bool
	}{
		{"mql_acct_abc", true},
		{"  mql_acct_abc  ", true},
		{"mql_pat_abc", false},
		{"mql_wkr_abc", false},
		{"mql_acc_abc", false},
		{"", false},
		{"eyJhbGciOiJSUzI1NiJ9.e30.x", false},
	} {
		if got := IsAccountToken(tc.token); got != tc.want {
			t.Errorf("IsAccountToken(%q) = %v, want %v", tc.token, got, tc.want)
		}
	}
}

// THE NEGATIVE CLAIM, asserted rather than asserted-in-a-comment.
//
// An account token authorizes nothing today (see the package comment:
// it cannot be the account, because nothing authenticates as one, and
// it must not be the operator, because the binding cannot narrow the
// operator's authority while the actor envelope has no tenancy
// dimension). "Authorizes nothing" is only true while no resolver
// exists, and the way a resolver arrives is by someone adding one here
// without reading the reasoning -- so the absence is pinned.
//
// This test reads THIS PACKAGE'S OWN SOURCE and fails the moment it
// grows a Resolve / Verify / LookupByKeyHash surface. Source rather
// than reflection, because a package cannot enumerate its own exported
// set at runtime -- and a scan is what makes the guard fire on the
// change that would break the claim, rather than on a list someone
// forgot to update.
func TestThisPackageDeclaresNoTokenResolutionFunction(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package source: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("the scan found no package source, so it measured nothing")
	}

	// Names that would turn a presented bearer into an identity. Hash /
	// Mint are absent on purpose: minting and digesting are custody, and
	// custody is the whole of what this package does.
	banned := regexp.MustCompile(`^(Resolve|Verify|Authenticate|Admit)|LookupByKeyHash$|ByKeyHash$`)

	scanned := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			scanned++
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil {
					continue
				}
				if banned.MatchString(fn.Name.Name) {
					t.Errorf("%s declares %s. That resolves a presented account token "+
						"into an identity, which makes mql_acct_ a LIVE bearer credential "+
						"-- and this package's comment, "+
						"docs/public/operate/auth/account-tokens.md and "+
						"dsl/identity/concepts.memql all state it is not one. "+
						"docs/internal/design/account-isolation-model.md section 5.2 is why: "+
						"until the resolved actor carries a tenancy dimension, the account "+
						"binding cannot narrow what the bearer may do, so admitting the "+
						"credential means handing out the operator's whole authority under "+
						"a customer's name. Update all four, or do not add it.",
						filepath.Base(path), fn.Name.Name)
				}
			}
		}
	}
	if scanned < 2 {
		t.Errorf("scanned only %d source files; this package has more than that, so "+
			"the guard is reading the wrong directory", scanned)
	}
}

// The DSL half of the same claim: no query resolves an account_token
// row by its stored digest.
//
// The Go scan above cannot see this. Every other credential family in
// the tree has a by-keyHash query precisely because an interceptor
// resolves a presented bearer through it, so adding one here is the
// single change that would turn an inert credential into a live one --
// and it would live in a .memql file, where no Go compiler is looking.
func TestNoDslQueryLooksAnAccountTokenUpByItsDigest(t *testing.T) {
	const rel = "../../../dsl/identity/queries.memql"
	data, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	source := string(data)
	if !strings.Contains(source, "accountTokensForAccount") {
		t.Fatalf("%s declares no account-token query at all, so this guard is "+
			"reading the wrong file or the queries were renamed", rel)
	}

	// A by-digest lookup over this family would have to name both the
	// discriminator and the digest field. Either alone is legitimate
	// (workerTokenByKeyHash names the digest; the account-token reads
	// name the discriminator); together, in one construct, is the shape
	// that does not exist.
	for _, chunk := range strings.Split(source, "\nquery ")[1:] {
		// Bound the chunk at the construct's closing brace. Without this
		// the LAST chunk runs to end-of-file and would sweep in every
		// later query's text -- including workerTokenByKeyHash, which
		// legitimately names credentials.keyHash. A guard whose failure
		// depends on file ordering is a guard that will one day report a
		// hole that is not there, and be deleted for it.
		block := chunk
		if end := strings.Index(chunk, "\n}"); end >= 0 {
			block = chunk[:end]
		}
		if !strings.Contains(block, `identityType=="account_token"`) {
			continue
		}
		if strings.Contains(block, "credentials.keyHash") {
			t.Errorf("query %s resolves an account_token row by credentials.keyHash. "+
				"That is the hot-path shape an interceptor needs, and no interceptor "+
				"should have one: see TestThisPackageDeclaresNoTokenResolutionFunction "+
				"for the reasoning and the four places that would have to change with it.",
				strings.Fields(block)[0])
		}
	}
}
