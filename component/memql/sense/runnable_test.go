package sense

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// runnableFixture is a realistic .memql buffer written to CURRENT authoring
// rules -- file-top `use` imports, signature-bound concepts, bare payload
// fields, `row.` intrinsics, `&&` connectives -- carrying one of every runnable
// construct kind plus the non-runnable kinds that must never appear in the
// result.
const runnableFixture = `// Fixture for the runnable-construct scan.
use cognition.concepts.{ participant, space }
use common.traits.{ isActiveRecord }

/// Active human participants in a space.
@description("Get space participants")
query participant spaceParticipants {
  args {
    /// The space whose participants to read.
    spaceId  string  @required
    limit    int
    kind     string  @enum("human", "ai")
  }
  filter  spaceId==args.spaceId && isActiveRecord
  shape   participantFull
}

@description("Create a cognition space")
mutate space mutationCreateSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert {
    id:        args.spaceId
    name:      args.name
    createdAt: now
    createdBy: actor.userId
  }
}

@enabled
@description("Ensure today's daily space exists.")
logic logicProvisionDailySpace {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser(userId: args.event.payload.id)
  }
}

@enabled
@description("Search for users")
@handler(type="query", query="concept==v1:memql:backend:user")
@executionTime("fast")
tool searchUsers {
  active  boolean  @description("Filter by active status")
  limit   integer  @default("10") @description("Max results to return")
}

/// Auto-creates a session when a participant joins a space.
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation bootstrapSession {
  args {
    id any
  }
  step decide {
    logic bootstrapSession ( event )
  }
}

@trigger(schedule="0 */10 * * * *")
automation sweepStalePlans {
  step decide {
    logic sweepStalePlans ( event )
  }
}

@enabled
@description("Matches guest participants")
spec participant isGuestParticipant {
  return isGuest == true
}

@enabled
@description("Matches records with active==true")
trait isActiveRow {
  return active == true
}

@row
shape space spaceCard {
  row.id
  name
}

@description("A marker concept")
concept marker {
  ownerUserId  string  @required
}

@description("Score an utterance")
@executor("integration.cognition.scoreUtterance")
builtin cognitionScore {
  spaceId  string  @required
}
`

func newRunnableService() *Service { return New(nil) }

// wantSignatureRange derives the expected signature span straight from the
// fixture buffer, so the assertion cannot drift from the text if the fixture is
// edited and never encodes a hand-counted guess.
func wantSignatureRange(t *testing.T, source, signature string) Range {
	t.Helper()
	idx := strings.Index(source, signature)
	if idx < 0 {
		t.Fatalf("fixture error: signature %q not found", signature)
	}
	if strings.Contains(source[idx+1:], signature) {
		t.Fatalf("fixture error: signature %q is not unique", signature)
	}
	return Range{
		Start: positionFromOffset(source, idx),
		End:   positionFromOffset(source, idx+len(signature)),
	}
}

func byName(t *testing.T, got []RunnableConstruct, name string) RunnableConstruct {
	t.Helper()
	for _, rc := range got {
		if rc.Name == name {
			return rc
		}
	}
	t.Fatalf("construct %q not found in %v", name, names(got))
	return RunnableConstruct{}
}

func names(in []RunnableConstruct) []string {
	out := make([]string, 0, len(in))
	for _, rc := range in {
		out = append(out, rc.Kind+" "+rc.Name)
	}
	return out
}

func TestRunnableConstructs_FindsEveryRunnableKind(t *testing.T) {
	got := newRunnableService().RunnableConstructs(runnableFixture)

	want := []string{
		"query spaceParticipants",
		"mutate mutationCreateSpace",
		"logic logicProvisionDailySpace",
		"tool searchUsers",
		"automation bootstrapSession",
		"automation sweepStalePlans",
	}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("runnable constructs = %v; want %v (declaration order)", names(got), want)
	}
}

// The five runnable kinds are the whole set. spec / trait / shape / concept /
// builtin each defer their execution semantic to a design of their own, so an
// editor must get no Run affordance for them at all.
func TestRunnableConstructs_ExcludesNonRunnableKinds(t *testing.T) {
	got := newRunnableService().RunnableConstructs(runnableFixture)
	for _, forbidden := range []string{
		"isGuestParticipant", "isActiveRow", "spaceCard", "marker", "cognitionScore",
	} {
		for _, rc := range got {
			if rc.Name == forbidden {
				t.Errorf("non-runnable construct %q was returned as kind %q", forbidden, rc.Kind)
			}
		}
	}
}

func TestRunnableConstructs_SignatureRanges(t *testing.T) {
	got := newRunnableService().RunnableConstructs(runnableFixture)
	for _, tc := range []struct{ name, signature string }{
		{"spaceParticipants", "query participant spaceParticipants"},
		{"mutationCreateSpace", "mutate space mutationCreateSpace"},
		{"logicProvisionDailySpace", "logic logicProvisionDailySpace"},
		{"searchUsers", "tool searchUsers"},
		{"bootstrapSession", "automation bootstrapSession"},
		{"sweepStalePlans", "automation sweepStalePlans"},
	} {
		rc := byName(t, got, tc.name)
		want := wantSignatureRange(t, runnableFixture, tc.signature)
		if rc.SignatureRange != want {
			t.Errorf("%s signature range = %+v; want %+v", tc.name, rc.SignatureRange, want)
		}
	}
}

func TestRunnableConstructs_SignatureConcept(t *testing.T) {
	got := newRunnableService().RunnableConstructs(runnableFixture)
	if c := byName(t, got, "spaceParticipants").Concept; c != "participant" {
		t.Errorf("query concept = %q; want %q", c, "participant")
	}
	if c := byName(t, got, "mutationCreateSpace").Concept; c != "space" {
		t.Errorf("mutate concept = %q; want %q", c, "space")
	}
	// logic / tool / automation signatures carry no concept binding.
	for _, name := range []string{"logicProvisionDailySpace", "searchUsers", "bootstrapSession"} {
		if c := byName(t, got, name).Concept; c != "" {
			t.Errorf("%s concept = %q; want empty", name, c)
		}
	}
}

func TestRunnableConstructs_QueryArgsProjection(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(runnableFixture), "spaceParticipants")
	want := []RunnableArg{
		{Name: "spaceId", Type: "string", Required: true, Description: "The space whose participants to read."},
		{Name: "limit", Type: "number", Required: false},
		{Name: "kind", Type: "string", Required: false, Enum: []string{"human", "ai"}},
	}
	if !reflect.DeepEqual(rc.Args, want) {
		t.Errorf("query args = %+v; want %+v", rc.Args, want)
	}
}

func TestRunnableConstructs_ToolFieldsAreTheSchema(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(runnableFixture), "searchUsers")
	want := []RunnableArg{
		{Name: "active", Type: "boolean", Required: false, Description: "Filter by active status"},
		{Name: "limit", Type: "number", Required: false, Description: "Max results to return"},
	}
	if !reflect.DeepEqual(rc.Args, want) {
		t.Errorf("tool args = %+v; want %+v", rc.Args, want)
	}
	if rc.Trigger != nil {
		t.Errorf("tool trigger = %+v; want nil", rc.Trigger)
	}
}

func TestRunnableConstructs_MutationArgsAreRequired(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(runnableFixture), "mutationCreateSpace")
	if len(rc.Args) != 2 {
		t.Fatalf("mutate args = %+v; want 2", rc.Args)
	}
	for _, a := range rc.Args {
		if !a.Required {
			t.Errorf("arg %q required = false; want true (@required)", a.Name)
		}
	}
}

// An automation binds its triggering event as `args`, so it declares no caller
// arguments even though it may carry an args block projecting event fields.
// The trigger is what the editor builds the synthetic event from.
func TestRunnableConstructs_AutomationEventTrigger(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(runnableFixture), "bootstrapSession")
	if len(rc.Args) != 0 {
		t.Errorf("automation args = %+v; want empty (the event is bound as args)", rc.Args)
	}
	if rc.Args == nil {
		t.Error("automation args must be a non-nil empty slice, never nil")
	}
	want := &RunnableTrigger{Event: "node.created", Concept: "v1:cognition:participant"}
	if !reflect.DeepEqual(rc.Trigger, want) {
		t.Errorf("automation trigger = %+v; want %+v", rc.Trigger, want)
	}
}

func TestRunnableConstructs_AutomationScheduleTrigger(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(runnableFixture), "sweepStalePlans")
	if len(rc.Args) != 0 {
		t.Errorf("automation args = %+v; want empty", rc.Args)
	}
	want := &RunnableTrigger{Schedule: "0 */10 * * * *"}
	if !reflect.DeepEqual(rc.Trigger, want) {
		t.Errorf("scheduled automation trigger = %+v; want %+v (no concept, no event)", rc.Trigger, want)
	}
}

// Every arg type the corpus actually uses has to land on one of the six form
// types, and a type the mapping has never heard of has to degrade to "any"
// rather than error -- an untypeable arg is still an arg the developer fills in.
func TestNormaliseArgType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"string", "string"},
		{"text", "string"},
		{"enum", "string"},
		{"datetime", "string"},
		{"timestamp", "string"},
		{"int", "number"},
		{"integer", "number"},
		{"number", "number"},
		{"float", "number"},
		{"double", "number"},
		{"decimal", "number"},
		{"bool", "boolean"},
		{"boolean", "boolean"},
		{"object", "object"},
		{"map", "object"},
		{"json", "object"},
		{"array", "array"},
		{"list", "array"},
		{"[]string", "array"},
		{"[]object", "array"},
		{"any", "any"},
		{"", "any"},
		{"quaternion", "any"},
		{"STRING", "string"},
		{"  int  ", "number"},
	} {
		if got := normaliseArgType(tc.in); got != tc.want {
			t.Errorf("normaliseArgType(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// The array shorthand is lowered by the parser to Type=array + Items, so the
// projection must report "array" through the real parse path too, not only
// through the string mapping above.
func TestRunnableConstructs_ArrayAndAnyArgTypes(t *testing.T) {
	const src = `use cognition.concepts.{ space }

@description("Assorted arg types")
query space assortedTypes {
  args {
    ids        []string
    payload    object
    fraction   float
    stamp      datetime
    freeform   any
    flag       bool
  }
  filter  row.id in args.ids
  shape   spaceCard
}
`
	rc := byName(t, newRunnableService().RunnableConstructs(src), "assortedTypes")
	got := map[string]string{}
	for _, a := range rc.Args {
		got[a.Name] = a.Type
	}
	want := map[string]string{
		"ids": "array", "payload": "object", "fraction": "number",
		"stamp": "string", "freeform": "any", "flag": "boolean",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("arg types = %v; want %v", got, want)
	}
}

func TestRunnableConstructs_NoArgsBlockYieldsEmptySlice(t *testing.T) {
	const src = `use cognition.concepts.{ space }

@description("Every space")
query space allSpaces {
  filter  isActiveRecord
  shape   spaceCard
}
`
	rc := byName(t, newRunnableService().RunnableConstructs(src), "allSpaces")
	if rc.Args == nil {
		t.Fatal("args must be a non-nil empty slice, never nil")
	}
	if len(rc.Args) != 0 {
		t.Errorf("args = %+v; want empty", rc.Args)
	}
}

// The editor asks on every keystroke, so a half-typed buffer is the normal
// case. It must degrade to an empty list, never an error and never a panic.
func TestRunnableConstructs_MalformedBufferDegradesQuietly(t *testing.T) {
	for name, src := range map[string]string{
		"empty":            "",
		"whitespace":       "   \n\t\n",
		"unbalanced brace": "query participant spaceParticipants {\n  args {\n    spaceId string\n",
		"mid-typed header": "@description(\"wip\")\nquery partici",
		"garbage":          "}}}} not memql at all ((((",
		"unterminated str": "@description(\"wip\nquery participant p {\n}\n",
		"body nonsense":    "query participant p {\n  filter  &&&&\n  shape   x\n}\n",
	} {
		got := newRunnableService().RunnableConstructs(src)
		if got == nil {
			t.Errorf("%s: result must be a non-nil empty slice, never nil", name)
		}
		if len(got) != 0 {
			t.Errorf("%s: got %v; want no runnable constructs", name, names(got))
		}
	}
}

// One broken construct must not erase the run affordances of its healthy
// neighbours -- that is the whole reason each construct is parsed in isolation.
func TestRunnableConstructs_BrokenConstructDoesNotEraseNeighbours(t *testing.T) {
	const src = `use cognition.concepts.{ participant, space }

@description("Healthy")
query participant healthyQuery {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId
  shape   participantFull
}

@description("Broken -- logic without its mandatory body block")
logic brokenLogic {
  args {
    event object @required
  }
}

@description("Also healthy")
mutate space healthyMutation {
  args {
    spaceId  string  @required
  }
  insert {
    id:        args.spaceId
    createdAt: now
  }
}
`
	got := newRunnableService().RunnableConstructs(src)
	want := []string{"query healthyQuery", "mutate healthyMutation"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("constructs = %v; want %v (the broken logic is dropped, the rest survive)", names(got), want)
	}
}

// A Sense column counts runes (the lexer scans a []rune and does one column++
// per rune), so a multi-byte character in a signature must shift the end column
// by ONE, not by its UTF-8 byte width. Getting this wrong puts the CodeLens
// anchor past the end of the line.
//
// The fixture uses a tool because the struct-form lowering that query / mutate
// / logic / automation go through matches ASCII-only name patterns, so a
// non-ASCII name is only reachable on the natively-parsed declaration kinds.
func TestRunnableConstructs_SignatureColumnsCountRunes(t *testing.T) {
	const src = `@description("Non-ASCII identifiers are legal -- the lexer admits unicode.IsLetter")
@handler(type="query", query="concept==v1:memql:backend:user")
tool searchÜsers {
  active  boolean  @description("Filter by active status")
}
`
	rc := byName(t, newRunnableService().RunnableConstructs(src), "searchÜsers")
	want := wantSignatureRange(t, src, "tool searchÜsers")
	if rc.SignatureRange != want {
		t.Fatalf("signature range = %+v; want %+v", rc.SignatureRange, want)
	}
	// Pin the value explicitly: `tool searchÜsers` is 16 runes but 17 bytes,
	// so a byte-counting implementation would report an end column of 18.
	if rc.SignatureRange.Start.Column != 1 || rc.SignatureRange.End.Column != 17 {
		t.Errorf("signature columns = %d..%d; want 1..17 (byte counting would give 18)",
			rc.SignatureRange.Start.Column, rc.SignatureRange.End.Column)
	}
}

// Token.Pos / Token.EndPos are RUNE offsets (the lexer scans a []rune), so the
// per-construct fragment slice has to be taken over runes. A single multi-byte
// character earlier in the buffer -- an accented word in a description, which
// is entirely ordinary -- would otherwise shift every later slice left by its
// extra byte count and silently drop the constructs after it.
func TestRunnableConstructs_MultiByteEarlierInBufferDoesNotShiftSlices(t *testing.T) {
	const src = `use cognition.concepts.{ participant, space }

@description("Participants, naïvely filtered — em dash and accents included")
query participant firstQuery {
  args {
    spaceId  string  @required
  }
  filter  spaceId==args.spaceId
  shape   participantFull
}

@description("Créer un espace")
mutate space secondMutation {
  args {
    spaceId  string  @required
  }
  insert {
    id:        args.spaceId
    createdAt: now
  }
}

@description("Third, plain ASCII")
logic thirdLogic {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser(userId: args.event.payload.id)
  }
}
`
	got := newRunnableService().RunnableConstructs(src)
	want := []string{"query firstQuery", "mutate secondMutation", "logic thirdLogic"}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("constructs = %v; want %v (rune-offset slicing)", names(got), want)
	}
	for _, name := range []string{"firstQuery", "secondMutation", "thirdLogic"} {
		rc := byName(t, got, name)
		if len(rc.Args) != 1 {
			t.Errorf("%s args = %+v; want exactly one (a shifted slice loses the args block)", name, rc.Args)
		}
	}
	if rc := byName(t, got, "thirdLogic"); rc.SignatureRange != wantSignatureRange(t, src, "logic thirdLogic") {
		t.Errorf("thirdLogic signature range = %+v; want the authored span", rc.SignatureRange)
	}
}

// A construct with no annotation preamble is sliced from its keyword, and one
// with a preamble is sliced from the first `@` -- otherwise an automation's
// @trigger would be cut off, or a preceding non-runnable construct's
// annotations would be spliced onto the next runnable one.
func TestRunnableConstructs_AnnotationPreambleAttribution(t *testing.T) {
	const src = `use cognition.concepts.{ participant }

@enabled
@description("A non-runnable spec whose annotations must stay with it")
spec participant isGuestParticipant {
  return isGuest == true
}

@trigger(event="node.deleted", concept="v1:cognition:participant")
automation onParticipantRemoved {
  step decide {
    logic onParticipantRemoved ( event )
  }
}

automation noPreamble {
  step decide {
    logic noPreamble ( event )
  }
}
`
	got := newRunnableService().RunnableConstructs(src)
	want := []string{"automation onParticipantRemoved", "automation noPreamble"}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("constructs = %v; want %v", names(got), want)
	}
	trigger := byName(t, got, "onParticipantRemoved").Trigger
	if trigger == nil || trigger.Event != "node.deleted" || trigger.Concept != "v1:cognition:participant" {
		t.Errorf("trigger = %+v; want the @trigger kwargs (the preamble must be inside the parsed slice)", trigger)
	}
	if tr := byName(t, got, "noPreamble").Trigger; tr != nil {
		t.Errorf("trigger for an un-annotated automation = %+v; want nil", tr)
	}
}

// The terse single-step automation form declares no body at all:
//
//	automation NAME @trigger(...) => logic targetLogic
//
// Ten of them live in dsl/. They are runnable automations, and -- because
// their `@trigger` sits at depth 0 with no body to close -- failing to
// recognise one also strands its annotation as a preamble that swallows the
// NEXT declaration. Both halves are asserted here.
func TestRunnableConstructs_TerseAutomationForm(t *testing.T) {
	const src = `use identity.concepts.{ user }

/// Soft-revoke expired delegations every 5 minutes.
automation expireDelegations @trigger(schedule="0 */5 * * * *") => logic revokeExpiredDelegations

/// React to a new delegation row.
automation onDelegationCreated @trigger(event="node.created", concept="v1:identity:delegation", partition="*") => logic onDelegationCreated

@description("A block-form automation immediately after two terse ones")
@trigger(event="node.updated", concept="v1:identity:user")
automation onUserUpdated {
  step decide {
    logic onUserUpdated ( event )
  }
}
`
	got := newRunnableService().RunnableConstructs(src)
	want := []string{
		"automation expireDelegations",
		"automation onDelegationCreated",
		"automation onUserUpdated",
	}
	if !reflect.DeepEqual(names(got), want) {
		t.Fatalf("constructs = %v; want %v", names(got), want)
	}

	terse := byName(t, got, "expireDelegations")
	if terse.SignatureRange != wantSignatureRange(t, src, "automation expireDelegations") {
		t.Errorf("terse signature range = %+v; want the `automation NAME` span", terse.SignatureRange)
	}
	if w := (&RunnableTrigger{Schedule: "0 */5 * * * *"}); !reflect.DeepEqual(terse.Trigger, w) {
		t.Errorf("terse trigger = %+v; want %+v", terse.Trigger, w)
	}
	if len(terse.Args) != 0 {
		t.Errorf("terse automation args = %+v; want empty", terse.Args)
	}

	evented := byName(t, got, "onDelegationCreated")
	wantTrigger := &RunnableTrigger{Event: "node.created", Concept: "v1:identity:delegation"}
	if !reflect.DeepEqual(evented.Trigger, wantTrigger) {
		t.Errorf("terse event trigger = %+v; want %+v", evented.Trigger, wantTrigger)
	}

	// The block automation after them must keep its OWN trigger, proving the
	// terse declarations did not strand a preamble that ran into it.
	block := byName(t, got, "onUserUpdated")
	wantBlock := &RunnableTrigger{Event: "node.updated", Concept: "v1:identity:user"}
	if !reflect.DeepEqual(block.Trigger, wantBlock) {
		t.Errorf("block automation trigger = %+v; want %+v", block.Trigger, wantBlock)
	}
}

// A corpus sweep: every construct the live dsl/ tree declares with a runnable
// keyword must come back from the scan. The oracle is a deliberately dumb
// header regex -- it exists only to enumerate what SHOULD be found, never to
// produce the answer -- so a fragment-parse regression anywhere in ~520 real
// constructs shows up as a concrete missing name rather than a silent
// coverage hole.
func TestRunnableConstructs_CoversTheLiveCorpus(t *testing.T) {
	const corpusRoot = "../../../dsl"
	if _, err := os.Stat(corpusRoot); err != nil {
		t.Skipf("dsl tree not reachable from here: %v", err)
	}
	headerRe := regexp.MustCompile(
		`(?m)^(query|mutate|logic|tool|automation)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*[{@]`)

	svc := newRunnableService()
	scanned, missing := 0, []string{}
	err := filepath.WalkDir(corpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return err
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found := map[string]bool{}
		for _, rc := range svc.RunnableConstructs(string(source)) {
			found[rc.Kind+" "+rc.Name] = true
		}
		for _, m := range headerRe.FindAllStringSubmatch(string(source), -1) {
			scanned++
			key := m[1] + " " + m[2]
			if !found[key] {
				missing = append(missing, key+" in "+path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", corpusRoot, err)
	}
	if scanned < 400 {
		t.Fatalf("oracle found only %d runnable headers in %s; the corpus or the regex moved", scanned, corpusRoot)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d corpus constructs were not returned:\n  %s",
			len(missing), scanned, strings.Join(missing, "\n  "))
	}
}

// lifecycleFixture carries the two states memql#3333 added to the contract:
// `@autoInjected` tool fields, and `@disabled` on each runnable kind that can
// take it. It is kept separate from runnableFixture so the existing projection
// assertions keep asserting the DEFAULT (both flags false) on constructs that
// carry neither annotation.
const lifecycleFixture = `use cognition.concepts.{ space }

@description("Produce a file deliverable")
@handler(type="function", function="produceArtifact")
tool produceArtifact {
  filename     string  @required @description("Name of the file to write")
  ownerUserId  string  @autoInjected @description("Server-stamped owner")
  partitionId  string  @required @autoInjected @description("Server-stamped partition")
}

@disabled
@description("A tool the loader skips")
@handler(type="query", query="concept==v1:memql:backend:user")
tool retiredSearch {
  limit  integer
}

@disabled
@description("A query the loader skips")
query space disabledSpaceLookup {
  args {
    spaceId  string  @required
  }
  filter  row.id==args.spaceId
  shape   spaceCard
}

@disabled
@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation disabledBootstrap {
  step decide {
    logic bootstrapSession ( event )
  }
}

@enabled
@description("An explicitly-enabled logic")
logic enabledLogic {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser(userId: args.event.payload.id)
  }
}
`

// A tool field's @autoInjected has to arrive as a PER-FIELD flag. The engine
// stamps those values server-side and drops whatever the caller sent, so a
// client that cannot tell them apart has to disclaim the whole form -- which is
// how the extension's blanket notice came to exist (memql#3333).
//
// The flagged fields stay IN the arg list. Filtering them out here would hide a
// field the tool's own schema declares and diverge from what dispatch does.
func TestRunnableConstructs_ToolAutoInjectedFieldsAreFlagged(t *testing.T) {
	rc := byName(t, newRunnableService().RunnableConstructs(lifecycleFixture), "produceArtifact")
	want := []RunnableArg{
		{Name: "filename", Type: "string", Required: true, Description: "Name of the file to write"},
		{Name: "ownerUserId", Type: "string", Required: false, Description: "Server-stamped owner", AutoInjected: true},
		{Name: "partitionId", Type: "string", Required: true, Description: "Server-stamped partition", AutoInjected: true},
	}
	if !reflect.DeepEqual(rc.Args, want) {
		t.Errorf("tool args = %+v;\nwant %+v", rc.Args, want)
	}
}

// query / mutate / logic args have no @autoInjected annotation at all, so the
// flag must never come back true for them -- a client that marks an ordinary
// arg as engine-supplied tells the developer their value is ignored when it is
// not.
func TestRunnableConstructs_NonToolArgsAreNeverAutoInjected(t *testing.T) {
	got := newRunnableService().RunnableConstructs(runnableFixture)
	for _, name := range []string{"spaceParticipants", "mutationCreateSpace", "logicProvisionDailySpace"} {
		for _, a := range byName(t, got, name).Args {
			if a.AutoInjected {
				t.Errorf("%s arg %q autoInjected = true; want false (only a tool field can carry @autoInjected)", name, a.Name)
			}
		}
	}
}

// @disabled means the construct is not loaded at runtime right now, so a run of
// it can only be refused. Reporting it lets a client render the state instead
// of surfacing it as a FAILED_PRECONDITION after the click, where it is
// indistinguishable from a @filter miss.
//
// It is reported, NOT filtered: @disabled is a reversible switch and the
// construct is still a real declaration in the buffer.
func TestRunnableConstructs_DisabledIsReported(t *testing.T) {
	got := newRunnableService().RunnableConstructs(lifecycleFixture)
	for _, name := range []string{"retiredSearch", "disabledSpaceLookup", "disabledBootstrap"} {
		if !byName(t, got, name).Disabled {
			t.Errorf("%s disabled = false; want true (@disabled)", name)
		}
	}
}

// ENABLED is the ABSENCE of @disabled, not the presence of @enabled: @enabled
// is the explicit-on default and a no-op, and the overwhelming majority of the
// corpus carries neither annotation.
func TestRunnableConstructs_EnabledAndUnannotatedAreNotDisabled(t *testing.T) {
	explicit := newRunnableService().RunnableConstructs(lifecycleFixture)
	if byName(t, explicit, "enabledLogic").Disabled {
		t.Error("enabledLogic disabled = true; want false (@enabled is the explicit-on default)")
	}
	if byName(t, explicit, "produceArtifact").Disabled {
		t.Error("produceArtifact disabled = true; want false (carries no lifecycle annotation)")
	}
	for _, rc := range newRunnableService().RunnableConstructs(runnableFixture) {
		if rc.Disabled {
			t.Errorf("%s %s disabled = true; want false (the fixture disables nothing)", rc.Kind, rc.Name)
		}
	}
}
