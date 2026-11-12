package accounts

// export_bytes.go -- the bytes a file carries (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections B
// "Bytes" and C "@bytes").
//
// A row is believed about which bytes are its own only as far as its kind's
// declaration allows. For the NEWEST version of each carried row, each
// @bytes field that is not `counted`:
//
//   - a `file://` reference names files shipped inside the engine image, and
//     is skipped;
//   - any other is resolved to its object key by the blob reader's OWN parse
//     (AccountBytesReader.Resolve), and that same key is asked
//     BytesField.Admits and then read, so the key judged is the key read. A
//     reference the declaration does not admit, or whose shape (one object
//     or a tree) is not the field's, is recorded as refused in bytes.jsonl,
//     counted in the report, and never read. One the reader's own floor
//     refuses (another container, a place under exports/) is refused the
//     same way: the reader says so with azureblob.ErrBytesReferenceRefused,
//     and any other error from it fails the run;
//   - an admitted reference is read with BytesField.Places as `under`: one
//     object through Stat and Open, or every object of a tree through List
//     and Open. Each distinct object is copied once, with every row that
//     uses it; one that is gone is recorded as missing and the run goes on.
//     An object whose length differs from what storage reported fails the
//     run (Writer.Bytes refuses a short or a long reader).
//
// A `counted` field is never opened: Stat sizes it where storage answers, and
// it is counted "size unknown" where it does not. A reference only an OLDER
// version of a carried row makes is counted the same way.
//
// Nothing here reads storage inside the snapshot: the references are resolved
// (a parse, no request) and collected during the walk, and the bytes are read
// after the transaction ends. A field declared @secret is never read as bytes.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/accountexport"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// refusedByReader reports whether the blob reader refused the reference, as
// opposed to failing.
func refusedByReader(err error) bool { return errors.Is(err, azureblob.ErrBytesReferenceRefused) }

// byteRef is one reference, as the row wrote it, with the rows that make it.
type byteRef struct {
	ref    string // as the row wrote it (the first row to write it)
	key    string // as the reader resolves it: the key Admits judged
	tree   bool
	under  []string
	usedBy []accountexport.RowField
}

type bytesBook struct {
	// resolve is the blob reader's own parse (AccountBytesReader.Resolve).
	resolve func(ref string) (key string, tree, shipped bool, err error)
	// err is the first resolution that failed for another reason than a
	// refusal; it fails the run at the end of the row that met it.
	err     error
	fields  map[string][]memorynodes.BytesField // by concept, read once
	carried map[string]*byteRef
	refused map[string]*byteRef
	// sized is every reference counted and not carried: a `counted` field's,
	// and one only an older version made. Keyed by the reference, with its
	// places for Stat.
	sized map[string][]string
}

func newBytesBook() *bytesBook {
	return &bytesBook{
		fields: map[string][]memorynodes.BytesField{}, carried: map[string]*byteRef{},
		refused: map[string]*byteRef{}, sized: map[string][]string{},
	}
}

func (b *bytesBook) bytesFields(c *memorynodes.Concept) []memorynodes.BytesField {
	if c == nil {
		return nil
	}
	f, ok := b.fields[c.Name]
	if !ok {
		// A field declared secret is never read as bytes: its value does not
		// travel, so neither do the bytes it names.
		for _, bf := range c.BytesFields() {
			if !isSecretPath(c, bf.Name) {
				f = append(f, bf)
			}
		}
		b.fields[c.Name] = f
	}
	return f
}

// tracks reports whether the kind declares any @bytes field.
func (b *bytesBook) tracks(c *memorynodes.Concept) bool { return len(b.bytesFields(c)) > 0 }

// references is every @bytes reference one version makes, with its places.
func (b *bytesBook) references(c *memorynodes.Concept, n memorynodes.MemoryNode) map[string][]string {
	payload, err := payloadMap(n.Payload)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, f := range b.bytesFields(c) {
		for _, v := range fieldValues(payload, f.Name) {
			if !strings.HasPrefix(v, "file://") {
				out[v] = f.Places(n.ID, payload)
			}
		}
	}
	return out
}

func addUse(m map[string]*byteRef, mapKey, ref, key string, tree bool, under []string, use accountexport.RowField) {
	r := m[mapKey]
	if r == nil {
		r = &byteRef{ref: ref, key: key, tree: tree, under: under}
		m[mapKey] = r
	}
	r.usedBy = append(r.usedBy, use)
}

func (b *bytesBook) resolveRef(ref string) (string, bool, bool, error) {
	if b.resolve == nil {
		return "", false, false, errors.New("the bytes reader is not wired on this node")
	}
	return b.resolve(ref)
}

// fromNewest reads a carried row's newest version's references; older are
// the references its older versions made.
func (b *bytesBook) fromNewest(c *memorynodes.Concept, rowID string, payload map[string]any, older map[string][]string) {
	current := map[string]bool{}
	for _, f := range b.bytesFields(c) {
		for _, v := range fieldValues(payload, f.Name) {
			current[v] = true
			if strings.HasPrefix(v, "file://") {
				continue
			}
			places := f.Places(rowID, payload)
			if f.Counted {
				b.sized[v] = places
				continue
			}
			use := accountexport.RowField{Concept: c.Name, ID: rowID, Field: f.Name}
			key, tree, shipped, err := b.resolveRef(v)
			switch {
			case err != nil && !refusedByReader(err):
				if b.err == nil {
					b.err = fmt.Errorf("resolve the bytes of %s %s: %w", c.Name, rowID, err)
				}
				continue
			case shipped:
				continue
			case err != nil || tree != f.Tree || !f.Admits(rowID, payload, key):
				addUse(b.refused, v, v, "", f.Tree, nil, use)
				continue
			}
			addUse(b.carried, key, v, key, tree, places, use)
		}
	}
	for ref, under := range older {
		if !current[ref] {
			b.sized[ref] = under
		}
	}
}

// plannedObject is one object to copy, with every row that uses it.
type plannedObject struct {
	meta  accountexport.BytesMeta
	size  int64
	under []string
}

// copyBytes is the bytes phase.
func (a *attempt) copyBytes(ctx context.Context) error {
	b := a.snap.bytes
	r := &a.report.Bytes
	needsStorage := len(b.carried) > 0 || len(b.sized) > 0
	var src accountBytesSource
	if needsStorage {
		if a.w.bytes == nil {
			return fmt.Errorf("the bytes reader is not wired on this node")
		}
		var err error
		if src, err = a.w.bytes(); err != nil {
			return fmt.Errorf("bytes storage: %w", err)
		}
	}

	plan := map[string]*plannedObject{}
	var missing []accountexport.BytesMeta
	refused := map[string]*byteRef{}
	for k, v := range b.refused {
		refused[k] = v
	}
	for _, ref := range sortedKeys(b.carried) {
		br := b.carried[ref]
		uses := sortedUses(br.usedBy)
		if br.tree {
			objects, err := src.List(ctx, "blob://"+br.key+"/", br.under)
			if err != nil {
				if refusedByReader(err) {
					refused[br.ref] = br
					continue
				}
				return fmt.Errorf("list the bytes of %s: %w", br.ref, err)
			}
			if len(objects) == 0 {
				missing = append(missing, accountexport.BytesMeta{Key: br.ref, UsedBy: uses})
				continue
			}
			prefix := br.key + "/"
			for _, o := range objects {
				addPlanned(plan, o.Key, o.Size, br.under, uses, br.ref, strings.TrimPrefix(o.Key, prefix))
			}
			continue
		}
		info, ok, shipped, err := src.Stat(ctx, br.key, br.under)
		switch {
		case err != nil && refusedByReader(err):
			refused[br.ref] = br
		case err != nil:
			return fmt.Errorf("look up the bytes of %s: %w", br.ref, err)
		case shipped:
		case !ok:
			missing = append(missing, accountexport.BytesMeta{Key: br.ref, UsedBy: uses})
		default:
			addPlanned(plan, info.Key, info.Size, br.under, uses, "", "")
		}
	}

	keys := sortedKeys(plan)
	for _, k := range keys {
		a.prog.BytesTotal += plan[k].size
	}
	a.progress(a.prog)
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := plan[k]
		rc, err := src.Open(ctx, k, p.under)
		if err != nil {
			return fmt.Errorf("open the bytes of %s: %w", k, err)
		}
		err = a.out.Bytes(p.meta, p.size, rc)
		rc.Close()
		if err != nil {
			return err
		}
		r.Objects++
		r.Bytes += p.size
		a.prog.BytesDone += p.size
		a.progress(a.prog)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Key < missing[j].Key })
	for _, m := range missing {
		if err := a.out.MissingBytes(m); err != nil {
			return err
		}
		r.Missing++
	}
	for _, ref := range sortedKeys(refused) {
		if err := a.out.RefusedBytes(accountexport.BytesMeta{Key: ref, UsedBy: sortedUses(refused[ref].usedBy)}); err != nil {
			return err
		}
		r.Refused++
	}

	// Counted, never opened.
	carriedRefs := map[string]bool{}
	for _, br := range b.carried {
		carriedRefs[br.ref], carriedRefs[br.key] = true, true
	}
	for _, ref := range sortedKeys(b.sized) {
		if carriedRefs[ref] {
			continue
		}
		r.NotCarriedObjects++
		info, ok, shipped, err := src.Stat(ctx, ref, b.sized[ref])
		switch {
		case shipped:
			r.NotCarriedObjects--
		case err == nil && ok:
			r.NotCarriedBytes += info.Size
		default:
			r.NotCarriedUnknownSize++
		}
	}
	return nil
}

func addPlanned(plan map[string]*plannedObject, key string, size int64, under []string, uses []accountexport.RowField, treeOf, path string) {
	p := plan[key]
	if p == nil {
		plan[key] = &plannedObject{
			meta: accountexport.BytesMeta{Key: key, UsedBy: uses, TreeOf: treeOf, Path: path},
			size: size, under: under,
		}
		return
	}
	p.meta.UsedBy = sortedUses(append(p.meta.UsedBy, uses...))
}

func sortedUses(u []accountexport.RowField) []accountexport.RowField {
	out := append([]accountexport.RowField(nil), u...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Concept != out[j].Concept {
			return out[i].Concept < out[j].Concept
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Field < out[j].Field
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
