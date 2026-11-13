package accounts

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/accountexport"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/secret"
)

// Use the pack's actual declaration and the registry's schema builder,
// then the Accounts export scrubber. A store-specific export test alone cannot
// protect historical versions carried by a whole-account export.
func TestReviewRequestRetrySecretsDoNotTravelWithAccountHistory(t *testing.T) {
	// Go tests run from this package directory, including GOWORK=off in the
	// full CI checkout. Read the source embedded by reviewspack directly: a
	// Go import would invert the integrations module boundary back to root.
	// Missing source must fail, never turn this privacy regression into a skip.
	source, err := os.ReadFile("../../packs/reviewspack/dsl/concepts.memql")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(string(source))
	if err != nil {
		t.Fatal(err)
	}
	var concept *memorynodes.Concept
	for _, definition := range file.Definitions {
		if declaration, ok := definition.(*parser.ConceptDecl); ok && declaration.Name == "reviewRequest" {
			concept, err = memorynodes.BuildConceptFromDecl(declaration, "v1:reviews:reviewRequest")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if concept == nil || concept.BelongsTo == nil || concept.BelongsTo.Scope != parser.BelongsToAccount {
		t.Fatal("the actual review request must be an account-scoped export kind")
	}
	currentSchema, err := concept.DefinitionSchema()
	if err != nil {
		t.Fatal(err)
	}
	// Older versions may predate the secret annotation. The current definition
	// must protect them too, whether their schema is inline, shared, or absent.
	legacySchema := json.RawMessage(`{"type":"object","properties":{"tokenCiphertext":{"type":"string"}}}`)
	snapshot := &snapshot{
		scrubbed:     map[accountexport.ScrubbedField]bool{},
		schemaDocs:   map[string]json.RawMessage{"review-request-before-secret": legacySchema},
		versionRules: map[string]*secretRules{},
	}
	t.Setenv(secret.EnvMasterKey, strings.Repeat("72", 32))
	for i, version := range []struct {
		name   string
		schema json.RawMessage
	}{
		{"historical without schema", nil},
		{"historical inline schema", legacySchema},
		{"historical shared schema", json.RawMessage(`{"$ref":"memql-schema:review-request-before-secret"}`)},
		{"current schema", currentSchema},
	} {
		t.Run(version.name, func(t *testing.T) {
			plain := `{"purpose":"reviews/request-retry/v1","token":"invented-export-test-bearer-` + version.name + `"}`
			ciphertext, _, err := secret.Encrypt(plain)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{
				"accountId": "export-test-account", "productHandle": "floss", "sendState": "pending",
				"tokenCiphertext": ciphertext,
			})
			if err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(payload)
			row := memorynodes.MemoryNode{
				ID: concept.Name + ":request-test", Concept: concept.Name,
				CreatedAt: time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC),
				Schema:    version.schema, Payload: payload,
			}
			carried, err := snapshot.removeSecrets(concept, row)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(carried, &got); err != nil {
				t.Fatal(err)
			}
			if _, present := got["tokenCiphertext"]; present {
				t.Fatal("account export carried a recoverable review-request retry credential")
			}
			if got["accountId"] != "export-test-account" || got["productHandle"] != "floss" || got["sendState"] != "pending" {
				t.Fatal("scrubbing retry credentials discarded the request's ordinary account data")
			}
			if !snapshot.scrubbed[accountexport.ScrubbedField{Concept: concept.Name, Field: "tokenCiphertext"}] {
				t.Fatal("export report did not identify the omitted retry credential")
			}
			if !bytes.Equal(row.Payload, original) {
				t.Fatal("export changed the stored request instead of its exported copy")
			}
			opened, err := secret.Decrypt(ciphertext)
			if err != nil || opened != plain {
				t.Fatal("export damaged the credential still needed by the request's retry")
			}
		})
	}
}
