package campaigns

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
)

// template_from_layout_test.go -- campaignCreateTemplateFromLayout, and the
// composition a template carries.

func (tw *templateWorld) fromLayout(layoutID string) (savedTemplate, error) {
	tw.t.Helper()
	nodes, err := tw.w.handleCreateTemplateFromLayout(importCtx(), map[string]any{
		"accountId": templateOrg, "name": "Spring news", "layoutId": layoutID,
	}, 0)
	if err != nil {
		return savedTemplate{}, err
	}
	return decodeSaved(tw.t, nodes), nil
}

// expectedLayoutDocument is the layout as a template made from it starts.
func expectedLayoutDocument(t *testing.T, id string, kit *emaildoc.BrandKit) map[string]any {
	t.Helper()
	info, ok := emaildoc.LayoutMeta(id)
	layout, built := emaildoc.Layout(id)
	if !ok || !built {
		t.Fatalf("unknown layout %q", id)
	}
	doc, err := emaildoc.ExpandToMinimum(layout, info)
	if err != nil {
		t.Fatal(err)
	}
	if kit != nil {
		if doc, err = emaildoc.ApplyBrand(doc, *kit); err != nil {
			t.Fatal(err)
		}
	}
	object, err := documentObject(doc)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestCreateTemplateFromLayoutAppliesTheBrandKit(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.brandKit = storedBrandRow(t, testBrandKit(t))
	kit, _, ok, err := tw.w.brandKitFor(importCtx(), templateOrg)
	if !ok || err != nil {
		t.Fatalf("brandKitFor: ok=%v err=%v", ok, err)
	}

	receipt, err := tw.fromLayout("builtin:article")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.TemplateID == "" || receipt.Status != "draft" || !receipt.Saved || receipt.Revision != templateNow.Format(time.RFC3339Nano) {
		t.Errorf("receipt = %+v", receipt)
	}
	if string(receipt.raw["issues"]) == "null" {
		t.Error("issues is null")
	}
	writes := tw.engine.writes()
	if len(writes) != 1 || !writes[0].internal {
		t.Fatalf("writes = %v", writes)
	}
	requireParses(t, writes[0].query)
	name, args := parseRenderedCall(writes[0].query)
	if name != "createTemplate" || args["templateId"] != receipt.TemplateID || args["name"] != "Spring news" ||
		args["accountId"] != templateOrg || args["layoutId"] != "builtin:article" {
		t.Errorf("write = %s %v", name, args)
	}
	if !sameJSON(t, args["document"], expectedLayoutDocument(t, "builtin:article", &kit)) {
		t.Error("the stored document is not the layout with the brand applied")
	}
	layout, _ := emaildoc.Layout("builtin:article")
	if args["subject"] != layout.Subject || layout.Subject == "" {
		t.Errorf("subject = %v, want the layout's example subject %q", args["subject"], layout.Subject)
	}
	if _, set := args["composition"]; set {
		t.Error("a plain template carries a composition")
	}
}

func TestCreateTemplateFromLayoutWithoutKitUsesNeutralLook(t *testing.T) {
	tw := newTemplateWorld(t)
	// Steps starts with two steps (the minimum), whatever the brand.
	if _, err := tw.fromLayout("builtin:steps"); err != nil {
		t.Fatal(err)
	}
	_, args := parseRenderedCall(tw.engine.writes()[0].query)
	if !sameJSON(t, args["document"], expectedLayoutDocument(t, "builtin:steps", nil)) {
		t.Error("without a kit the template is not the expanded neutral layout")
	}
}

func TestCreateTemplateFromLayoutRefusesUnknownLayout(t *testing.T) {
	tw := newTemplateWorld(t)
	for _, id := range []string{"", "builtin:nope", "org:abc"} {
		_, err := tw.fromLayout(id)
		if !errors.Is(err, errUnknownLayout) || err.Error() != "choose one of the built-in layouts" {
			t.Errorf("layout %q: err = %v", id, err)
		}
	}
	if len(tw.engine.writes()) != 0 {
		t.Error("a refused layout wrote something")
	}
}

func TestCreateTemplateFromLayoutRefusesWithoutWritePermission(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.capable = func(string, string, string) bool { return false }
	_, err := tw.fromLayout("builtin:article")
	if !errors.Is(err, errTemplateNeedsWrite) {
		t.Fatalf("err = %v", err)
	}
	if len(tw.engine.writes()) != 0 {
		t.Error("a refused caller wrote something")
	}
	tw = newTemplateWorld(t)
	for _, name := range []string{"", strings.Repeat("x", 201), "a\nb"} {
		_, err := tw.w.handleCreateTemplateFromLayout(importCtx(), map[string]any{"accountId": templateOrg, "name": name, "layoutId": "builtin:article"}, 0)
		if !errors.Is(err, errChooseTemplate) {
			t.Errorf("name %q: err = %v", name, err)
		}
	}
}

func sampleComposition() composition {
	return composition{
		GoalID: "goal-1", State: "writing", Error: "",
		StartedAt:    time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		BaseRevision: time.Date(2029, 1, 1, 0, 0, 1, 500, time.UTC),
		Request: compositionRequest{
			LayoutID: "builtin:article", Name: "Spring news", Description: "Tell them about spring.",
			FileIDs: []string{"f1", "f2"}, ReferenceFileID: "f3",
		},
		Notes: []string{"menu.xlsx was left out because it holds no readable text."},
	}
}

func TestCompositionRoundTripsThroughTheTemplateRow(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	want := sampleComposition()
	id, _, _, err := tw.w.createTemplateFromLayout(importCtx(), templateOrg, "Spring news", "builtin:article", &want)
	if err != nil || id == "" {
		t.Fatalf("createTemplateFromLayout: %q %v", id, err)
	}
	_, args := parseRenderedCall(tw.engine.writes()[0].query)
	if _, ok := args["composition"].(map[string]any); !ok {
		t.Fatalf("composition not written: %v", args["composition"])
	}
	got, ok := compositionFromRow(tw.engine.fakeEngine.template)
	if !ok {
		t.Fatal("the row has no composition")
	}
	if !sameJSON(t, got, want) {
		t.Errorf("composition = %+v, want %+v", got, want)
	}
	if _, ok := compositionFromRow(map[string]any{"name": "x"}); ok {
		t.Error("a row without composition reports one")
	}
}

func TestWriteCompositionKeepsTheContent(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	start := sampleComposition()
	id, first, _, err := tw.w.createTemplateFromLayout(importCtx(), templateOrg, "Spring news", "builtin:article", &start)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]any{}
	for k, v := range tw.engine.fakeEngine.template {
		before[k] = v
	}

	done := start
	done.State, done.FinishedAt = "done", templateNow.Add(time.Minute)
	revision, err := tw.w.writeComposition(importCtx(), id, done)
	if err != nil {
		t.Fatal(err)
	}
	if !revision.After(first) {
		t.Errorf("revision %v is not after %v", revision, first)
	}
	after := tw.engine.fakeEngine.template
	for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "sourceArtifactId", "document", "status", "accountId"} {
		if !sameJSON(t, before[field], after[field]) {
			t.Errorf("%s changed", field)
		}
	}
	got, _ := compositionFromRow(after)
	if got.State != "done" || !got.FinishedAt.Equal(done.FinishedAt) {
		t.Errorf("composition = %+v", got)
	}
	var held bool
	for _, key := range tw.gate.keys {
		held = held || key == id
	}
	if !held {
		t.Errorf("the template lock was not taken: %v", tw.gate.keys)
	}
	if _, err := tw.w.writeComposition(importCtx(), "missing", done); err == nil {
		t.Error("a missing template was written")
	}
}
