package automations_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Exercise the loaded contracts and bindings, not source-text matching: an
// optional field omitted at either hop must not disappear before the script.
func TestGitOpsAppArgumentsSurviveInstallAutomation(t *testing.T) {
	fields := map[string]string{
		"githubAppId": "repoGithubAppId", "githubAppInstallationId": "repoGithubAppInstallationId",
		"githubAppPrivateKeyFile": "repoGithubAppPrivateKeyFile", "githubAppEnterpriseBaseUrl": "repoGithubAppEnterpriseBaseUrl",
	}
	for _, name := range []string{"installInstance", "bringUpInstance"} {
		a := loadInstallAutomation(t, name)
		for _, field := range fields {
			found := false
			for _, arg := range a.Args.Fields {
				if arg.Name == field {
					found = arg.Type == "string" && arg.Optional
				}
			}
			if !found {
				t.Fatalf("%s lost optional string %s", name, field)
			}
		}
		var bound map[string]any
		for _, step := range a.Steps {
			if name == "installInstance" && step.Action != nil && strings.Split(step.Action.Ref, "@")[0] == "registerGitOpsRepo" {
				bound = step.Action.Args
			}
			if name == "bringUpInstance" && step.Automation != nil && step.Automation.Name == "installInstance" {
				bound = step.Automation.Args
			}
		}
		if bound == nil {
			t.Fatalf("%s lost repository setup call", name)
		}
		for key, field := range fields {
			if name == "bringUpInstance" {
				key = field
			}
			leaf, ok := bound[key].(*automations.ExprLeaf)
			if !ok || leaf.Node == nil || leaf.Src != "args."+field {
				t.Errorf("%s %s lost compiled reference to args.%s", name, key, field)
			}
		}
	}
	source, err := fs.ReadFile(memqldsl.Tree(), "deployment/actions.memql")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := actions.LoadSource(string(source), "deployment/actions.memql")
	if err != nil {
		t.Fatal(err)
	}
	var action *actions.Action
	for _, candidate := range loaded {
		if candidate.Name == "registerGitOpsRepo" {
			action = candidate
		}
	}
	if action == nil {
		t.Fatal("repository action missing")
	}
	for field := range fields {
		declared, forwarded := false, false
		for _, param := range action.Params {
			if param.Name == field {
				declared = param.Type == "string" && !param.Required
			}
		}
		for _, arg := range action.CallArgs {
			if arg.Key == field {
				forwarded = arg.ArgPath == field
			}
		}
		if !declared || !forwarded {
			t.Errorf("action lost optional argument forwarding for %s", field)
		}
	}
}
