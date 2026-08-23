package dslconformance

import (
	"fmt"
	"sort"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

// The same contract identity_audit_enum_contract_test.go enforces for
// v1:identity:auditEvent, for its sibling v1:identity:authActivity
// (memql#4328) -- and here the stakes are higher, because `action` is a CLOSED
// enum on this concept.
//
// On auditEvent, `action` is an unconstrained string: a writer emitting an
// action nobody declared still lands a row. On authActivity a writer emitting
// an undeclared action is REFUSED at insert, on every emission, and only the
// slog line survives -- which is memql#4213 all over again, on the log whose
// rows reuse detection depends on.
//
// Three directions are checked, and all three are needed:
//   - the mutation accepts nothing the concept refuses,
//   - the concept stores nothing the mutation cannot write,
//   - and no Go writer emits a value outside either.

// activityEnumFields are the enum-typed fields createAuthActivity accepts,
// keyed by DSL arg name, with the Go AuditEvent field carrying each.
var activityEnumFields = map[string]string{
	"action":  "Action",
	"outcome": "Outcome",
}

func authActivityEnumsFromDSL(t *testing.T) (mutation, concept map[string]enumSet) {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	mutation = map[string]enumSet{}
	concept = map[string]enumSet{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				if d.Name != "createAuthActivity" || d.Type != languageAst.FunctionTypeMutation || d.ArgsSchema == nil {
					continue
				}
				for _, f := range d.ArgsSchema.Fields {
					if _, enumTyped := activityEnumFields[f.Name]; !enumTyped {
						continue
					}
					set := enumSet{}
					for _, v := range f.Enum {
						set[fmt.Sprint(v)] = true
					}
					mutation[f.Name] = set
				}
			case *languageAst.ConceptDecl:
				if d.Name != "authActivity" {
					continue
				}
				for _, p := range d.Properties {
					if _, enumTyped := activityEnumFields[p.Name]; !enumTyped || p.Type == nil || p.Type.Kind != "enum" {
						continue
					}
					set := enumSet{}
					for _, v := range p.Type.EnumValues {
						set[v] = true
					}
					concept[p.Name] = set
				}
			}
		}
	}
	for field := range activityEnumFields {
		if len(mutation[field]) == 0 {
			t.Fatalf("createAuthActivity declares no enum for %q -- either the arg lost its enum "+
				"type or the parse narrowed; this gate cannot pass vacuously", field)
		}
		if len(concept[field]) == 0 {
			t.Fatalf("concept authActivity declares no enum for %q -- either the field lost its "+
				"enum type or the parse narrowed; this gate cannot pass vacuously", field)
		}
	}
	return mutation, concept
}

func TestAuthActivityEnumsAgreeBetweenMutationAndConcept(t *testing.T) {
	mutation, concept := authActivityEnumsFromDSL(t)
	for field, mset := range mutation {
		for _, v := range mset.sorted() {
			if !concept[field][v] {
				t.Errorf("createAuthActivity.%s accepts %q but concept authActivity.%s does not (%v). "+
					"The arg passes validation and the row insert refuses it one layer later.",
					field, v, field, concept[field].sorted())
			}
		}
		for _, v := range concept[field].sorted() {
			if !mset[v] {
				t.Errorf("concept authActivity.%s stores %q but createAuthActivity.%s never accepts "+
					"it (%v); the value is unreachable through the only writer.",
					field, v, field, mset.sorted())
			}
		}
	}
	// Neither field carries "" -- both are `!` on the concept. A mechanic with
	// no outcome is not a mechanic, and an empty action names nothing.
	for _, field := range []string{"action", "outcome"} {
		if concept[field][""] {
			t.Errorf("concept authActivity.%s accepts the empty string; both enums are required "+
				"and an empty value would be a row that records nothing", field)
		}
	}
}

// The Go side: every AuditEvent literal in component/identity that lands on the
// ACTIVITY stream must carry an action and an outcome the concept declares.
//
// It reuses the audit contract's scanner, which follows a literal into the call
// it is passed to -- which is what makes rotate.go's `r.activity(ctx,
// identity.AuditEvent{...}, user)` sites visible at all, since the Stream field
// is stamped by that helper rather than by the literal.
func TestIdentityActivityWritersMatchAuthActivityEnums(t *testing.T) {
	mutation, concept := authActivityEnumsFromDSL(t)
	scan := scanIdentityAuditWriters(t)

	type row struct {
		pos, field, value string
	}
	var table []row
	var activitySites int
	for _, site := range scan.sites {
		streams, _, present := scan.fieldValues(site, "Stream")
		if !present {
			continue // the default is the audit log; not this gate's business
		}
		onActivity := false
		for _, s := range streams {
			if s == "activity" {
				onActivity = true
			}
		}
		if !onActivity {
			continue
		}
		activitySites++
		for dslField, goField := range activityEnumFields {
			values, unresolved, has := scan.fieldValues(site, goField)
			if !has {
				t.Errorf("%s: an activity-stream AuditEvent sets no %s, and createAuthActivity "+
					"requires it", site.pos, goField)
				continue
			}
			for _, u := range unresolved {
				t.Errorf("%s: %s cannot be resolved to string literals (%s). Pass a literal or a "+
					"package constant so the value can be checked against the DSL enum.",
					site.pos, goField, u)
			}
			for _, v := range values {
				table = append(table, row{site.pos, dslField, v})
			}
		}
	}

	// The four writers the design names: the rotator's two outcomes, its
	// grace-window acceptance, and the PAT verifier. A scanner that found
	// fewer is broken rather than the tree being tidier -- and a gate over
	// zero sites passes forever.
	const minActivitySites = 4
	if activitySites < minActivitySites {
		t.Fatalf("found only %d activity-stream AuditEvent literal(s) under component/identity "+
			"(want >= %d). Either the scanner cannot see them or the writers were never moved.",
			activitySites, minActivitySites)
	}

	sort.Slice(table, func(i, j int) bool {
		if table[i].pos != table[j].pos {
			return table[i].pos < table[j].pos
		}
		return table[i].field < table[j].field
	})
	seen := map[string]enumSet{}
	for _, r := range table {
		if seen[r.field] == nil {
			seen[r.field] = enumSet{}
		}
		seen[r.field][r.value] = true
		if !mutation[r.field][r.value] {
			t.Errorf("%s: %s=%q is not in createAuthActivity's enum %v. The durable write is "+
				"refused on EVERY emission and only the slog line survives -- and for a rotation "+
				"that also costs reuse detection its evidence (memql#4328, memql#4329).",
				r.pos, r.field, r.value, mutation[r.field].sorted())
			continue
		}
		if !concept[r.field][r.value] {
			t.Errorf("%s: %s=%q passes createAuthActivity but concept authActivity refuses it %v",
				r.pos, r.field, r.value, concept[r.field].sorted())
		}
	}
	for field := range activityEnumFields {
		if len(seen[field]) == 0 {
			t.Errorf("no activity-stream literal resolved a %s value at all -- the resolver is "+
				"broken, not the writers", field)
		}
	}

	// Every declared action must have a writer. A value nobody emits is a
	// promise in the shape of an enum -- which is exactly what
	// refresh_succeeded and refresh_token_theft_detected were on auditEvent.
	for _, action := range concept["action"].sorted() {
		if !seen["action"][action] {
			t.Errorf("concept authActivity.action declares %q but no writer under "+
				"component/identity emits it. The enum is closed precisely so it names what is "+
				"actually written; drop the value or write the writer.", action)
		}
	}

	if testing.Verbose() {
		for _, r := range table {
			t.Logf("%-70s %-8s %q", r.pos, r.field, r.value)
		}
	}
}
