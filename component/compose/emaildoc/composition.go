package emaildoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// composition.go -- what Claude Code submits, and how it becomes an email.
//
// A composition is never a whole document: it is the subject, the preheader,
// the content of the layout's content blocks, the ids of optional blocks to
// leave out and how many times to repeat the repeatable ones. Compose applies
// the drops and the repeats to the brand-applied layout, which only its
// LayoutInfo allows, and then writes the content with Merge, whose rules
// (no brand blocks, no replaced link tags, only selected pictures) it keeps.

// Composition is what a draft says about an email.
type Composition struct {
	Subject   string         `json:"subject"`
	Preheader string         `json:"preheader"`
	Drop      []string       `json:"drop"`
	Repeat    map[string]int `json:"repeat"`
	Content   []ContentItem  `json:"content"`
}

// RepeatedID is the id of the nth copy of a block: the block's own for the
// first, and the id with "-n" after it for the others.
func RepeatedID(id string, n int) string {
	if n == 1 {
		return id
	}
	return id + "-" + strconv.Itoa(n)
}

// Compose writes c into a copy of layout: it leaves out the blocks c drops,
// repeats the blocks c repeats, and writes c's content with Merge. A drop
// names only blocks info lists as optional, and a repeat only blocks info
// lists as repeatable, within the range it gives. layout is never changed.
func Compose(layout Document, info LayoutInfo, c Composition, images map[string]string) (Document, error) {
	shaped, err := shape(layout, info, c.Drop, c.Repeat)
	if err != nil {
		return Document{}, err
	}
	return Merge(shaped, Content{Subject: c.Subject, Preheader: c.Preheader, Blocks: c.Content}, images)
}

// ComposeSlots lists the content blocks of layout once drop and repeat are
// applied, so a writer is shown the ids a composition may write to, the
// copies of a repeated block included.
func ComposeSlots(layout Document, info LayoutInfo, drop []string, repeat map[string]int) ([]Slot, error) {
	shaped, err := shape(layout, info, drop, repeat)
	if err != nil {
		return nil, err
	}
	return ContentSlots(shaped), nil
}

// shape returns a copy of layout with the drops and then the repeats applied.
func shape(layout Document, info LayoutInfo, drop []string, repeat map[string]int) (Document, error) {
	if layout.Mode() != ModeLayout {
		return Document{}, errors.New("this email is custom HTML, so it has no blocks to write into")
	}
	for _, id := range drop {
		if !slices.Contains(info.Optional, id) {
			return Document{}, fmt.Errorf("the draft dropped a block this layout needs: %s", id)
		}
	}
	ids := make([]string, 0, len(repeat))
	for id := range repeat {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		n := repeat[id]
		rng, ok := info.Repeat[id]
		if !ok {
			return Document{}, fmt.Errorf("the draft repeated %s %d times; this layout allows %d to %d", id, n, 1, 1)
		}
		if n < rng.Min || n > rng.Max || n < 1 {
			return Document{}, fmt.Errorf("the draft repeated %s %d times; this layout allows %d to %d", id, n, rng.Min, rng.Max)
		}
		if slices.Contains(drop, id) {
			return Document{}, fmt.Errorf("the draft both dropped and repeated %s", id)
		}
	}
	raw, err := layout.JSON()
	if err != nil {
		return Document{}, err
	}
	out, err := Parse(raw)
	if err != nil {
		return Document{}, fmt.Errorf("the layout is not a valid email document: %w", err)
	}
	for _, id := range drop {
		removeBlock(&out.Blocks, id)
	}
	for _, id := range ids {
		if n := repeat[id]; n > 1 {
			if err := repeatBlock(&out.Blocks, id, n); err != nil {
				return Document{}, err
			}
		}
	}
	shaped, err := out.JSON()
	if err != nil {
		return Document{}, err
	}
	return Parse(shaped)
}

// repeatBlock inserts n-1 copies of the block with this id after it, each
// copy after the one before.
func repeatBlock(blocks *[]Block, id string, n int) error {
	for i := range *blocks {
		b := &(*blocks)[i]
		if b.ID == id {
			clones := make([]Block, 0, n-1)
			for k := 2; k <= n; k++ {
				clone, err := cloneBlock(*b, k)
				if err != nil {
					return err
				}
				clones = append(clones, clone)
			}
			*blocks = slices.Insert(*blocks, i+1, clones...)
			return nil
		}
		if err := repeatBlock(&b.Blocks, id, n); err != nil {
			return err
		}
		for c := range b.Columns {
			if err := repeatBlock(&b.Columns[c].Blocks, id, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// cloneBlock copies b and every block inside it, giving each the id RepeatedID
// makes of it for the nth copy.
func cloneBlock(b Block, n int) (Block, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return Block{}, err
	}
	var out Block
	if err := json.Unmarshal(raw, &out); err != nil {
		return Block{}, err
	}
	renumber(&out, n)
	return out, nil
}

func renumber(b *Block, n int) {
	b.ID = RepeatedID(b.ID, n)
	for i := range b.Blocks {
		renumber(&b.Blocks[i], n)
	}
	for c := range b.Columns {
		for i := range b.Columns[c].Blocks {
			renumber(&b.Columns[c].Blocks[i], n)
		}
	}
}
