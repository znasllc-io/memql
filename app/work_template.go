package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type workTemplate struct {
	automation *automations.Automation
	registry   *memql.AuthoredRuntimeRegistry
	members    map[string]*automations.Automation
}

type workTemplateReader interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}

func workTemplateRows(ctx context.Context, engine workTemplateReader, name string, args map[string]any) ([]map[string]any, error) {
	var params []string
	for k, v := range args {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		params = append(params, k+":"+string(b))
	}
	result, err := engine.Execute(ctx, "query "+name+"("+strings.Join(params, ",")+")")
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(result), nil
}

// A compiled draft is durable but private to its run. An agent reconstructs
// it from owned rows; no planner registry or scheduler activation is assumed.
func loadWorkTemplate(ctx context.Context, engine *memql.MemQLEngine, loader *automations.Loader, journal *automations.RunJournal) (*workTemplate, error) {
	if journal.TemplateConstructId == "" {
		a, err := loader.LoadByName(journal.AutomationName)
		if err != nil {
			return nil, err
		}
		if err = automationRunRefusal(a); err != nil {
			return nil, err
		}
		return &workTemplate{automation: a}, nil
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId == "" || ac.UserId != journal.OwnerUserId {
		return nil, fmt.Errorf("work template needs the run owner's actor")
	}
	rows, err := workTemplateRows(ctx, engine, "authoringConstructById", map[string]any{"constructId": journal.TemplateConstructId})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("work template is not readable by this owner")
	}
	headline := rows[0]
	if headline["kind"] != "automation" || headline["name"] != journal.AutomationName {
		return nil, fmt.Errorf("work template identity does not match its run")
	}
	bundleID, _ := headline["bundleId"].(string)
	rows, err = workTemplateRows(ctx, engine, "authoringBundleById", map[string]any{"bundleId": bundleID})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("work template bundle is not readable")
	}
	bundle := rows[0]
	sourceRun, _ := bundle["sourceRunId"].(string)
	if bundle["status"] != "active" {
		if bundle["status"] != "validated" || sourceRun == "" {
			return nil, fmt.Errorf("work template bundle has not passed validation")
		}
		if memql.BareShortId(sourceRun) != memql.BareShortId(journal.RunId) {
			source, err := automations.LoadRunJournal(ctx, engine, sourceRun)
			if err != nil || source.OwnerUserId != journal.OwnerUserId || memql.BareShortId(source.GoalId) != memql.BareShortId(journal.GoalId) || memql.BareShortId(source.TemplateConstructId) != memql.BareShortId(journal.TemplateConstructId) {
				return nil, fmt.Errorf("draft template belongs to another work run")
			}
		}
	}
	rows, err = workTemplateRows(ctx, engine, "authoringConstructsForBundle", map[string]any{"bundleId": bundleID})
	if err != nil {
		return nil, err
	}
	var sources []string
	var closure []memql.SandboxConstruct
	for _, row := range rows {
		if row["status"] == "retired" {
			return nil, fmt.Errorf("work template contains a retired construct")
		}
		kind, _ := row["kind"].(string)
		switch kind {
		case "automation", "query", "mutation", "logic", "spec", "trait", "shape":
		default:
			return nil, fmt.Errorf("work draft kind %q requires separate activation", kind)
		}
		source, _ := row["source"].(string)
		name, _ := row["name"].(string)
		closure = append(closure, memql.SandboxConstruct{Kind: kind, Name: name, Source: source})
		sources = append(sources, source)
	}
	if journal.TemplateVersion != "" && memql.WorkBundleVersion(closure) != journal.TemplateVersion {
		return nil, fmt.Errorf("work template source changed since compilation")
	}
	reg := memql.NewAuthoredRuntimeRegistry()
	// Defined through the engine, whose define lowers the template's queries,
	// specs and traits in its registries exactly as a session define does
	// (memql#5366), so a member that does not lower refuses the template here.
	defined, err := engine.DefineSessionBundle(reg, journal.OwnerUserId, strings.Join(sources, "\n\n"), "")
	if err != nil || !defined.OK {
		return nil, fmt.Errorf("work template failed execution-node validation: %v (%+v)", err, defined.Diagnostics)
	}
	result := &workTemplate{registry: reg, members: map[string]*automations.Automation{}}
	for _, row := range rows {
		if row["kind"] != "automation" {
			continue
		}
		source, _ := row["source"].(string)
		auto, err := loader.CompileSource(source, "")
		if err != nil {
			return nil, err
		}
		if err = automationRunRefusal(auto); err != nil {
			return nil, err
		}
		result.members[auto.Name] = auto
	}
	result.automation = result.members[journal.AutomationName]
	if result.automation == nil {
		return nil, fmt.Errorf("work template closure lacks its entry point")
	}
	if journal.TemplateFingerprint != "" && result.automation.DefinitionFingerprint(id.New()) != journal.TemplateFingerprint {
		return nil, automations.ErrAutomationChanged
	}
	return result, nil
}
