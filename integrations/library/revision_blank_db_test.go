package library

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// The attachment supplies evidence, never a source range in the destination.
// Reproduce a model confusing the two, then ensure the bounded repair reaches
// approval on another replica with the blank-source protocol still in force.
func testBlankDestinationWithReference(t *testing.T, shared *revisionDB) {
	for _, tc := range []struct{ name, source string }{
		{"empty", ""}, {"newline", "\n"}, {"whitespace", " \t\r\n"}, {"existing", "# Existing\n\nKeep this.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shared.isolatedCase(t)
			referenceText := "# Reference\n\nSynthetic orchard evidence.\n"
			referenceID, reference := f.document(referenceText)
			artifact, doc := f.document(tc.source)
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Write the synthetic orchard note using the reference.", reviewAttachment{ArtifactID: referenceID, Version: reference.version, Revision: reference.revision})
			request := asString(args["requestId"])
			blank := strings.TrimSpace(tc.source) == ""
			before, after := "", "# Orchard\n\nA synthetic orchard note.\n"
			if !blank {
				before, after = "Keep this.", "Keep this.\n\nA synthetic orchard note."
			}
			f.ai.expectedReference = "Synthetic orchard evidence."
			f.ai.firstAnswer = &revisionAnswer{Summary: "Use reference", Edits: []revisionReplacement{{Before: referenceText, After: after, Reason: "Requested note", CommentIDs: []string{note}}}}
			f.ai.answer = revisionAnswer{Summary: "Create the requested note", Edits: []revisionReplacement{{Before: before, After: after, Reason: "Requested note", CommentIDs: []string{note}}}}
			var wait *workstate.HumanWait
			if err := f.execute(f.other, request, false); !errors.As(err, &wait) {
				t.Fatalf("repair did not reach human review: %v", err)
			}
			if f.ai.calls.Load() != 2 || f.ai.repairCalls.Load() != 1 || len(f.ai.editSchemas) != 2 {
				t.Fatalf("reference edit did not receive exactly one repair: calls=%d, repairs=%d", f.ai.calls.Load(), f.ai.repairCalls.Load())
			}
			for n, rawSchema := range f.ai.editSchemas {
				if f.ai.editBlankDestinations[n] != blank {
					t.Fatalf("model destination state disagrees with saved bytes: %v", f.ai.editBlankDestinations)
				}
				schemaJSON, _ := json.Marshal(rawSchema)
				schema, err := jsonschema.CompileString("revision.json", string(schemaJSON))
				if err != nil {
					t.Fatal(err)
				}
				for _, check := range []struct {
					name   string
					answer revisionAnswer
					valid  bool
				}{
					{"insertion or exact edit", f.ai.answer, true},
					{"genuine no-op remains representable", revisionAnswer{Summary: "Already satisfied", Edits: []revisionReplacement{}}, true},
					{"reference cannot be the blank source", *f.ai.firstAnswer, !blank},
					{"multiple insertions refused only for blank source", revisionAnswer{Summary: "Two edits", Edits: []revisionReplacement{f.ai.answer.Edits[0], f.ai.answer.Edits[0]}}, !blank},
				} {
					body, _ := json.Marshal(check.answer)
					var value any
					_ = json.Unmarshal(body, &value)
					if err := schema.Validate(value); (err == nil) != check.valid {
						t.Fatalf("attempt %d: %s: %v", n, check.name, err)
					}
				}
			}
			_, _, _, approval, err := f.second.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
			if err != nil {
				t.Fatal(err)
			}
			f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": memql.BareShortId(asString(approval["id"])), "decision": "approved", "answer": f.accepted(request)})
			if err := f.execute(f.engine, request, true); err != nil {
				t.Fatal(err)
			}
			changed, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			want := strings.Replace(tc.source, before, after, 1)
			if blank && strings.Contains(tc.source, "\r\n") {
				want = strings.ReplaceAll(after, "\n", "\r\n") + tc.source
			}
			if err != nil || asString(changed.backing["body"]) != want {
				t.Fatalf("approved draft changed unexpected bytes: got %q, want %q (%v)", asString(changed.backing["body"]), want, err)
			}
		})
	}
}
