package accounts

// export_references.go -- references.jsonl and connections.jsonl (design
// record docs/superpowers/specs/2026-11-12-account-export-design.md, section
// B, "References and connections").
//
// A reference is a row outside the file that a carried row's NEWEST version
// points at through a declared field: its `via`, or an outgoing
// @relationship. A person is not a reference (people.jsonl names them), a row
// the file carries is not one either (the carried set's hashes decide that the
// moment the pointing row is read, whichever field names it), and a field
// declared @secret is never read as one.
//
// THE KIND IS THE DECLARATION'S, NOT THE VALUE'S. A value is something a
// person wrote, so it does not choose which rows the run reads. When the
// field declares its target, a value whose own id names another kind is
// recorded as written, kind and id, provided "outside", and nothing is read.
// A field with no declared target (a `via` that may name any kind) takes the
// kind from the value's own id, and the same rules follow from there.
//
// A target row is READ in one case only: its kind is declared `shared` (and
// does not stay), to see whether the row is the cluster's own (it names no
// account) and to take its title, which is never a field declared secret.
// Nothing else is read:
//
//   - provided "cluster": a row of a cluster-wide kind, kind and id only, or
//     the cluster's own row of a shared kind, with its title;
//   - provided "outside": every other row the file does not carry -- another
//     account's, of a kind that stays, in no account, or not there at all --
//     kind and id only, which the pointing row already holds.
//
// connections.jsonl lists what is set up again on a target: one line per
// secret NAME a carried row names through a @secretName field (never the
// value, never a fingerprint), one per pointed-at row of a cluster-wide kind
// that declares a @secret value at any depth (a credential: kind and id only;
// the KIND decides, no row is read), and one per kind that stays, with its
// count from the census.
//
// Everything is ordered in Go's byte order of the values, the file's own
// order, so two runs over an unchanged account write the same lines.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Where a referenced row is provided.
const (
	providedCluster = "cluster"
	providedOutside = "outside"
)

// refRow names one row: its kind and canonical id.
type refRow struct {
	Concept string `json:"concept"`
	ID      string `json:"id"`
}

// refFrom counts the carried rows that point at a target through one field.
type refFrom struct {
	Concept string `json:"concept"`
	Field   string `json:"field"`
	Rows    int64  `json:"rows"`
}

// referenceLine is one line of references.jsonl.
type referenceLine struct {
	To       refRow    `json:"to"`
	Provided string    `json:"provided"`
	Name     string    `json:"name,omitempty"`
	From     []refFrom `json:"from"`
}

// connectionLine is one line of connections.jsonl, by kind.
type connectionLine struct {
	Kind    string    `json:"kind"`
	Name    string    `json:"name,omitempty"`
	From    []refFrom `json:"from,omitempty"`
	To      *refRow   `json:"to,omitempty"`
	Concept string    `json:"concept,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Rows    *int64    `json:"rows,omitempty"`
}

type fieldKey struct{ concept, field string }

type referenceBook struct {
	s       *snapshot
	targets map[refRow]map[fieldKey]int64
	// mismatched are targets a value named through a field that declares
	// another kind: listed as written, never read.
	mismatched map[refRow]bool
	secrets    map[string]map[fieldKey]int64
}

func newReferenceBook(s *snapshot) *referenceBook {
	return &referenceBook{s: s, targets: map[refRow]map[fieldKey]int64{}, mismatched: map[refRow]bool{}, secrets: map[string]map[fieldKey]int64{}}
}

// referenceField is one field of a kind that points at another row.
type referenceField struct {
	field  string
	target string // the kind it declares, or "" when the value's id names it
}

// referenceFields is a kind's `via` and its outgoing relationships, people
// and secret fields left out, each field once.
func referenceFields(c *memorynodes.Concept) []referenceField {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []referenceField
	for _, r := range c.Relationships {
		if r.Direction != "outgoing" || r.TargetConcept == userConcept || seen[r.Field] {
			continue
		}
		seen[r.Field] = true
		if !isSecretPath(c, r.Field) {
			out = append(out, referenceField{field: r.Field, target: r.TargetConcept})
		}
	}
	if d := c.BelongsTo; d != nil && d.Via != "" && !seen[d.Via] && !isSecretPath(c, d.Via) {
		out = append(out, referenceField{field: d.Via, target: d.Parent})
	}
	return out
}

// targetOf answers the row a value names through a field that declares the
// kind `declared` ("" for none). mismatched is true for a value whose own id
// names another kind than the declared one: that row is the value as written.
func targetOf(value, declared string) (row refRow, mismatched, ok bool) {
	value = strings.TrimSpace(value)
	concept, short, err := id.ParseNodeId(value)
	if err != nil || short == "" {
		concept, short = "", value
	}
	switch {
	case short == "":
		return refRow{}, false, false
	case declared != "" && concept != "" && concept != declared:
		return refRow{Concept: concept, ID: value}, true, true
	case declared != "":
		return refRow{Concept: declared, ID: declared + ":" + short}, false, true
	case concept == "":
		// No declared kind and none in the value: nothing says what it is.
		return refRow{}, false, false
	}
	return refRow{Concept: concept, ID: concept + ":" + short}, false, true
}

// fromRow notes what a carried row's newest version points at.
func (b *referenceBook) fromRow(c *memorynodes.Concept, rowID string, payload map[string]any) {
	for _, f := range referenceFields(c) {
		for _, v := range fieldValues(payload, f.field) {
			t, mismatched, ok := targetOf(v, f.target)
			// A row the file carries is no reference, whichever field
			// names it and whatever kind that field declares.
			if !ok || t.Concept == userConcept || b.s.isCarried(t.Concept, t.ID) {
				continue
			}
			if mismatched {
				b.mismatched[t] = true
			}
			from := b.targets[t]
			if from == nil {
				from = map[fieldKey]int64{}
				b.targets[t] = from
			}
			from[fieldKey{c.Name, f.field}]++
		}
	}
	for _, field := range c.SecretNameFields() {
		if isSecretPath(c, field) {
			continue
		}
		for _, name := range fieldValues(payload, field) {
			from := b.secrets[name]
			if from == nil {
				from = map[fieldKey]int64{}
				b.secrets[name] = from
			}
			from[fieldKey{c.Name, field}]++
		}
	}
}

func sortedFrom(m map[fieldKey]int64) []refFrom {
	out := make([]refFrom, 0, len(m))
	for k, n := range m {
		out = append(out, refFrom{Concept: k.concept, Field: k.field, Rows: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Concept != out[j].Concept {
			return out[i].Concept < out[j].Concept
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// resolveBatch is how many targets of one kind one read asks for.
const resolveBatch = 500

// resolve answers references.jsonl. Only targets of a shared kind are read
// (see the file header); every other line is decided by its kind alone.
func (b *referenceBook) resolve(ctx context.Context) ([]referenceLine, error) {
	byKind := map[string][]refRow{}
	for t := range b.targets {
		byKind[t.Concept] = append(byKind[t.Concept], t)
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var out []referenceLine
	for _, kind := range kinds {
		targets := byKind[kind]
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		c := b.s.a.byName[kind]
		// A kind that stays is never read, whatever else it declares.
		readKind := c.SharedRows() && c.Stays() == ""
		var readable []refRow
		for _, t := range targets {
			if !b.mismatched[t] && readKind {
				readable = append(readable, t)
			}
		}
		newest := map[string]map[string]any{}
		for start := 0; start < len(readable); start += resolveBatch {
			got, err := b.newest(ctx, kind, readable[start:min(start+resolveBatch, len(readable))])
			if err != nil {
				return nil, err
			}
			for k, v := range got {
				newest[k] = v
			}
		}
		for _, t := range targets {
			line := referenceLine{To: t, Provided: providedOutside, From: sortedFrom(b.targets[t])}
			switch {
			case b.mismatched[t]:
			case c.ClusterWide():
				line.Provided = providedCluster
			case readKind:
				if payload, found := newest[memql.BareShortId(t.ID)]; found && namesNoAccount(payload) {
					line.Provided = providedCluster
					line.Name = rowTitle(c, payload)
				}
			}
			out = append(out, line)
		}
	}
	return out, nil
}

// newest is the newest payload of each target, by short id. Called for the
// targets of a shared kind only.
func (b *referenceBook) newest(ctx context.Context, kind string, targets []refRow) (map[string]map[string]any, error) {
	ids := make([]string, 0, 2*len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID, memql.BareShortId(t.ID))
	}
	raw := map[string]json.RawMessage{}
	err := memql.AccountRowVersions(ctx, b.s.q, kind, ids, func(n memorynodes.MemoryNode, _ bool) error {
		// Versions arrive oldest first within an id; the two spellings of
		// one id are two ids here, and either is the row.
		raw[memql.BareShortId(n.ID)] = n.Payload
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(raw))
	for short, p := range raw {
		m := map[string]any{}
		_ = json.Unmarshal(p, &m)
		out[short] = m
	}
	return out, nil
}

// namesNoAccount reports whether a shared kind's row is the cluster's own.
func namesNoAccount(payload map[string]any) bool {
	account, _ := payload["accountId"].(string)
	return strings.TrimSpace(account) == ""
}

// rowTitle is the name a person knows a row by.
func rowTitle(c *memorynodes.Concept, payload map[string]any) string {
	fields := []string{"name", "title", "displayName"}
	if c != nil && c.DisplayCard != nil && c.DisplayCard.Primary != "" {
		fields = append([]string{c.DisplayCard.Primary}, fields...)
	}
	for _, f := range fields {
		// A field declared secret is never a title.
		if isSecretPath(c, f) {
			continue
		}
		if v := fieldValues(payload, f); len(v) == 1 {
			return v[0]
		}
	}
	return ""
}

// connections is connections.jsonl: secrets by name, credentials by row, then
// every kind that stays with its count.
func (b *referenceBook) connections(census map[string]memql.AccountCensusLine) []connectionLine {
	var out []connectionLine
	names := make([]string, 0, len(b.secrets))
	for n := range b.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, connectionLine{Kind: "secret", Name: n, From: sortedFrom(b.secrets[n])})
	}
	var creds []refRow
	for t := range b.targets {
		c := b.s.a.byName[t.Concept]
		if !b.mismatched[t] && c.ClusterWide() && len(SecretPaths(c)) > 0 {
			creds = append(creds, t)
		}
	}
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].Concept != creds[j].Concept {
			return creds[i].Concept < creds[j].Concept
		}
		return creds[i].ID < creds[j].ID
	})
	for i := range creds {
		out = append(out, connectionLine{Kind: "credential", To: &creds[i]})
	}
	for _, kind := range b.s.a.scoped {
		c := b.s.a.byName[kind]
		if reason := c.Stays(); reason != "" {
			rows := census[kind].Rows
			out = append(out, connectionLine{Kind: "stays", Concept: kind, Reason: reason, Rows: &rows})
		}
	}
	return out
}
