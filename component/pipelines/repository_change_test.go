package pipelines

import (
	"encoding/json"
	"strings"
	"testing"
)

func repositoryChangeFixture() (RepositoryChangeRule, RepositoryProposal) {
	rule := RepositoryChangeRule{Name: "engine-watch", Repository: "acme/widget", Mode: "propose", BranchPrefix: "engine-bump/", Paths: []string{"product.env"}}
	proposal := RepositoryProposal{Schema: 1, Commit: strings.Repeat("a", 40), Key: "v1.2.3", Title: "engine: bump to v1.2.3", Body: "Review the new release and image pins.",
		Files: []RepositoryReplacement{{Path: "product.env", BaseSHA256: strings.Repeat("b", 64), Content: "ENGINE_REF=v1.2.3\n"}}}
	return rule, proposal
}

func TestRepositoryProposalCannotExpandItsOperatorGrant(t *testing.T) {
	for name, mutate := range map[string]func(*RepositoryProposal){
		"another commit":   func(p *RepositoryProposal) { p.Commit = strings.Repeat("c", 40) },
		"another file":     func(p *RepositoryProposal) { p.Files[0].Path = "go.mod" },
		"escape":           func(p *RepositoryProposal) { p.Files[0].Path = "../product.env" },
		"duplicate file":   func(p *RepositoryProposal) { p.Files = append(p.Files, p.Files[0]) },
		"unknown base":     func(p *RepositoryProposal) { p.Files[0].BaseSHA256 = "latest" },
		"binary":           func(p *RepositoryProposal) { p.Files[0].Content = "\x00" },
		"large file":       func(p *RepositoryProposal) { p.Files[0].Content = strings.Repeat("x", (1<<20)+1) },
		"branch traversal": func(p *RepositoryProposal) { p.Key = "a/../../main" },
		"title newline":    func(p *RepositoryProposal) { p.Title = "title\nsecond" },
	} {
		t.Run(name, func(t *testing.T) {
			rule, proposal := repositoryChangeFixture()
			commit := proposal.Commit
			mutate(&proposal)
			raw, _ := json.Marshal(proposal)
			if _, err := ReadRepositoryProposal(raw, rule, commit); err == nil {
				t.Fatal("proposal escaped its grant")
			}
		})
	}
}

func TestRepositoryProposalRejectsAmbiguousAndUnknownJSON(t *testing.T) {
	rule, proposal := repositoryChangeFixture()
	raw, _ := json.Marshal(proposal)
	for _, bad := range []string{
		strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":2`, 1),
		strings.Replace(string(raw), `"schema":1`, `"schema":1,"Schema":2`, 1),
		strings.Replace(string(raw), `"schema":1`, `"schema":1,"token":"writer"`, 1),
		string(raw) + `{}`, string(raw) + `null`,
	} {
		if _, err := ReadRepositoryProposal([]byte(bad), rule, proposal.Commit); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
}

func TestRepositoryProposalUnchangedAndAllowedNewFile(t *testing.T) {
	rule, proposal := repositoryChangeFixture()
	for _, files := range [][]RepositoryReplacement{nil, proposal.Files, {{Path: "product.env", Content: "ENGINE_REF=v1.2.3\n"}}} {
		proposal.Files = files
		raw, _ := json.Marshal(proposal)
		if _, err := ReadRepositoryProposal(raw, rule, proposal.Commit); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryUpdateRequiresBothActorAndAuthor(t *testing.T) {
	rule, _ := repositoryChangeFixture()
	rule.Mode, rule.BranchPrefix = "update", "dependabot/go_modules/"
	if rule.Validate() == nil {
		t.Fatal("missing identities allowed")
	}
	rule.Author = "dependabot[bot]"
	if rule.Validate() == nil {
		t.Fatal("missing signed actor allowed")
	}
	rule.Actor = "dependabot[bot]"
	if err := rule.Validate(); err != nil {
		t.Fatal(err)
	}
	rule.Paths = []string{".github/workflows/ci.yml"}
	if rule.Validate() == nil {
		t.Fatal("workflow write allowed")
	}
}

func TestRepositoryChangeCompilationKeepsWriterOutOfWorker(t *testing.T) {
	spec := &Spec{Image: "toolchain", Caches: []string{"go"}, Stages: []StageSpec{
		{Name: "proposal", Steps: []StepSpec{{Name: "generate", Run: "prepare", Artifacts: []string{"proposal.json"}}}},
		{Name: "apply", Steps: []StepSpec{{Name: "change", RepositoryChange: &RepositoryChange{Rule: "tidy", Step: "proposal.generate", Path: "proposal.json"}}}},
	}}
	plan, err := Compile(spec, CompileInput{Event: EventPush, Mode: ModeFull, Compute: ComputeCluster})
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps()[1]
	if step.Kind != StepRepositoryChange || step.Image != "" || step.Run != "" || len(step.Caches) != 0 || len(step.Secrets) != 0 {
		t.Fatal("writer acquired command authority", step)
	}
	spec.Stages[1].Steps[0].RepositoryChange.Rule = "other"
	if step.RepositoryChange.Rule != "tidy" {
		t.Fatal("compiled authority aliased manifest")
	}
	if _, err := Compile(spec, CompileInput{StageSelection: &StageSelection{Included: []string{"apply"}}}); err == nil {
		t.Fatal("writer without producer accepted")
	}
	for _, change := range []func(*StepSpec){func(s *StepSpec) { s.Run = "shell" }, func(s *StepSpec) { s.GitHubReadToken = true }, func(s *StepSpec) { s.Secrets = []string{"WRITER"} }, func(s *StepSpec) { s.SARIF = &SARIFReport{Step: "proposal.generate", Path: "proposal.sarif"} }} {
		s := spec.Stages[1].Steps[0]
		change(&s)
		if validateRepositoryChangeStep(spec, "apply", s) == nil {
			t.Fatal("command authority accepted")
		}
	}
}

func TestRepositoryChangeOperatorRulesRefuseAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"schema":1,"schema":1,"rules":[]}`, `{"schema":2,"rules":[]}`, `{"schema":1,"rules":[],"token":"untrusted"}`, `{"schema":1,"rules":[]} {}`} {
		if _, err := ReadRepositoryChangeRules([]byte(raw)); err == nil {
			t.Fatal("invalid operator config accepted", raw)
		}
	}
}
