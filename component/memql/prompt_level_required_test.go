package memql

import (
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"text/template"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestPromptWithoutLevelIsRefusedAtLoad (memql#5426): a prompt that declares
// no @level is a skip on the load report -- which strict boot refuses -- naming
// the prompt, its line and the fix under prompt_level_missing, and it is not
// registered. A @disabled prompt is held to the rule too: disabled is "not
// loaded now", not exempt from the language. The prompt that declares one is
// the control: it registers, and draws no skip.
func TestPromptWithoutLevelIsRefusedAtLoad(t *testing.T) {
	overlay := fstest.MapFS{
		"prompts.memql": {Data: []byte(`/// Has a level.
@templateFile("summ.tmpl")
@level("fast")
prompt levelProbeWithLevel {
  title  string!  @description("The title.")
}

/// Has none.
@templateFile("summ.tmpl")
prompt levelProbeWithoutLevel {
  title  string!  @description("The title.")
}

/// Disabled, and has none.
@disabled
@templateFile("summ.tmpl")
prompt levelProbeDisabledWithoutLevel {
  title  string!  @description("The title.")
}
`)},
		"summ.tmpl": {Data: []byte("Summarise {{.title}}")},
	}
	const domain = "promptlevelrequired"
	memqldsl.RegisterTree(domain, withLanguageLine(overlay))
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	registry := newPromptRegistry()
	rep := newLoadReport()
	if _, err := LoadUnifiedPrompts(discardLogger(), registry, template.New("partials"), rep); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}

	if _, ok := registry.Get("levelProbeWithLevel"); !ok {
		t.Error("the prompt that declares @level did not register")
	}
	if _, ok := registry.Get("levelProbeWithoutLevel"); ok {
		t.Error("a prompt with no @level registered")
	}

	want := map[string]int{"levelProbeWithoutLevel": 10, "levelProbeDisabledWithoutLevel": 17}
	for _, s := range rep.Skipped {
		if !strings.HasPrefix(s.File, domain+"/") {
			continue
		}
		line, expected := want[s.Name]
		if !expected {
			t.Errorf("unexpected skip: %+v", s)
			continue
		}
		delete(want, s.Name)
		if s.Code != CodePromptLevelMissing || s.Phase != "level" {
			t.Errorf("skip for %s: code %q phase %q, want %q at phase level", s.Name, s.Code, s.Phase, CodePromptLevelMissing)
		}
		for _, part := range []string{
			"line " + strconv.Itoa(line) + ":",
			"no @level declared",
			"fast, strong, reasoning, embeddings",
			`@level("fast")`,
			"[" + CodePromptLevelMissing + "]",
		} {
			if !strings.Contains(s.Err, part) {
				t.Errorf("skip for %s does not say %q: %s", s.Name, part, s.Err)
			}
		}
	}
	for name := range want {
		t.Errorf("no skip for %s: a prompt with no @level loaded", name)
	}
}
