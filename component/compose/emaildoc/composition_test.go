package emaildoc

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func repeatLayout(t *testing.T) (Document, LayoutInfo) {
	t.Helper()
	d, err := Parse([]byte(layoutJSON(`
{"id":"logo","type":"image","role":"brand","alt":"Logo","width":200,"height":50},
{"id":"intro","type":"text","role":"content","text":"Welcome."},
{"id":"note","type":"highlight","role":"content","text":"A note."},
{"id":"step","type":"section","role":"content","blocks":[
  {"id":"step-title","type":"heading","text":"Step"},
  {"id":"step-body","type":"text","text":"Do this."}]},
{"id":"legal","type":"text","role":"brand","text":"[Unsubscribe]({{unsubscribeUrl}})"}`)))
	if err != nil {
		t.Fatal(err)
	}
	return d, LayoutInfo{ID: "test:steps", Optional: []string{"note", "step"}, Repeat: map[string]RepeatRange{"step": {Min: 1, Max: 3}}}
}

func slotIDs(slots []Slot) []string {
	var ids []string
	for _, s := range slots {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestComposeDropsOnlyOptionalBlocks(t *testing.T) {
	layout, info := repeatLayout(t)
	out, err := Compose(layout, info, Composition{Subject: "S", Drop: []string{"note"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasID(out.Blocks, "note") {
		t.Fatal("note was not dropped")
	}
	if !hasID(layout.Blocks, "note") {
		t.Fatal("the layout was changed")
	}
	_, err = Compose(layout, info, Composition{Subject: "S", Drop: []string{"intro"}}, nil)
	if err == nil || err.Error() != "the draft dropped a block this layout needs: intro" {
		t.Fatalf("got %v", err)
	}
	_, err = Compose(layout, info, Composition{Subject: "S", Drop: []string{"step"}, Repeat: map[string]int{"step": 2}}, nil)
	if err == nil || err.Error() != "the draft both dropped and repeated step" {
		t.Fatalf("got %v", err)
	}
}

func TestComposeRepeatsWithinRangeAndSuffixesDescendants(t *testing.T) {
	layout, info := repeatLayout(t)
	title := "Second"
	out, err := Compose(layout, info, Composition{
		Subject: "S", Repeat: map[string]int{"step": 3},
		Content: []ContentItem{{ID: "step-title-2", Text: &title}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, b := range out.Blocks {
		order = append(order, b.ID)
	}
	if got := strings.Join(order, ","); got != "logo,intro,note,step,step-2,step-3,legal" {
		t.Fatalf("order %s", got)
	}
	if b := blockByID(t, out, "step-title-2"); b.Text != "Second" {
		t.Fatalf("got %q", b.Text)
	}
	blockByID(t, out, "step-body-3")
	if b := blockByID(t, out, "step-title"); b.Text != "Step" {
		t.Fatalf("original changed: %q", b.Text)
	}
}

func TestComposeRefusesRepeatOutsideRange(t *testing.T) {
	layout, info := repeatLayout(t)
	for _, c := range []struct {
		id   string
		n    int
		want string
	}{
		{"step", 4, "the draft repeated step 4 times; this layout allows 1 to 3"},
		{"step", 0, "the draft repeated step 0 times; this layout allows 1 to 3"},
	} {
		_, err := Compose(layout, info, Composition{Subject: "S", Repeat: map[string]int{c.id: c.n}}, nil)
		if err == nil || err.Error() != c.want {
			t.Errorf("got %v, want %s", err, c.want)
		}
	}
	for _, n := range []int{1, 2} {
		_, err := Compose(layout, info, Composition{Subject: "S", Repeat: map[string]int{"intro": n}}, nil)
		if want := "the draft repeated intro; this layout does not repeat that block"; err == nil || err.Error() != want {
			t.Errorf("got %v, want %s", err, want)
		}
	}
}

func TestComposeLeavesBrandSlotsUntouched(t *testing.T) {
	layout, info := repeatLayout(t)
	alt := "Hacked"
	_, err := Compose(layout, info, Composition{Subject: "S", Content: []ContentItem{{ID: "logo", Alt: &alt}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "brand block") {
		t.Fatalf("got %v", err)
	}
}

func TestComposeSlotsListsRepeatedIDs(t *testing.T) {
	layout, info := repeatLayout(t)
	slots, err := ComposeSlots(layout, info, []string{"note"}, map[string]int{"step": 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slotIDs(slots), ","); got != "intro,step-title,step-body,step-title-2,step-body-2" {
		t.Fatalf("got %s", got)
	}
	if _, err := ComposeSlots(layout, info, []string{"intro"}, nil); err == nil {
		t.Fatal("want a refusal")
	}
}

func TestComposeIdenticalPicturesCountOnce(t *testing.T) {
	pic := testPNG(t, 4, false)
	var blocks []string
	var content []ContentItem
	for n := 0; n < 9; n++ {
		id := fmt.Sprintf("pic%d", n)
		blocks = append(blocks, fmt.Sprintf(`{"id":%q,"type":"image","role":"content","alt":"x","width":100,"height":50}`, id))
		h := "p"
		content = append(content, ContentItem{ID: id, Image: &h})
	}
	layout, err := Parse([]byte(layoutJSON(strings.Join(blocks, ","))))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Compose(layout, LayoutInfo{}, Composition{Subject: "S", Content: content}, map[string]string{"p": pic})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Render(context.Background(), out, nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countIssues(r.Issues, IssueImagesLimit); n != 0 {
		t.Fatalf("images_limit said %d times: %+v", n, r.Issues)
	}
}

func TestEveryLayoutStaysWithinPictureBudgetWithAFullKit(t *testing.T) {
	for _, info := range Layouts() {
		t.Run(info.ID, func(t *testing.T) {
			kit := testKit(t)
			kit.Logo.Src = testPNG(t, 4, false)
			// The parts for a band and the whole picture for a compact slot:
			// either way no email carries more than six of the kit's pictures.
			kit.Signature = &BrandSignature{Photo: testPNG(t, 5, false), NameImage: testPNG(t, 6, false), Title: "Founder", Picture: testRectPNG(t, 40, 12)}
			kit.Social = []Link{
				{Network: "Instagram", Href: "https://instagram.com/x", Icon: testPNG(t, 7, false)},
				{Network: "YouTube", Href: "https://youtube.com/x", Icon: testPNG(t, 8, false)},
				{Network: "Facebook", Href: "https://facebook.com/x", Icon: testPNG(t, 9, false)},
			}
			branded, err := ApplyBrand(info.Document, kit)
			if err != nil {
				t.Fatal(err)
			}
			repeat := map[string]int{}
			for id, rng := range info.Repeat {
				repeat[id] = rng.Max
			}
			slots, err := ComposeSlots(branded, info, nil, repeat)
			if err != nil {
				t.Fatal(err)
			}
			images := map[string]string{}
			var content []ContentItem
			side := 10
			for _, s := range slots {
				if s.Type != typeImage {
					continue
				}
				side++
				handle := fmt.Sprintf("h%d", side)
				images[handle] = testPNG(t, side, false)
				content = append(content, ContentItem{ID: s.ID, Image: &handle})
			}
			out, err := Compose(branded, info, Composition{Subject: "S", Repeat: repeat, Content: content}, images)
			if err != nil {
				t.Fatal(err)
			}
			r, err := Render(context.Background(), out, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if n := countIssues(r.Issues, IssueImagesLimit); n != 0 {
				t.Fatalf("images_limit: %+v", r.Issues)
			}
		})
	}
}
