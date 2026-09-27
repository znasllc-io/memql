package sense

// tool_gates_test.go -- the editor knows the tool gates of memql#5438 the
// moment the registry does: hover explains each, and a use of the deprecated
// @allowedRoles is squiggled with the form's warning.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

func TestHover_TheToolGates(t *testing.T) {
	s := New(&stubRegistry{})
	for _, tc := range []struct {
		src  string
		want string
	}{
		{"@requiresAgentRole(\"assistant\")\ntool probe {\n}\n", "AGENT-KIND gate"},
		{"@requiresRank(\"developer\")\ntool probe {\n}\n", "FLOOR"},
		{"@allowedRoles(\"assistant\")\ntool probe {\n}\n", "DEPRECATED"},
	} {
		res := hoverAt(t, s, tc.src, 1, 4)
		if res == nil || !strings.Contains(res.Contents, "(annotation)") || !strings.Contains(res.Contents, tc.want) {
			t.Errorf("hover over %q = %+v; want an annotation hover saying %q", strings.SplitN(tc.src, "\n", 2)[0], res, tc.want)
		}
	}
}

func TestDiagnose_AllowedRolesWarnsWithTheFormsWarning(t *testing.T) {
	form, ok := deprecation.Lookup(deprecation.AllowedRoles)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.AllowedRoles)
	}
	src := "/// A probe.\n@handler(type=\"function\", name=\"probe\")\n@allowedRoles(\"assistant\")\ntool probe {\n  a  string\n}\n"
	var found []Diagnostic
	for _, d := range New(nil).Diagnose(src, "") {
		if d.Code == deprecation.AllowedRoles {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s diagnostic, got %+v", deprecation.AllowedRoles, found)
	}
	d := found[0]
	if d.Severity != SeverityWarning || d.Message != form.Warning() || d.Range.Start.Line != 3 || d.Range.Start.Column != 1 {
		t.Errorf("got %+v; want a Warning at 3:1 carrying the form's warning", d)
	}
	// The replacement spelling draws nothing.
	for _, d := range New(nil).Diagnose(strings.Replace(src, "@allowedRoles", "@requiresAgentRole", 1), "") {
		if d.Code == deprecation.AllowedRoles || d.Severity == SeverityError {
			t.Errorf("the replacement still reports %+v", d)
		}
	}
}
