package accounts

// export_people.go -- people.jsonl and an operator's versions (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section B,
// "People").
//
// People are cluster-wide and their rows never travel. A file names each
// person its carried rows name, found four ways: the owner field a kind's
// @rowAuthz(owner=...) names, every field with an outgoing @relationship to
// v1:identity:user, the account's membership rows (whose userId is such a
// field), and the writer of every version. The first two and the membership
// are read from a row's NEWEST version; the writer from every version.
//
// Four kinds of person:
//
//   - member / former: a person with a membership row in the account; member
//     when one is active. Id, name, email, role and whether it is active.
//   - operator: an owner or developer with no membership row in the account.
//     Id and the word only. The primary account's own file has none: its
//     owners and developers are its people.
//   - other: anyone else a row names. Id, name and email.
//
// A version an operator wrote has its request context removed: the keys
// identity.displayName, identity.email, request.userAgent and every geo.* and
// server.* key of its metadata (component/metadata's flat keys), and an email
// in createdBy becomes `user:<short id>`. A version is an operator's when the
// subject it records (identity.subject) is an operator, or when no subject
// resolves to a person and the role it recorded (identity.role) was owner or
// developer. Every other version's metadata travels as stored.
//
// The role a member holds is the person's role on the cluster
// (v1:identity:user.role): a membership row carries no role of its own.
//
// Everyone on the cluster is read once, id, name, email, role and active
// only, so a writer can be told from a stranger while the walk streams and no
// statement is issued inside it. No sign-in material is read.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// membershipConcept is the account's membership rows.
const membershipConcept = "v1:identity:groupMembership"

// The ranks an operator holds (v1:rbac:role slugs; neither has an alias).
var operatorRoles = map[string]bool{"owner": true, "developer": true}

// The kinds of person in people.jsonl.
const (
	personMember   = "member"
	personFormer   = "former"
	personOther    = "other"
	personOperator = "operator"
)

// personLine is one line of people.jsonl.
type personLine struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role,omitempty"`
	Active *bool  `json:"active,omitempty"`
}

type clusterPerson struct {
	short, name, email, role string
	active                   bool
}

type peopleBook struct {
	primary   bool
	users     map[string]*clusterPerson // short id -> the person
	byEmail   map[string]string         // lower-cased email -> short id
	members   map[string]bool           // short id -> holds an active membership
	operators map[string]bool
	named     map[string]bool
	byName    map[string]*memorynodes.Concept
}

// clusterPeopleSQL is every person's newest version, five fields of it.
//
// staged-data: MUST-NOT-GATE -- the people an account's rows name, read behind the export's capability; a person is a person whether or not their kind's data is staged.
const clusterPeopleSQL = `SELECT id, COALESCE(payload->>'displayName', ''), COALESCE(payload->>'primaryEmail', ''),
       COALESCE(payload->>'role', ''), payload->'active' IS DISTINCT FROM 'false'::jsonb
  FROM (SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes" WHERE concept = $1 ORDER BY id, "createdAt" DESC) u`

func loadPeople(ctx context.Context, q memql.AccountQuerier, account string, primary bool, byName map[string]*memorynodes.Concept) (*peopleBook, error) {
	b := &peopleBook{
		primary: primary, users: map[string]*clusterPerson{}, byEmail: map[string]string{},
		members: map[string]bool{}, operators: map[string]bool{}, named: map[string]bool{}, byName: byName,
	}
	rows, err := q.QueryContext(ctx, clusterPeopleSQL, userConcept)
	if err != nil {
		return nil, fmt.Errorf("read the people: %w", err)
	}
	for rows.Next() {
		var id string
		p := &clusterPerson{}
		if err := rows.Scan(&id, &p.name, &p.email, &p.role, &p.active); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the people: %w", err)
		}
		p.short = memql.BareShortId(id)
		b.users[p.short] = p
		if e := strings.ToLower(strings.TrimSpace(p.email)); e != "" {
			b.byEmail[e] = p.short
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the people: %w", err)
	}
	if err := b.loadMembers(ctx, q, account); err != nil {
		return nil, err
	}
	if !primary {
		for short, p := range b.users {
			if operatorRoles[p.role] {
				if _, member := b.members[short]; !member {
					b.operators[short] = true
				}
			}
		}
	}
	return b, nil
}

// loadMembers reads the account's membership rows, newest version of each.
func (b *peopleBook) loadMembers(ctx context.Context, q memql.AccountQuerier, account string) error {
	if !b.byName[membershipConcept].AccountScoped() {
		return nil
	}
	ids, err := memql.AccountKindRowIDs(ctx, q, account, membershipConcept)
	if err != nil {
		return err
	}
	const batch = 500
	for start := 0; start < len(ids); start += batch {
		newest := map[string]json.RawMessage{}
		err := memql.AccountRowVersions(ctx, q, membershipConcept, ids[start:min(start+batch, len(ids))],
			func(n memorynodes.MemoryNode, _ bool) error {
				newest[n.ID] = n.Payload // versions arrive oldest first: the last is the newest
				return nil
			})
		if err != nil {
			return err
		}
		for _, payload := range newest {
			var m struct {
				UserID string `json:"userId"`
				Status string `json:"status"`
			}
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			short, ok := b.resolve(m.UserID)
			if !ok {
				continue
			}
			b.members[short] = b.members[short] || m.Status == "active"
		}
	}
	return nil
}

// resolve answers the person a value names: an id in either spelling, or an
// email address.
func (b *peopleBook) resolve(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if strings.Contains(v, "@") {
		short, ok := b.byEmail[strings.ToLower(v)]
		return short, ok
	}
	if strings.Count(v, ":") > 0 && !strings.HasPrefix(v, userConcept+":") {
		return "", false
	}
	short := memql.BareShortId(v)
	_, ok := b.users[short]
	return short, ok
}

// fromRow names the people a row's newest version names.
func (b *peopleBook) fromRow(c *memorynodes.Concept, payload map[string]any) {
	for _, field := range personFields(c) {
		for _, v := range fieldValues(payload, field) {
			if short, ok := b.resolve(v); ok {
				b.named[short] = true
			}
		}
	}
}

// personFields is the owner field of a kind's @rowAuthz and every field with
// an outgoing relationship to a person.
func personFields(c *memorynodes.Concept) []string {
	if c == nil {
		return nil
	}
	var out []string
	if d := c.RowAuthz; d != nil && d.Tier == parser.RowAuthzOwned && d.Owner != "" && d.Owner != "id" {
		out = append(out, d.Owner)
	}
	for _, r := range c.Relationships {
		if r.Direction == "outgoing" && r.TargetConcept == userConcept {
			out = append(out, r.Field)
		}
	}
	// A field declared secret names nobody: its value is removed from the
	// file, so it must not reach people.jsonl either.
	kept := out[:0]
	for _, f := range out {
		if !isSecretPath(c, f) {
			kept = append(kept, f)
		}
	}
	return kept
}

// fieldValues reads a string, or a list of strings, at a dotted path.
func fieldValues(payload map[string]any, path string) []string {
	var cur any = payload
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	switch v := cur.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return []string{v}
		}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// versionMeta is the two keys of a version's metadata that say who wrote it.
type versionMeta struct {
	Subject string `json:"identity.subject"`
	Role    string `json:"identity.role"`
}

func readVersionMeta(raw json.RawMessage) versionMeta {
	var m versionMeta
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// writer answers the person who wrote a version: the subject it records,
// else its createdBy.
func (b *peopleBook) writer(n memorynodes.MemoryNode) (string, bool) {
	if short, ok := b.resolve(readVersionMeta(n.Metadata).Subject); ok {
		return short, true
	}
	return b.resolve(n.CreatedBy)
}

// noteWriter names the writer of every carried version.
func (b *peopleBook) noteWriter(n memorynodes.MemoryNode) {
	if short, ok := b.writer(n); ok {
		b.named[short] = true
	}
}

// operatorVersion reports whether an operator wrote the version, and which
// one when it can tell.
func (b *peopleBook) operatorVersion(n memorynodes.MemoryNode) (bool, string) {
	// Only the primary account's own file has no operators. A cluster with no
	// operator today still scrubs a version whose subject no longer resolves
	// and whose recorded role was an operator's.
	if b.primary {
		return false, ""
	}
	m := readVersionMeta(n.Metadata)
	if short, ok := b.resolve(m.Subject); ok {
		return b.operators[short], short
	}
	if !operatorRoles[strings.TrimSpace(m.Role)] {
		return false, ""
	}
	short, _ := b.resolve(n.CreatedBy)
	return true, short
}

// scrubbedMetaKey reports whether an operator's version loses the key.
func scrubbedMetaKey(k string) bool {
	switch k {
	case "identity.displayName", "identity.email", "request.userAgent":
		return true
	}
	return strings.HasPrefix(k, "geo.") || strings.HasPrefix(k, "server.")
}

// scrub answers the version's metadata and createdBy as the file carries them.
func (b *peopleBook) scrub(n memorynodes.MemoryNode) (json.RawMessage, string, error) {
	isOperator, short := b.operatorVersion(n)
	if !isOperator {
		return n.Metadata, n.CreatedBy, nil
	}
	metadata := n.Metadata
	if len(metadata) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &fields); err != nil {
			return nil, "", fmt.Errorf("the metadata is not an object: %w", err)
		}
		removed := false
		for k := range fields {
			if scrubbedMetaKey(k) {
				delete(fields, k)
				removed = true
			}
		}
		if removed {
			out, err := json.Marshal(fields)
			if err != nil {
				return nil, "", err
			}
			metadata = out
		}
	}
	createdBy := n.CreatedBy
	if strings.Contains(createdBy, "@") {
		if short == "" {
			short, _ = b.resolve(createdBy)
		}
		// An address no person on the cluster has is still an address: it
		// is replaced by the word rather than kept.
		if short != "" {
			createdBy = "user:" + short
		} else {
			createdBy = personOperator
		}
	}
	return metadata, createdBy, nil
}

// lines is people.jsonl, in id order.
func (b *peopleBook) lines() []personLine {
	shorts := make([]string, 0, len(b.named))
	for s := range b.named {
		shorts = append(shorts, s)
	}
	sort.Strings(shorts)
	out := make([]personLine, 0, len(shorts))
	for _, short := range shorts {
		p := b.users[short]
		line := personLine{ID: userConcept + ":" + short}
		active, member := b.members[short]
		switch {
		case member:
			line.Kind = personFormer
			if active {
				line.Kind = personMember
			}
			line.Name, line.Email, line.Role = p.name, p.email, p.role
			line.Active = &active
		case b.operators[short]:
			line.Kind = personOperator
		default:
			line.Kind = personOther
			line.Name, line.Email = p.name, p.email
		}
		out = append(out, line)
	}
	return out
}
