package procedure

// catalog_sections.go -- a succeeded goal run's live sections become catalogued
// automations (epic memql#5414, #5418; design D24: decomposition spends
// intelligence once).
//
// Compile writes every section it plans live as an automation of its own in the
// run's draft bundle, named by the section's signature
// (integrations/planner/work_compile_section_automations.go). This is the half
// that makes that worth doing: when the run SUCCEEDS, each such automation is
// promoted into the owner's catalog under that signature, and compile's exact
// section tier (cataloguedConstructsForGoalSignature) then serves it to the
// next goal whose section asks for the same thing -- instead of planning the
// section live again. The reuse sweep counts every goal that called it by name,
// so two distinct goals make it reusable and the near tier may serve it too.
//
// A COPY, IN A BUNDLE OF ITS OWN -- never the draft's row flipped in place. The
// section tiers serve only an ACTIVE construct, and carry the WHOLE bundle it
// belongs to into the next goal's draft, every member of which must be active
// (work_compile_sections.go catalogClosure). A run's draft bundle holds that
// run's template and every sibling section, all draft and bound to the one run,
// so a section catalogued in place would never be served, and activating the
// draft to make it servable would drag the old goal's template into every later
// goal's draft. So the catalogued construct is the section automation's source,
// alone in a new validated bundle, active, catalogued with the draft's bundle as
// its provenance (catalogedFromBundleId), and given its goalSignature LAST: the
// signature is what a later goal is served by without planning, so it is
// written only onto a construct that is already everything it claims to be.
//
// WHAT IS NOT CATALOGUED, and why each is a rule rather than a preference:
//
//   - a run with steps computed against an upstream version no longer current,
//     or one its owner disliked: its sections are not the answer that stands;
//   - a section whose current version ran with a person's override: the answer
//     that stands came from the override, which the automation does not carry,
//     so cataloguing the automation would serve the behaviour the person
//     stepped in to change;
//   - a section whose current answer its owner disliked, for the same reason;
//   - a section whose signature the owner's catalog already holds: an
//     already-catalogued section writes nothing, which is also what makes a
//     second delivery of the event, and a later goal that was SERVED the
//     section and carried its source in, write nothing;
//   - a section Gate 1 refuses on its own, since it will be carried alone.
//
// THE GATE. @serverOnly is refused on a builtin at parse, so the handler admits
// its caller itself: only the catalogSucceededSections automation, under
// internal origin, naming the owner its completion event carries. That owner is
// borrowed and re-verified by the owner-filtered run read, exactly as
// learnFromRun borrows it; nobody else may catalogue, because a catalogued
// signature is an equivalence a caller must not be able to assert.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/planner"
)

// catalogAutomationActor is the synthetic actor the catalogSucceededSections
// automation runs under (component/automations' system actor prefix and the
// automation's name).
const catalogAutomationActor = "system:automation:catalogSucceededSections"

// sectionCatalogKeyPrefix begins a catalogued section's catalogKey.
const sectionCatalogKeyPrefix = "section:"

// maxSectionTitleRunes bounds a catalogued section's bundle title.
const maxSectionTitleRunes = 120

// SectionCatalogResult is what one pass over a succeeded run catalogued.
type SectionCatalogResult struct {
	RunId string
	// Catalogued are the constructs written into the owner's catalog.
	Catalogued []string
	// Skipped is why the run's sections were not looked at, when they were
	// not.
	Skipped string
	// NotCatalogued are the section automations looked at and left out, each
	// with why.
	NotCatalogued []SectionNotCatalogued
}

// SectionNotCatalogued is one section automation left out of the catalog.
type SectionNotCatalogued struct {
	Automation string
	Reason     string
}

func (r SectionCatalogResult) reply() map[string]any {
	out := map[string]any{
		"runId":      r.RunId,
		"catalogued": append([]string{}, r.Catalogued...),
	}
	if r.Skipped != "" {
		out["skipped"] = r.Skipped
	}
	if len(r.NotCatalogued) > 0 {
		left := make([]any, 0, len(r.NotCatalogued))
		for _, n := range r.NotCatalogued {
			left = append(left, map[string]any{"automation": n.Automation, "reason": n.Reason})
		}
		out["notCatalogued"] = left
	}
	return out
}

func (i *Integration) handleCatalogSections(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	runId := strings.TrimSpace(argString(args, "runId"))
	if runId == "" {
		return nil, fmt.Errorf("procedure.catalogSections: runId is required")
	}
	owner, err := catalogSectionsOwner(ctx, args)
	if err != nil {
		return nil, err
	}
	res, err := i.catalogSections(ownerActor(ctx, owner), owner, runId)
	if err != nil {
		return nil, err
	}
	i.log().Info("procedure: a succeeded run's sections were looked at for the catalog", "runId", runId,
		"catalogued", len(res.Catalogued), "notCatalogued", len(res.NotCatalogued), "skipped", res.Skipped)
	return i.reply(res.reply()), nil
}

// catalogSectionsOwner admits the one caller the handler runs for and answers
// the owner whose authority it borrows. See the file header.
func catalogSectionsOwner(ctx context.Context, args map[string]any) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || !ac.Synthetic || ac.UserId != catalogAutomationActor || !auth.OriginFromContext(ctx).IsInternal() {
		return "", fmt.Errorf("procedure.catalogSections: only the catalogSucceededSections automation catalogues a run's sections -- " +
			"a catalogued section is served to a later goal by its signature without being planned, and no caller may assert that equivalence")
	}
	owner := strings.TrimSpace(argString(args, "ownerUserId"))
	if owner == "" {
		return "", fmt.Errorf("procedure.catalogSections: the run's ownerUserId is required -- every read runs as that person, and a blank one reads zero rows and no error")
	}
	return owner, nil
}

// catalogSections is the handler body, under the owner's borrowed authority.
func (i *Integration) catalogSections(ctx context.Context, owner, runId string) (SectionCatalogResult, error) {
	res := SectionCatalogResult{RunId: runId}
	run, err := i.readRunForCaller(ctx, runId)
	if err != nil {
		return res, err
	}
	if run == nil {
		return res, fmt.Errorf("procedure.catalogSections: run %s is not readable as the owner its event named", runId)
	}
	if !sameUser(str(run, "ownerUserId"), owner) {
		// Belt and braces over the owned read, as learnFromRun has it.
		return res, fmt.Errorf("procedure.catalogSections: run %s belongs to another person", runId)
	}
	switch {
	case str(run, "status") != "succeeded":
		res.Skipped = "the run has not succeeded"
	case strings.TrimSpace(str(run, "templateConstructId")) == "":
		res.Skipped = "the run was not compiled into a draft of its own, so it worked no section out"
	case len(stringList(run["staleSteps"])) > 0:
		res.Skipped = "its head holds steps computed against an upstream version that is no longer current"
	}
	if res.Skipped != "" {
		return res, nil
	}
	observations, err := i.store.query(ctx, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": str(run, "id")}))
	if err != nil {
		return res, err
	}
	fb := readFeedback(observations)
	if fb.runDisliked {
		res.Skipped = "its owner disliked the run"
		return res, nil
	}

	draftBundle, sections, err := i.draftSectionAutomations(ctx, str(run, "templateConstructId"))
	if err != nil {
		return res, err
	}
	if len(sections) == 0 {
		res.Skipped = "its draft wrote no section as an automation of its own"
		return res, nil
	}
	// Every version of the run's steps, to find the version of each section
	// that stands. A read that fails catalogues NOTHING: a section cannot be
	// shown not to have been overridden or disliked, and a section left out
	// is planned live next time, which costs what it always did.
	versions, err := i.store.runStepVersions(ctx, str(run, "id"))
	if err != nil {
		return res, err
	}
	seen := map[string]bool{}
	for _, m := range sections {
		name := str(m, "name")
		constructId, why := i.catalogSection(ctx, run, draftBundle, m, versions, fb, seen)
		if why != "" {
			res.NotCatalogued = append(res.NotCatalogued, SectionNotCatalogued{Automation: name, Reason: why})
			continue
		}
		res.Catalogued = append(res.Catalogued, constructId)
	}
	return res, nil
}

// draftSectionAutomations reads the run's draft -- its template construct and
// that construct's bundle -- and answers the bundle id and every member that
// is a section automation, sorted by name.
func (i *Integration) draftSectionAutomations(ctx context.Context, templateConstructId string) (string, []map[string]any, error) {
	rows, err := i.store.query(ctx, "query "+call("authoringConstructById", map[string]any{"constructId": templateConstructId}))
	if err != nil {
		return "", nil, err
	}
	if len(rows) != 1 {
		return "", nil, fmt.Errorf("procedure.catalogSections: the run's template construct %s is not readable as its owner", templateConstructId)
	}
	bundleId := strings.TrimSpace(str(rows[0], "bundleId"))
	if bundleId == "" {
		return "", nil, fmt.Errorf("procedure.catalogSections: the run's template construct %s names no bundle", templateConstructId)
	}
	members, err := i.store.query(ctx, "query "+call("authoringConstructsForBundle", map[string]any{"bundleId": bundleId}))
	if err != nil {
		return "", nil, err
	}
	var out []map[string]any
	for _, m := range members {
		if str(m, "kind") == "automation" && strings.HasPrefix(str(m, "name"), planner.SectionAutomationPrefix) && str(m, "status") != "retired" {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return str(out[a], "name") < str(out[b], "name") })
	return bundleId, out, nil
}

// catalogSection catalogues one section automation of the run's draft, or says
// why it does not. seen holds the signatures already decided in this run.
func (i *Integration) catalogSection(ctx context.Context, run map[string]any, draftBundle string, m map[string]any, versions []map[string]any, fb feedback, seen map[string]bool) (string, string) {
	name, source := str(m, "name"), str(m, "source")
	sec, err := planner.ReadSectionAutomation(source)
	if err != nil {
		return "", err.Error()
	}
	if sec.Name != name {
		return "", fmt.Sprintf("its row is named %s and its source declares %s", name, sec.Name)
	}
	if seen[sec.Signature] {
		return "", "another section automation of the run answers the same section"
	}
	seen[sec.Signature] = true

	v := currentAutomationStep(versions, name)
	switch {
	case v == nil:
		return "", "no step of the run called it"
	case str(v, "status") != "done":
		return "", "the step that called it did not finish"
	case len(obj(v, "override")) > 0:
		return "", "its current version ran with a person's override, which the automation does not carry"
	case newestVerdictOn(fb.steps[str(v, "key")], intOf(v, "version")) == work.VerdictDislike:
		return "", "its owner disliked the section's current answer"
	}

	existing, err := i.store.query(ctx, "query "+call("cataloguedConstructsForGoalSignature", map[string]any{"goalSignature": sec.Signature}))
	if err != nil {
		return "", "the owner's catalog could not be read: " + err.Error()
	}
	for _, e := range existing {
		if str(e, "goalSignature") == sec.Signature {
			return "", "the owner's catalog already holds this section, as " + str(e, "id")
		}
	}

	report, ran := i.compile(source, name)
	switch {
	case !ran:
		return "", "no compile gate is installed on the node that saw the run succeed, so its source was never checked on its own"
	case !report.OK:
		return "", "its source did not pass the compile gate on its own: " + firstFailure(report)
	case !compiledAutomation(report, name):
		return "", "the compile gate skipped it, so its source was never checked on its own"
	}

	constructId, err := i.writeCataloguedSection(ctx, run, draftBundle, m, sec)
	if err != nil {
		i.log().Warn("procedure: a section could not be written into the catalog", "runId", str(run, "id"), "automation", name, "error", err)
		return "", "writing it into the catalog failed: " + err.Error()
	}
	return constructId, ""
}

// writeCataloguedSection writes the section automation into the owner's catalog
// as a construct of its own (see the file header): the bundle, the construct,
// Gate 1's verdict, the active status and the catalogue stamp -- and the
// signature last.
func (i *Integration) writeCataloguedSection(ctx context.Context, run map[string]any, draftBundle string, m map[string]any, sec planner.SectionAutomation) (string, error) {
	bundleId, constructId := id.NewShortId(), id.NewShortId()
	namespace := strings.TrimSpace(str(m, "targetNamespace"))
	if namespace == "" {
		namespace = "authored"
	}
	writes := []struct {
		name string
		args map[string]any
	}{
		{"createAuthoringBundle", map[string]any{
			"bundleId": bundleId,
			"title":    truncateRunes("Section: "+sec.Purpose, maxSectionTitleRunes),
			"summary":  "A section of a decomposed goal, worked out live in run " + str(run, "id") + " and catalogued for the next goal that asks for it: " + sec.Purpose,
		}},
		{"createAuthoringConstruct", map[string]any{
			"constructId":     constructId,
			"bundleId":        bundleId,
			"kind":            "automation",
			"name":            sec.Name,
			"targetNamespace": namespace,
			"source":          str(m, "source"),
		}},
		{"recordBundleValidation", map[string]any{
			"bundleId": bundleId,
			"status":   "validated",
			"validationReport": map[string]any{
				"ok": true, "gate1Ran": true, "catalogedFromRunId": str(run, "id"),
			},
		}},
		{"setConstructStatus", map[string]any{"constructId": constructId, "status": "active"}},
		{"catalogueConstruct", map[string]any{
			"constructId":      constructId,
			"catalogKey":       sectionCatalogKeyPrefix + sec.Signature,
			"catalogMatchText": "kind:automation intent:" + sec.Purpose,
			"fromBundleId":     draftBundle,
		}},
		// LAST: the key a later goal is served by without planning.
		{"recordConstructGoalSignature", map[string]any{"constructId": constructId, "goalSignature": sec.Signature}},
	}
	for _, w := range writes {
		if err := i.store.writeInternal(ctx, "mutation "+call(w.name, w.args)); err != nil {
			return "", err
		}
	}
	i.log().Info("procedure: a section worked out live was catalogued", "runId", str(run, "id"),
		"automation", sec.Name, "constructId", constructId, "goalSignature", sec.Signature)
	return "v1:authoring:construct:" + constructId, nil
}

// currentAutomationStep is the version that stands of the top-level step that
// called the named automation: the one workStepVersions marks current. A
// version the read did not mark either way is not taken for current, because
// a section judged on a version that does not stand would be judged on
// somebody else's answer.
func currentAutomationStep(versions []map[string]any, automation string) map[string]any {
	for _, v := range versions {
		key := str(v, "key")
		if key == "" || strings.Contains(key, "/") || str(v, "stepType") != "automation" {
			continue
		}
		if strings.TrimSpace(str(obj(v, "call"), "name")) != automation {
			continue
		}
		if current, _ := v["current"].(bool); current {
			return v
		}
	}
	return nil
}
