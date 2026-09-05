package memql

import (
	"strings"
	"testing"
)

// The call an automation step actually makes, end to end (memql#4927).
//
// # What was broken
//
// The `object` argument profile was written against `name({a: 1})` and checked
// `trimmed[0] == '{'`. memql#2335 then RETIRED that wrapper: the rewriter
// lowers every call to `name(a: 1)` and the parser rejects the braces. From
// that release on, an `object`-profile builtin could only be called in a
// spelling the language refuses, and every automation step that passed
// arguments to one was refused at parse:
//
//	function "packageNoteUpstreamFromWebhook" execution failed:
//	invalid argument: packageNoteUpstreamFromWebhook() requires a JSON object argument
//
// with the arguments plainly present in the call. Two shipped automations were
// dead this way for as long as they existed -- the inbound webhook that notes a
// package's upstream moving, and campaign bounce/complaint ingestion. Both
// fail only when the automation FIRES, on somebody else's HTTP request, which
// is why every suite in the repo stayed green.
//
// # Why the strings below are written the way they are
//
// They are what `renderFunctionArgs` (component/automations/steps/function.go)
// emits, not a plausible-looking hand-written call. It stores a step's named
// arguments as a single positional object and renders it by stripping the
// braces off `renderMemQLValue`, which QUOTES its keys -- so the text that
// reaches the parser starts with `"`, not with an identifier. A fix matched
// against the bare-key spelling alone would repair the shape nobody sends, and
// a test written in that spelling would agree with it.
//
// The parenthesised form is asserted through tryParseMetaCommand rather than
// parseMetaCommandArgs so the registry entry, the profile inference and the
// arg contract are the real ones.

func TestAnAutomationStepsRenderedCallReachesItsBuiltin(t *testing.T) {
	e := newParserTestEngine(t)

	cases := []struct {
		what  string
		query string
		want  map[string]string
	}{
		{
			// dsl/platform/automations.memql, automation notePackageUpstreamFromWebhook.
			what: "the inbound webhook that notes an upstream moving",
			query: `packageNoteUpstreamFromWebhook("inboundRequestId": "req-1", ` +
				`"source": "github", "body": "{}")`,
			want: map[string]string{"inboundRequestId": "req-1", "source": "github"},
		},
		{
			// dsl/campaigns/automations.memql, automation ingestCampaignFeedback.
			what:  "campaign bounce and complaint ingestion",
			query: `campaignIngestFeedback("inboundRequestId": "req-2")`,
			want:  map[string]string{"inboundRequestId": "req-2"},
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			expr, matched, err := e.tryParseMetaCommand(c.query)
			if err != nil {
				t.Fatalf("refused: %v\n\nThis is the memql#4927 failure: the step's own "+
					"arguments are in the call and the profile cannot read them.", err)
			}
			if !matched || expr == nil {
				t.Fatalf("the call did not resolve to a builtin at all")
			}
			for k, want := range c.want {
				if got, _ := expr.Args[k].(string); got != want {
					t.Errorf("args[%q] = %q, want %q (all args: %v)", k, got, want, expr.Args)
				}
			}
		})
	}
}

// The bare-key spelling a person writes by hand must work too -- it is what
// every `builtin X (a: 1)` step reads as in the DSL source.
func TestTheHandWrittenBareKeyCallIsAcceptedAsWell(t *testing.T) {
	e := newParserTestEngine(t)
	expr, _, err := e.tryParseMetaCommand(`campaignIngestFeedback(inboundRequestId: "req-3")`)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got, _ := expr.Args["inboundRequestId"].(string); got != "req-3" {
		t.Errorf("args = %v", expr.Args)
	}
}

// The braced form is still accepted, because it is what an HTTP or SDK caller
// sends and what previewInsert and its neighbours are documented as.
func TestTheBracedCallStillWorks(t *testing.T) {
	e := newParserTestEngine(t)
	expr, _, err := e.tryParseMetaCommand(`campaignIngestFeedback({inboundRequestId: "req-4"})`)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got, _ := expr.Args["inboundRequestId"].(string); got != "req-4" {
		t.Errorf("args = %v", expr.Args)
	}
}

// THE CONTROLS. Widening the profile to read a named-args body must not have
// widened it to read anything at all -- each of these is a call that says
// nothing an object could be built from, and each must still be refused with
// the profile's own sentence rather than a decoder error about a body this
// should never have assembled.
func TestWhatTheObjectProfileMustStillRefuse(t *testing.T) {
	e := newParserTestEngine(t)
	for _, q := range []string{
		`campaignIngestFeedback()`,
		`campaignIngestFeedback("just a string")`,
		`campaignIngestFeedback("v1:platform:package:abc")`,
		`campaignIngestFeedback(42)`,
		`campaignIngestFeedback([1, 2])`,
	} {
		_, _, err := e.tryParseMetaCommand(q)
		if err == nil {
			t.Errorf("%s: accepted, want refused", q)
			continue
		}
		if !strings.Contains(err.Error(), "requires a JSON object argument") {
			t.Errorf("%s: refused with %q, want the profile's own sentence -- a decoder "+
				"error here means a body was assembled from something that is not one", q, err)
		}
	}
}
