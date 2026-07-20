package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// #2625: an unknown actor member is a LOAD error on every function
// surface -- previously a runtime error on two and a silent nil on the
// other two.
func TestValidateActorMemberPaths(t *testing.T) {
	fd := func(kind languageParser.FunctionType, name string) *languageParser.FunctionDef {
		return &languageParser.FunctionDef{Name: name, Type: kind}
	}
	bad := `@actor
func (Query) todos(_ any) {
  return concept==v1:todos:todo AND ownerUserId == actor.displayName, nil
}`
	err := validateActorMemberPaths(bad, fd(languageParser.FunctionTypeQuery, "todos"))
	if err == nil {
		t.Fatal("unknown actor member must fail load")
	}
	if !strings.Contains(err.Error(), "displayName") || !strings.Contains(err.Error(), "valid:") {
		t.Errorf("error must name the member and the valid set: %v", err)
	}

	for _, kind := range []languageParser.FunctionType{
		languageParser.FunctionTypeMutation,
		languageParser.FunctionTypeLogic,
		languageParser.FunctionTypeAutomation,
	} {
		src := "@actor\nfunc (X) probe(_ any) {\n  v := actor.id\n  return v, nil\n}"
		if err := validateActorMemberPaths(src, fd(kind, "probe")); err == nil {
			t.Errorf("%v: unknown member must fail load (the silent-nil surfaces too)", kind)
		}
	}

	for name, ok := range map[string]string{
		"canonical": "actor.userId",
		"alias":     "actor.isOwner",
		"now":       "actor.now",
		"email":     "actor.primaryEmail",
	} {
		src := "@actor\nfunc (Query) probe(_ any) {\n  v := " + ok + "\n  return v, nil\n}"
		if err := validateActorMemberPaths(src, fd(languageParser.FunctionTypeQuery, "probe")); err != nil {
			t.Errorf("%s (%s) must load: %v", name, ok, err)
		}
	}

	// The corpus non-targets never flag.
	prose := "func (Query) probe(_ any) {\n  // governance compares actor.rank to target.rank\n  return concept==v1:todos:todo, nil\n}"
	if err := validateActorMemberPaths(prose, fd(languageParser.FunctionTypeQuery, "probe")); err != nil {
		t.Errorf("comment prose must not flag: %v", err)
	}
	stamp := "func (Logic) onThing(_ any) {\n  who := event.actor.id\n  return who, nil\n}"
	if err := validateActorMemberPaths(stamp, fd(languageParser.FunctionTypeLogic, "onThing")); err != nil {
		t.Errorf("event.actor.id is the event stamp, not the auth envelope: %v", err)
	}
}

// TestActorMemberRuleSourcesAreShared is the story's conformance
// requirement: the loader rule and the sense rule must consume the SAME
// tables -- no second hand-maintained property list may creep in. Both
// are driven here through their public entry points over one corpus of
// probes; any divergence in accept/reject fails.
func TestActorMemberRuleSourcesAreShared(t *testing.T) {
	probes := []string{
		"actor.userId", "actor.role", "actor.identityId", "actor.isClusterOwner",
		"actor.primaryEmail", "actor.now", "actor.isOwner",
		"actor.displayName", "actor.id", "actor.rank", "actor.userid", "actor.config",
	}
	for _, probe := range probes {
		body := "@actor\nquery todo probe {\n  filter todo.o == " + probe + "\n}\n"
		senseFlags := len(sense.ActorUnknownPropertyDiagnostics(body)) > 0

		loaderSrc := "@actor\nfunc (Query) probe(_ any) {\n  v := " + probe + "\n  return v, nil\n}"
		loaderFlags := validateActorMemberPaths(loaderSrc,
			&languageParser.FunctionDef{Name: "probe", Type: languageParser.FunctionTypeQuery}) != nil

		if senseFlags != loaderFlags {
			t.Errorf("%s: sense flags=%v but loader flags=%v -- the two rules have drifted", probe, senseFlags, loaderFlags)
		}
		_, valid := auth.ActorEnvelopeCanonicalName(strings.TrimPrefix(probe, "actor."))
		if loaderFlags == valid {
			t.Errorf("%s: loader verdict disagrees with the canonical table", probe)
		}
	}
}
