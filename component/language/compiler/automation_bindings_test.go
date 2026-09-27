package compiler

import (
	"strings"
	"testing"
)

func TestAutomationBindingsAtCompile(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"before-write actor missing", "@trigger(before=\"write\", concept=\"v1:probe:ticket\")\nautomation probe { row.owner = actor.userId }", "does not declare @actor"},
		{"before-write actor declared", "@actor\n@trigger(before=\"write\", concept=\"v1:probe:ticket\")\nautomation probe { row.owner = actor.userId }", ""},
		{"before-write argument missing", "@trigger(before=\"write\", concept=\"v1:probe:ticket\")\nautomation probe { row.title = args.title }", "body reads args.title"},
		{"actor missing", "automation probe { return actor.userId }", "does not declare @actor"},
		{"actor declared", "@actor\nautomation probe { return actor.userId }", ""},
		{"argument missing", "automation probe { return args.path }", "body reads args.path"},
		{"argument declared", "automation probe {\n args {\n path string\n }\n return args.path\n}", ""},
		{"nested argument", "automation probe { for row in [] { builtin run(path: args.path) } }", "body reads args.path"},
		{"string is not read", "automation probe { return \"actor.userId args.path\" }", ""},
		{"lambda actor", "automation probe { return [].map(actor => actor.userId) }", ""},
		{"lambda args", "automation probe { return [].map(args => args.path) }", ""},
		// memql#5426: the header's lambdas read the bound args as a statement
		// does, so an undeclared read there is refused under the same code.
		{"trigger filter argument missing", "@trigger(event=\"node.updated\", concept=\"v1:probe:ticket\")\n@filter(row => row.status == args.wanted)\nautomation probe {\n args {\n id string\n }\n return args.id\n}", "trigger filter reads args.wanted, which is not declared in its args block"},
		{"trigger filter argument declared", "@trigger(event=\"node.updated\", concept=\"v1:probe:ticket\")\n@filter(row => row.status == args.wanted)\nautomation probe {\n args {\n id string\n wanted string\n }\n return args.id + args.wanted\n}", ""},
		{"trigger filter lambda args", "@trigger(event=\"node.updated\", concept=\"v1:probe:ticket\")\n@filter(row => row.tags.any(args => args == \"x\"))\nautomation probe {\n args {\n id string\n }\n return args.id\n}", ""},
		{"loop until argument missing", "@trigger(event=\"node.updated\", concept=\"v1:probe:ticket\")\n@filter(row => row.status != \"done\")\n@loop(maxDepth=3, until=row => row.status == args.goal)\nautomation probe {\n args {\n id string\n }\n return args.id\n}", "@loop until reads args.goal, which is not declared in its args block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileSource(tc.source)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want %q", err, tc.want)
			} else if strings.Contains(tc.want, "args.") && !strings.Contains(err.Error(), "["+CodeArgsUndeclared+"]") {
				t.Fatalf("the undeclared-argument refusal does not carry %s: %v", CodeArgsUndeclared, err)
			}
		})
	}
}
