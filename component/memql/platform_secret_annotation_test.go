package memql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// platform_secret_annotation_test.go pins memql#3113 / issue #3181: the
// @secret annotation must be carried by the fields in the SHIPPED tree that
// actually hold credential material, not by a fixture.
//
// Why a real-tree test and not another fixture. Before this task the whole
// annotation was live and stamped NOTHING: `grep -rn "@secret" dsl/` matched
// exactly one file, dsl/_reference/_concept.memql, which is `_`-prefixed and
// skipped by the loader (component/memql/dslfs/walker.go, dsl/runtime_mount.go).
// So markSecretArgsFields ran over zero fields and memql#3036's redaction
// branch never fired in production, while every fixture test stayed green.
// A test that reads the fixture cannot see that. These read dsl/.
//
// THE SWEEP, re-run on this branch (issue #3181's first DoD item). Every
// concept-field description in dsl/ was searched for stored-credential
// language (plaintext / cleartext / ciphertext / never rendered / never
// logged / secret / password / api key). The result:
//
// ANNOTATED -- the three fields that hold credential material at top level:
//   - v1:platform:globalSecret.encryptedValue     (NaCl secretbox ciphertext)
//   - v1:platform:partitionSecret.encryptedValue  (same, partition-scoped)
//   - v1:identity:authCode.code                   (CLEARTEXT one-time OAuth
//     code -- the tree's only stored-plaintext credential. Whether it needs
//     to exist at all is issue #3187; this annotation is correct either way.)
//
// DELIBERATELY NOT ANNOTATED, with the reason:
//   - globalSecret.fingerprint / partitionSecret.fingerprint -- last 4 chars,
//     and the schema says "For UI display only; lets operators tell rotated
//     secrets apart". It exists to be shown. Redacting it would remove the
//     only thing the field is for.
//   - the v1:identity:identity credential family (keyHash for api_key,
//     worker_token, node_token, badge; the oauth variant) -- every member is
//     a SHA-256 digest whose schema states "Plaintext never stored", AND they
//     are nested under `credentials`, which SecretFields() structurally
//     cannot see (see TestSecretFieldsIsTopLevelOnly below).
//   - v1:safety:*.argsRedacted, v1:worker:invocation.argsRedacted,
//     v1:router:*.errorMessage -- already-redacted or contractually
//     secret-free payloads, not credential stores.
//
// SCOPE, restated here because this file is where someone adding an
// annotation will land: @secret is NOT blanket protection. It redacts a
// rejected value in the function-args validator, and it matches by ARGUMENT
// NAME rather than write target, so a mutation writing
// `encryptedValue: args.blob` leaves `blob` unredacted. Issues #3182 / #3183 /
// #3184 extend it to the tool-args validator, the automation args binder and
// concept payload validation respectively. Concept.SecretFields' doc comment
// carries the authoritative surface list.

// conceptFromRealTree parses ONE named concept out of a real dsl/ file. The
// legacy ParseConceptMemQL helper returns the file's FIRST concept, which is
// no use for the consolidated per-namespace files this tree uses, so this
// walks the declarations and builds the one asked for.
func conceptFromRealTree(t *testing.T, relPath, declName, conceptID string) *memoryNodes.Concept {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	file, err := parser.ParseFile(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}
	for _, def := range file.Definitions {
		cd, ok := def.(*parser.ConceptDecl)
		if !ok || cd.Name != declName {
			continue
		}
		c, err := memoryNodes.BuildConceptFromDecl(cd, conceptID)
		if err != nil {
			t.Fatalf("build concept %s from %s: %v", declName, relPath, err)
		}
		return c
	}
	t.Fatalf("concept %q not found in %s", declName, relPath)
	return nil
}

func hasField(list []string, name string) bool {
	for _, f := range list {
		if f == name {
			return true
		}
	}
	return false
}

// The shipped tree annotates the credential-bearing fields, and only those.
func TestRealTreeAnnotatesItsSecretFields(t *testing.T) {
	for _, tc := range []struct {
		relPath   string
		declName  string
		conceptID string
		wantS     string   // the field that MUST be @secret
		wantNotS  []string // fields that must deliberately NOT be
	}{
		{
			relPath:   "dsl/platform/concepts.memql",
			declName:  "globalSecret",
			conceptID: "v1:platform:globalSecret",
			wantS:     "encryptedValue",
			wantNotS:  []string{"fingerprint", "name", "kind", "description"},
		},
		{
			relPath:   "dsl/platform/concepts.memql",
			declName:  "partitionSecret",
			conceptID: "v1:platform:partitionSecret",
			wantS:     "encryptedValue",
			wantNotS:  []string{"fingerprint", "name", "kind", "description"},
		},
		{
			relPath:   "dsl/identity/concepts.memql",
			declName:  "authCode",
			conceptID: "v1:identity:authCode",
			wantS:     "code",
			wantNotS:  []string{"codeHash", "clientId", "redirectURI", "state"},
		},
	} {
		t.Run(tc.conceptID, func(t *testing.T) {
			c := conceptFromRealTree(t, tc.relPath, tc.declName, tc.conceptID)
			secret := c.SecretFields()

			if !hasField(secret, tc.wantS) {
				t.Errorf("%s.%s is NOT @secret in %s (SecretFields = %v).\n\n"+
					"That field holds credential material. Without the annotation "+
					"markSecretArgsFields stamps nothing and memql#3036's redaction "+
					"branch never fires for it.",
					tc.conceptID, tc.wantS, tc.relPath, secret)
			}
			for _, f := range tc.wantNotS {
				if hasField(secret, f) {
					t.Errorf("%s.%s is @secret, but it is on the deliberately-not-annotated "+
						"list (see this file's header for the reason). If that decision "+
						"changed, change the record here too.", tc.conceptID, f)
				}
			}
		})
	}
}

// SecretFields() reads TOP-LEVEL properties only. Anyone annotating a nested
// field would otherwise get silence rather than an error, so the limitation is
// pinned rather than merely documented.
//
// v1:identity:identity is the live case: its whole credential family sits
// under the `credentials` object, so even if a nested member were annotated
// SecretFields could not report it.
func TestSecretFieldsIsTopLevelOnly(t *testing.T) {
	nested, err := memoryNodes.ParseConceptMemQL([]byte(`
concept wrapper {
  label       enum("a")!
  credentials object!  @variant(discriminator="label") {
    a {
      apiKey  string!  @secret
    }
  }
}
`), "v1/identity/wrapper")
	if err != nil {
		t.Fatalf("parse nested concept: %v", err)
	}
	if got := nested.SecretFields(); hasField(got, "apiKey") {
		t.Fatalf("SecretFields reported the NESTED apiKey (%v).\n\n"+
			"If nested support was added deliberately, this test and the "+
			"'top-level only' caveat in Concept.SecretFields, "+
			"platform_secret_annotation_test.go and issue #3181 all need "+
			"updating together -- do not let it silently half-work.", got)
	}
}

// End to end through the REAL concept: load a mutation bound to the shipped
// v1:platform:globalSecret, feed it a bad encryptedValue, and confirm the
// value is gone from the message.
//
// Proven to bite: drop `@secret` from encryptedValue in
// dsl/platform/concepts.memql and this fails, because mustNotLeak requires
// BOTH that the value is absent AND that the placeholder is present.
func TestRealTreeSecretRedactionEndToEnd(t *testing.T) {
	const conceptID = "v1:platform:globalSecret"
	real := conceptFromRealTree(t, "dsl/platform/concepts.memql", "globalSecret", conceptID)
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{conceptID: real})

	src := `use platform.concepts.{ globalSecret }
mutate globalSecret storeGlobalSecret {
	args {
		encryptedValue  string  @required  @pattern("^[A-Za-z0-9+/=]+$")
		name            string  @required  @pattern("^[A-Z_]+$")
	}
	insert {
		id: args.name
		encryptedValue: args.encryptedValue
	}
}`

	fn, err := tryParseNewFunctionSyntax("storeGlobalSecret", "mutation", src, "dsl/platform/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}
	if fn.BoundConcept != conceptID {
		t.Fatalf("BoundConcept = %q, want %q -- the stamp resolves through it", fn.BoundConcept, conceptID)
	}
	if !fieldByName(t, fn, "encryptedValue").Secret {
		t.Fatal("args field \"encryptedValue\" was NOT stamped Secret from the real concept")
	}

	v := &functionValidator{}
	mustNotLeak(t, v.validateFunctionArgs(fn, map[string]any{
		"encryptedValue": secretValue,
		"name":           "MY_SECRET",
	}))

	// Control on the same loaded function: a non-secret arg still reports its
	// value, so the redaction is targeted rather than blanket.
	err = v.validateFunctionArgs(fn, map[string]any{
		"encryptedValue": "QUJD",
		"name":           "not-upper",
	})
	if err == nil {
		t.Fatal("expected a validation error for the bad name")
	}
	if !strings.Contains(err.Error(), "not-upper") {
		t.Errorf("a non-secret value must still appear in the message, got:\n  %s", err.Error())
	}
}
