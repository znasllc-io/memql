package pipelinerun

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// deploy_guards.go -- what makes a release wait for the owner's Deploy button (design
// record docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md, section
// 7, step 1). Each is read between what the installation runs and the release, because
// a release held back at one of these is still in the range of the next.

// migrationDirectories is where migrations live. They are read as listings at both ends
// rather than from a comparison, which stops listing at 300 files.
var migrationDirectories = []string{"component/database", "component/database/memory-nodes/migrations"}

func (i *Integration) deployGuards(ctx context.Context, gh trainGitHub, token string, p Pipeline, installed, sha string) ([]pipelines.DeployGuard, error) {
	guards := []pipelines.DeployGuard{}
	var changing []string
	for _, dir := range migrationDirectories {
		before, err := gh.DirectoryFiles(ctx, token, p.Repository, installed, dir)
		if err != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", dir, installed, err)
		}
		after, err := gh.DirectoryFiles(ctx, token, p.Repository, sha, dir)
		if err != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", dir, shortCommit(sha), err)
		}
		for _, name := range after {
			path := dir + "/" + name
			if slices.Contains(before, name) || !pipelines.MigrationPath(path) {
				continue
			}
			source, err := gh.FileAtRef(ctx, token, p.Repository, sha, path)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", path, err)
			}
			if pipelines.ClassifyMigrationFile(path, string(source)) == pipelines.MigrationData {
				changing = append(changing, name)
			}
		}
	}
	if len(changing) > 0 {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardData,
			Detail: "It changes tables or data: " + strings.Join(changing, ", ") + "."})
	}

	beforeContract, err := gh.FileAtRef(ctx, token, p.Repository, installed, pipelines.WorkerContractPath)
	if err != nil {
		return nil, fmt.Errorf("reading the worker contract at %s: %w", installed, err)
	}
	afterContract, err := gh.FileAtRef(ctx, token, p.Repository, sha, pipelines.WorkerContractPath)
	if err != nil {
		return nil, fmt.Errorf("reading the worker contract at %s: %w", shortCommit(sha), err)
	}
	if changed := pipelines.ChangedWorkerContracts(string(beforeContract), string(afterContract)); len(changed) > 0 {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardContract,
			Detail: "It changes what machines must accept: " + strings.Join(changed, "; ") + "."})
	}

	beforeManifest, err := gh.FileAtRef(ctx, token, p.Repository, installed, pipelines.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading the pipeline at %s: %w", installed, err)
	}
	afterManifest, err := gh.FileAtRef(ctx, token, p.Repository, sha, pipelines.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading the pipeline at %s: %w", shortCommit(sha), err)
	}
	was, errWas := manifestStepKeys(beforeManifest)
	is, errIs := manifestStepKeys(afterManifest)
	if errWas != nil || errIs != nil {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardStep,
			Detail: "Its pipeline could not be compared with what runs now, so it may have lost a step."})
	} else if removed := pipelines.RemovedPipelineSteps(was, is); len(removed) > 0 {
		guards = append(guards, pipelines.DeployGuard{Code: pipelines.DeployGuardStep,
			Detail: "It removes pipeline steps: " + strings.Join(removed, ", ") + ". The pipeline must not be able to switch off its own tests."})
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
