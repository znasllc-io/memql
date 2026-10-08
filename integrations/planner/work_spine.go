package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

// All mutable state belongs to ONE compile. Handles returned to DSL are keys
// into this scope's evidence, never a construct ID or an authority token.
// workflowhost serializes native operations even in a parallel DSL branch.
type spineScope struct {
	loop            *PlannerAgentLoop
	req             CompileRequest
	near            authoringNearMatcher
	sandbox         authoringSandbox
	out             CompileOutcome
	keys            []string
	conversation    any
	conversational  bool
	candidates      map[string]work.CatalogCandidate
	candidateRoutes map[string]work.Route
	reads           map[string][]any
	classified      bool
	prepared        bool
	acknowledged    bool
	designed        bool
	emitted         bool
	validated       bool
	done            bool
	diagnosticKey   string
	sameDiagnostics int
	failed          error
	decision        sectionableDecision
	guidance        []map[string]any
	plan            designPlan
	bundle          authoringBundle
	report          memql.SandboxReport
	repairs         int
	seen            map[string]bool
	draftModes      map[string]bool
}

func (l *PlannerAgentLoop) newSpineScope(req CompileRequest, near authoringNearMatcher, sandbox authoringSandbox) (*spineScope, error) {
	if strings.TrimSpace(req.Statement) == "" {
		return nil, fmt.Errorf("work compile: goal %s has an empty statement", req.GoalId)
	}
	keys := inputKeys(req.Input)
	sig := work.GoalSignature(req.Statement, keys)
	conversation, conversational := req.Input["conversation"]
	if conversational {
		// A follow-up has no reusable meaning without its transcript. Keep it out
		// of both text-only catalogue tiers, including learned procedures.
		raw, err := json.Marshal(conversation)
		if err != nil {
			return nil, fmt.Errorf("work compile: invalid conversation: %w", err)
		}
		sig = work.GoalSignature(req.Statement+"\nconversation:"+string(id.NewUntracked().FromBytes(raw)), keys)
	}
	out := CompileOutcome{Signature: sig}
	scope := &spineScope{req: req, near: near, sandbox: sandbox, out: out, keys: keys, conversation: conversation, conversational: conversational,
		candidates: map[string]work.CatalogCandidate{}, candidateRoutes: map[string]work.Route{}, reads: map[string][]any{}, seen: map[string]bool{}, draftModes: map[string]bool{}}
	scope.loop = &PlannerAgentLoop{logger: l.logger, engine: &spineMeteredEngine{Engine: l.engine, scope: scope}}
	return scope, nil
}

func (s *spineScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"spineContext": func(context.Context, map[string]any) (any, error) {
			return map[string]any{"statement": s.req.Statement, "input": s.req.Input, "conversational": s.conversational, "repairAttempts": spineRepairAttempts()}, nil
		},
		"spineCandidates":      s.findCandidates,
		"spineUseCandidate":    s.useCandidate,
		"spineClassify":        s.classify,
		"spineAcknowledge":     s.acknowledge,
		"spinePrepareSections": s.prepareSections,
		"spineDraft":           s.draft,
		"spineDesign":          s.design,
		"spineEmit":            s.emit,
		"spineValidate":        s.validate,
		"spineRepair":          s.repair,
		"spinePersist":         s.persist,
		"spineRefuse": func(_ context.Context, args map[string]any) (any, error) {
			return nil, fmt.Errorf("Spine refused: %s", getString(args, "message"))
		},
	}
	for name, op := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if s.failed != nil {
				return nil, s.failed
			}
			if s.done {
				s.failed = fmt.Errorf("Spine cannot call %s after selecting a plan", name)
				return nil, s.failed
			}
			if err := ctx.Err(); err != nil {
				s.failed = err
				return nil, err
			}
			value, err := op(ctx, args)
			if err != nil {
				s.failed = err
			}
			return value, err
		}
	}
	return ops
}

func (s *spineScope) modelAllowed() error {
	if s.req.MaxModelCalls > 0 && s.out.ModelCalls >= s.req.MaxModelCalls {
		return fmt.Errorf("work compile: this run's maxModelCalls ceiling is exhausted")
	}
	return nil
}

func (s *spineScope) findCandidates(ctx context.Context, args map[string]any) (any, error) {
	kind := getString(args, "kind")
	if kind != "exact" && kind != "procedure" && kind != "near" {
		return nil, fmt.Errorf("invalid Spine candidate kind %q", kind)
	}
	if rows, ok := s.reads[kind]; ok {
		return map[string]any{"ok": true, "candidates": rows, "message": ""}, nil
	}
	rows := []any{}
	var candidates []work.CatalogCandidate
	var err error
	// Conversation signatures cannot be served from text-only evidence, even
	// if a custom workflow asks for it. This is a data-integrity constraint.
	if !s.conversational {
		switch kind {
		case "exact":
			candidates, err = s.loop.cataloguedForSignature(ctx, s.req.OwnerUserId, s.out.Signature)
		case "procedure":
			candidates, err = s.loop.servableProceduresForSignature(ctx, s.req.OwnerUserId, s.out.Signature)
		case "near":
			if s.near != nil {
				found, e := s.near.CatalogNearMatches(ctx, work.NormalizeStatement(s.req.Statement), maxNearMatchCandidates)
				err = e
				if err == nil {
					candidates = nearCandidates(found, s.keys)
				}
			}
		}
	}
	if err != nil {
		// A read miss is explicit evidence; no external effect has been attempted.
		// The workflow chooses whether that evidence permits a paid fallback.
		s.loop.warnCompile("work compile: candidate read failed", s.req, err)
		return map[string]any{"ok": false, "candidates": rows, "message": err.Error()}, nil
	}
	for n, c := range candidates {
		handle := fmt.Sprintf("%s:%d", kind, n)
		s.candidates[handle] = c
		route := work.RouteCatalogExact
		if kind == "near" {
			route = work.RouteCatalogNear
		}
		s.candidateRoutes[handle] = route
		rows = append(rows, map[string]any{"handle": handle, "name": c.Name, "similarity": c.Similarity, "missingArgs": c.MissingArgs})
	}
	// Cache detached public evidence; the native candidate remains private.
	s.reads[kind] = rows
	return map[string]any{"ok": true, "candidates": rows, "message": ""}, nil
}

func (s *spineScope) useCandidate(_ context.Context, args map[string]any) (any, error) {
	handle := getString(args, "handle")
	c, ok := s.candidates[handle]
	if !ok {
		return nil, fmt.Errorf("Spine candidate was not read in this scope")
	}
	s.out.Route = s.candidateRoutes[handle]
	s.out.ConstructId, s.out.AutomationName = c.ConstructId, c.Name
	if s.out.Route == work.RouteCatalogNear {
		s.out.Gaps = append([]string(nil), c.MissingArgs...)
	}
	if s.out.Route == work.RouteCatalogExact && c.Rung != work.RungNone {
		s.out.AutomationName = replayProcedureAutomation
		s.out.ConstructId = ""
		s.out.Variables = map[string]any{work.ProcedureConstructVariable: c.ConstructId}
	}
	s.done = true
	return true, nil
}

func (s *spineScope) classify(ctx context.Context, _ map[string]any) (any, error) {
	if s.classified {
		return nil, fmt.Errorf("Spine may classify only once")
	}
	s.classified = true
	if err := s.modelAllowed(); err != nil {
		return nil, err
	}
	l, req, out := s.loop, s.req, &s.out
	sig, keys, conversation, conversational := out.Signature, s.keys, s.conversation, s.conversational
	guidance := l.descriptionGuidance(ctx, req, sig)
	if conversational {
		ctx = l.withAcknowledgementCandidate(ctx, req)
	}

	// Tier 3: ONE classifier call answering complexity AND sectionability.
	triageCtx, cancelTriage := context.WithTimeout(airoute.WithCallPurpose(ctx, "Understanding request", 0), 60*time.Second)
	complexity, _, sectionable, cerr := l.classifyGoal(triageCtx, req.Statement, time.Now().UTC().Format(time.RFC3339), guidance, keys, conversation)
	cancelTriage()

	if cerr != nil {
		return nil, fmt.Errorf("work compile: intent classification failed; retry the request: %w", cerr)
	}
	if complexity == complexityUnknown {
		return nil, fmt.Errorf("work compile: intent classification returned no valid complexity; retry the request")
	}
	if conversational && sectionable.Intent != "reply" && sectionable.Intent != "task" && sectionable.Intent != "automation" {
		return nil, fmt.Errorf("work compile: intent classification omitted a valid intent; retry the request")
	}
	if conversational && sectionable.RequiresFile == nil {
		return nil, fmt.Errorf("work compile: intent classification omitted the delivery contract; retry the request")
	}

	out.Workload, out.WorkTitle = sectionable.Workload, strings.TrimSpace(sectionable.WorkTitle)
	ack := strings.TrimSpace(sectionable.Acknowledgement)
	if validAcknowledgement(ack) {
		out.Acknowledgement = ack
	}
	switch out.Workload {
	case "quick", "lookup", "research", "project":
	case "":
		// Older authored classifiers remain valid; complexity supplies a
		// conservative estimate without manufacturing another model call.
		out.Workload = "research"
		if sectionable.Intent == "reply" && complexity == complexityTrivial {
			out.Workload = "quick"
		}
	default:
		return nil, fmt.Errorf("work compile: invalid workload classification")
	}
	if len([]rune(out.WorkTitle)) > 80 {
		out.WorkTitle = string([]rune(out.WorkTitle)[:80])
	}
	if conversational && sectionable.Intent == "reply" {
		if sectionable.Sectionable || *sectionable.RequiresFile || sectionable.Navigation != nil {
			return nil, fmt.Errorf("work compile: conflicting reply delivery contract; retry the request")
		}
		out.Reply = out.Workload == "quick"
	}
	s.guidance, s.decision = guidance, sectionable
	return map[string]any{"complexity": string(complexity), "intent": sectionable.Intent, "sectionable": sectionable.Sectionable, "conversational": conversational, "workload": out.Workload, "acknowledgement": out.Acknowledgement}, nil
}

func (s *spineScope) acknowledge(ctx context.Context, _ map[string]any) (any, error) {
	if !s.classified || s.acknowledged {
		return nil, fmt.Errorf("Spine acknowledgement requires one classification and permits one repair")
	}
	s.acknowledged = true
	if err := s.modelAllowed(); err != nil {
		return false, nil
	}
	ackOutcome := s.out
	s.out.Acknowledgement = s.loop.repairAcknowledgement(ctx, s.req, s.conversation, &ackOutcome)
	return s.out.Acknowledgement != "", nil
}

func (s *spineScope) prepareSections(ctx context.Context, args map[string]any) (any, error) {
	if !s.classified || s.prepared {
		return nil, fmt.Errorf("Spine sections require one classification and may be prepared once")
	}
	route := work.Route(getString(args, "route"))
	if route != work.RouteTrivial && route != work.RouteSectionable {
		return nil, fmt.Errorf("invalid Spine draft route")
	}
	s.prepared = true
	d, dec := s.loop.decideDecomposition(ctx, s.req, work.Decision{Route: route}, s.decision, &s.out)
	s.decision = dec
	s.out.Route = route
	return map[string]any{"valid": d.Route != work.RouteAuthor, "reason": s.out.DecompositionRefused}, nil
}

func (s *spineScope) draft(ctx context.Context, args map[string]any) (any, error) {
	if !s.classified || !s.prepared || s.sandbox == nil {
		return nil, fmt.Errorf("work compile: runnable draft requires classification, section validation and Gate 1 sandbox")
	}
	mode := getString(args, "mode")
	if mode != "default" && mode != "live" && mode != "inline" {
		return nil, fmt.Errorf("invalid Spine draft mode")
	}
	if s.draftModes[mode] {
		return nil, fmt.Errorf("Spine draft mode may be attempted once")
	}
	s.draftModes[mode] = true
	if mode == "live" {
		s.decision = s.decision.withoutCatalog()
		for n := range s.out.Sections {
			s.out.Sections[n].Route, s.out.Sections[n].Candidate, s.out.Sections[n].Similarity = work.SectionIntelligence, nil, 0
		}
	}
	if mode == "inline" {
		s.decision.inlineAll = "the draft that wrote it as an automation of its own did not persist; the Spine selected inline live sections"
	}
	if s.out.DecompositionRefused != "" {
		// The workflow chose to draft the whole goal after the boundary check
		// cleared its invalid sections. Report the plan we actually build.
		s.out.Route = work.RouteTrivial
	}
	out, err := s.loop.reasoningDraft(ctx, s.req, s.out, s.sandbox, s.decision)
	if err == nil {
		s.out = out
		s.done = true
		return map[string]any{"ok": true, "message": "", "hasCatalog": false, "hasLiveSections": false}, nil
	}
	var persistence *draftPersistenceError
	if errors.As(err, &persistence) {
		return nil, err
	}
	// Only failures before the first write permit a different representation.
	return map[string]any{"ok": false, "message": err.Error(), "hasCatalog": s.decision.catalog != nil, "hasLiveSections": s.decision.cutsSectionAutomations(s.req)}, nil
}

func (s *spineScope) design(ctx context.Context, _ map[string]any) (any, error) {
	if !s.classified || s.designed || s.sandbox == nil {
		return nil, fmt.Errorf("Spine design requires classification and a sandbox, and may run once")
	}
	if s.conversational && s.decision.Intent != "automation" {
		return nil, fmt.Errorf("work compile: conversational source authoring requires automation intent")
	}
	if err := s.modelAllowed(); err != nil {
		return nil, err
	}
	s.designed = true
	var err error
	s.plan, err = s.loop.runDesignPassGuided(airoute.WithCallPurpose(ctx, "Designing automation", 0), compileStatement(s.req), s.req.OwnerUserId, nil, s.guidance)
	s.out.Route = work.RouteAuthor
	return err == nil, err
}

func (s *spineScope) emit(ctx context.Context, _ map[string]any) (any, error) {
	if !s.designed || s.emitted {
		return nil, fmt.Errorf("Spine emission requires a design and may run once")
	}
	if err := s.modelAllowed(); err != nil {
		return nil, err
	}
	s.emitted = true
	var err error
	s.bundle, err = s.loop.emitBundle(airoute.WithCallPurpose(ctx, "Writing automation", 0), compileStatement(s.req), s.plan)
	if err != nil {
		return nil, err
	}
	s.seen[bundleFingerprint(s.bundle.Constructs)] = true
	return true, nil
}

func (s *spineScope) validate(_ context.Context, _ map[string]any) (any, error) {
	if !s.emitted || s.sandbox == nil {
		return nil, fmt.Errorf("Spine validation requires an emitted bundle and sandbox")
	}
	if !s.validated {
		s.report = s.sandbox.CompileBundle(s.bundle.Constructs)
		key := fmt.Sprint(failingDiagnostics(s.report))
		if key == s.diagnosticKey {
			s.sameDiagnostics++
		} else {
			s.sameDiagnostics = 0
		}
		s.diagnosticKey = key
		s.validated = true
	}
	return map[string]any{"ok": s.report.OK, "message": fmt.Sprint(failingDiagnostics(s.report))}, nil
}

func (s *spineScope) repair(ctx context.Context, _ map[string]any) (any, error) {
	if !s.validated || s.report.OK || s.repairs >= spineRepairCap() || s.sameDiagnostics >= 2 {
		return nil, fmt.Errorf("Spine repair requires failed validation and remaining repair budget")
	}
	if err := s.modelAllowed(); err != nil {
		return nil, err
	}
	failing := failingConstructs(s.bundle.Constructs, s.report)
	if len(failing) == 0 {
		return nil, fmt.Errorf("Spine validation has no repairable diagnostics")
	}
	s.repairs++
	repaired, err := s.loop.repairConstructs(airoute.WithCallPurpose(ctx, "Repairing automation", s.repairs), compileStatement(s.req), s.bundle, failing, s.report)
	if err != nil {
		return nil, err
	}
	if err = validateRepair(s.bundle.Constructs, failing, repaired); err != nil {
		return nil, err
	}
	candidate := mergeRepaired(s.bundle.Constructs, repaired)
	fingerprint := bundleFingerprint(candidate)
	if s.seen[fingerprint] {
		return nil, fmt.Errorf("Spine repair stopped: repeated source")
	}
	s.seen[fingerprint] = true
	s.bundle.Constructs = candidate
	s.validated = false
	return true, nil
}

func (s *spineScope) persist(ctx context.Context, _ map[string]any) (any, error) {
	if !s.validated || !s.report.OK {
		return nil, fmt.Errorf("Spine cannot persist a bundle that did not pass Gate 1")
	}
	out, err := s.loop.persistWorkDraft(ctx, s.req, s.out, s.bundle, s.sandbox)
	if err != nil {
		return nil, err
	}
	s.out = out
	s.done = true
	return true, nil
}

// Once a write is attempted, its failure may mean an uncertain effect. A
// different draft must not be retried as if validation alone had failed.
type draftPersistenceError struct{ err error }

func (e *draftPersistenceError) Error() string { return e.err.Error() }
func (e *draftPersistenceError) Unwrap() error { return e.err }

func spineRepairAttempts() []any {
	out := make([]any, spineRepairCap())
	for n := range out {
		out[n] = n + 1
	}
	return out
}

// The installed workflow may choose fewer attempts, never an unbounded repair loop.
func spineRepairCap() int { return min(repairAttemptCap(), 32) }

// Meter the model boundary, including every phase hidden within a source
// emission. Counting only the outer stage would let a multi-phase design
// spend the run's entire ceiling several times over with a fake/in-process
// engine; the production provider guard remains an independent backstop.
type spineMeteredEngine struct {
	Engine
	scope *spineScope
}

func (e *spineMeteredEngine) InvokeAI(ctx context.Context, name string, data map[string]any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.scope.modelAllowed(); err != nil {
		return nil, err
	}
	result, err := e.Engine.InvokeAI(ctx, name, data)
	if err == nil || !memql.IsProviderUnavailable(err) {
		e.scope.out.ModelCalls++
	}
	return result, err
}
func (e *spineMeteredEngine) InvokeAIChatWithFilteredTools(context.Context, string, map[string]any, []string) (string, error) {
	return "", fmt.Errorf("Spine compilation cannot open an unmetered tool loop")
}
