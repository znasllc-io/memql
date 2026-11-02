package pipelinerun

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// deploy_guards.go -- what makes a release wait for the owner's Deploy button (design
// record docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section
// 7, step 1). Each is read between what the installation runs and the release, because
// a release held back at one of these is still in the range of the next.
//
// A WRONG "WAIT" COSTS ONE PRESS OF A BUTTON, AND A WRONG "GO" COSTS PRODUCTION, so every
// doubt waits: a comparison that stopped listing, a migration this cannot read through,
// more files with no patch than it will read.

// deployUnpatchedLimit bounds the files read whole because GitHub gave no patch. GitHub
// stops giving them part way through a large comparison: 18 of 181 files between two
// real releases, and 79 of 300 across nine.
const deployUnpatchedLimit = 60

func (i *Integration) deployGuards(ctx context.Context, gh trainGitHub, token string, p Pipeline, installed, sha string) ([]pipelines.DeployGuard, error) {
	guards := []pipelines.DeployGuard{}
	files, complete, err := gh.ComparePatches(ctx, token, p.Repository, installed, sha)
	if err != nil {
		return nil, fmt.Errorf("comparing %s with %s: %w", installed, shortCommit(sha), err)
	}
	if !complete {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardUnknown,
			Detail: fmt.Sprintf("More than %d files changed since %s, so what it does to tables and machines could not all be read.", len(files), installed)})
		return guards, nil
	}
	var changing, contracts []string
	unpatched := 0
	readSide := func(ref, path string) (string, error) {
		body, err := gh.FileAtRef(ctx, token, p.Repository, ref, path)
		if githubapp.StatusOf(err) == http.StatusNotFound {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("reading %s at %s: %w", path, shortCommit(ref), err)
		}
		return string(body), nil
	}
	for _, file := range files {
		schema, contract := pipelines.EngineCodePath(file.Path), pipelines.WorkerContractPath(file.Path)
		if pipelines.MigrationPath(file.Path) {
			source, err := readSide(sha, file.Path)
			if err != nil {
				return nil, err
			}
			if source == "" || pipelines.ClassifyMigrationFile(file.Path, source) == pipelines.MigrationData {
				changing = append(changing, file.Path)
				continue
			}
		}
		if !schema && !contract {
			continue
		}
		var lines []string
		switch {
		case file.Patch != "":
			lines = pipelines.PatchLines(file.Patch)
		case file.Status == "renamed" && file.Previous != "" && !schema && !contract:
			continue
		default:
			if unpatched++; unpatched > deployUnpatchedLimit {
				guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardUnknown,
					Detail: fmt.Sprintf("GitHub gave no patch for more than %d of the files changed since %s, so what it does to tables and machines could not all be read.", deployUnpatchedLimit, installed)})
				return guards, nil
			}
			previous := file.Path
			if file.Previous != "" {
				previous = file.Previous
			}
			before, err := readSide(installed, previous)
			if err != nil {
				return nil, err
			}
			after, err := readSide(sha, file.Path)
			if err != nil {
				return nil, err
			}
			lines = pipelines.DifferingLines(before, after)
		}
		if schema && len(pipelines.SchemaChanges(lines)) > 0 {
			changing = append(changing, file.Path)
		}
		if contract {
			for _, line := range pipelines.ContractChanges(lines) {
				contracts = append(contracts, file.Path+": "+line)
			}
		}
	}
	if len(changing) > 0 {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardData,
			Detail: "It changes tables or data: " + strings.Join(changing, ", ") + "."})
	}
	if len(contracts) > 0 {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardContract,
			Detail: "It changes what machines must accept: " + strings.Join(contracts, "; ") + ". Machines and engine must move together, in the order COMPATIBILITY.md gives. Pressing Deploy alone goes back, because no machine could verify the rollout."})
	}
	for _, file := range files {
		if file.Path != pipelines.ManifestPath {
			continue
		}
		before, err := readSide(installed, pipelines.ManifestPath)
		if err != nil {
			return nil, err
		}
		after, err := readSide(sha, pipelines.ManifestPath)
		if err != nil {
			return nil, err
		}
		was, errWas := manifestStepKeys([]byte(before))
		is, errIs := manifestStepKeys([]byte(after))
		if errWas != nil || errIs != nil {
			guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardStep,
				Detail: "Its pipeline could not be compared with what runs now, so it may have lost a step."})
		} else if removed := pipelines.RemovedPipelineSteps(was, is); len(removed) > 0 {
			guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardStep,
				Detail: "It removes pipeline steps: " + strings.Join(removed, ", ") + ". The pipeline must not be able to switch off its own tests."})
		}
	}
	return guards, nil
}

// manifestStepKeys reads a manifest's <stage>.<step> names.
func manifestStepKeys(manifest []byte) ([]string, error) {
	var doc struct {
		Pipeline struct {
			Stages []struct {
				Name  string `yaml:"name"`
				Steps []struct {
					Name string `yaml:"name"`
				} `yaml:"steps"`
			} `yaml:"stages"`
		} `yaml:"pipeline"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(manifest)).Decode(&doc); err != nil {
		return nil, err
	}
	var keys []string
	for _, stage := range doc.Pipeline.Stages {
		for _, step := range stage.Steps {
			keys = append(keys, stage.Name+"."+step.Name)
		}
	}
	return keys, nil
}
