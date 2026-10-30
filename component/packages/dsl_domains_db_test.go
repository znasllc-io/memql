package packages

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// dsl_domains_db_test.go -- package.dslDomains against a real engine (design
// record 2026-10-29, D5).
//
// The offline tests beside analyze_access_gate_test.go prove what an analysis
// SENDS. What they cannot prove is what a row DOES with it: that the write
// replaces the list rather than merging into it, that the field comes back
// through the shape every package read returns, and that the read the bff's
// access handler makes -- the site's package, under the site's owner -- gets it.
//
// Postgres-gated like its neighbours: a skip is what an unreachable database
// produces, and MEMQL_REQUIRE_DB=1 turns it into a failure.

// dslDomainsOf is the dslDomains a package read returned, and whether the row
// carried the field at all: a package nobody has analyzed since the field
// existed has none, which is not the same as an empty list.
func dslDomainsOf(row map[string]any) (list []string, present bool) {
	if row == nil {
		return nil, false
	}
	_, present = row["dslDomains"]
	return rowStrings(row, "dslDomains"), present
}

func TestTheDomainsAPackageShipsAreRewrittenWholeAndComeBackThroughItsShape(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	owner := "v1:identity:user:dd-owner-" + suffix
	pkg := "v1:platform:package:dd-pkg-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{pkg, owner} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	seedTiePrincipal(t, eng, owner, auth.RoleOwner)
	ownerCtx := tieActorCtx(owner, auth.RoleOwner)
	createTiedPackage(t, eng, ownerCtx, pkg, "dd-"+suffix, "dd-acct-"+suffix)

	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}
	declares := []DeclaredDeployable{{Name: "docs", Kind: KindStatic}}
	read := func(t *testing.T) []string {
		t.Helper()
		row, err := s.packageById(ownerCtx, pkg)
		if err != nil || row == nil {
			t.Fatalf("read the package as its owner: %v (%v)", err, row)
		}
		list, present := dslDomainsOf(row)
		if !present {
			t.Fatalf("packageById returned no dslDomains: the field is not in the packageFull shape (%v)", row)
		}
		return list
	}

	// A package analyzed before the field existed carries none, and the bff
	// reads that as a package that ships nothing: no gate opens.
	if row, err := s.packageById(ownerCtx, pkg); err != nil || row == nil {
		t.Fatalf("read the new package: %v (%v)", err, row)
	} else if list, _ := dslDomainsOf(row); len(list) != 0 {
		t.Fatalf("a package nobody has analyzed has dslDomains %v", list)
	}

	for _, step := range []struct {
		name    string
		record  []string
		want    []string
		because string
	}{
		{"the first analysis", []string{"acme", "beta"}, []string{"acme", "beta"}, ""},
		// THE POINT OF REWRITING WHOLE. A merge would keep beta here for good,
		// and the package would go on owning a domain it no longer ships.
		{"a domain dropped upstream", []string{"acme"}, []string{"acme"}, "a merge keeps the dropped domain"},
		{"a domain added", []string{"acme", "gamma"}, []string{"acme", "gamma"}, ""},
		// nil is rendered as an empty list and not as the word null, which the
		// concept's []string refuses -- and a refused write leaves the list the
		// LAST analysis found.
		{"a nil list", nil, nil, "nil was written as null and refused, so the previous list stands"},
		{"a domain shipped again", []string{"acme"}, []string{"acme"}, ""},
		// An empty list really empties it: a source that ships no DSL any more
		// says so, and its old domains stop being its own.
		{"a source that ships no DSL any more", []string{}, nil, "an empty list does not replace the old one"},
	} {
		if err := s.recordPackageDeployables(ownerCtx, pkg, declares, step.record); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got := read(t)
		if len(got) == 0 && len(step.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: dslDomains = %v, want %v (%s)", step.name, got, step.want, step.because)
		}
	}

	// The same write carries declares, and writing the domains with it does not
	// disturb it.
	row, err := s.packageById(ownerCtx, pkg)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if declared, ok := row["declares"].([]any); !ok || len(declared) != 1 {
		t.Fatalf("declares = %v, want the one deployable the same write carried", row["declares"])
	}

	// Every read that returns a package returns the field, because they share
	// the one shape: the OS lists, and the bff's by-id read.
	if err := s.recordPackageDeployables(ownerCtx, pkg, declares, []string{"acme", "realns"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.queryAll(ownerCtx, "query packagesAll()")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		if memql.BareShortId(rowString(r, "id")) == memql.BareShortId(pkg) {
			found = true
			if list, present := dslDomainsOf(r); !present || !reflect.DeepEqual(list, []string{"acme", "realns"}) {
				t.Fatalf("packagesAll returned dslDomains %v (present %v), want [acme realns]", list, present)
			}
		}
	}
	if !found {
		t.Fatal("packagesAll did not return the package, so its shape was not checked")
	}
}

// The read Task 5's access handler makes: the site's package, as its owner,
// with the engine's origin stamped on the read because borrowed authority alone
// is a client read. The package tier is a composite one -- the owner, a cluster
// owner, or a member of the package's organization, ANDed with a current
// organization-membership and app check -- and the question this answers is
// whether the owner of a site is admitted to the package that made it.
//
// The answer depends on how the package is tied, and the cases are written out
// because a gate that cannot read its package's domains is a gate that answers
// 404 for no reason a person can see.
func TestASiteOwnerReadsTheDomainsOfThePackageTheyOwn(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}

	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	account := "dd-read-acct-" + suffix
	if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "dd read")`, langparser.QuoteString(account))); err != nil {
		t.Fatal(err)
	}

	// seed writes a package owned by a user of the given role. Raw, with the
	// list in the payload, because the question is the READ: a row the pipeline
	// would have written is a row that carries a list. tag keeps one case's
	// rows from being another's.
	seed := func(t *testing.T, tag string, role auth.Role, tiedTo string) (owner, pkg string) {
		t.Helper()
		owner = fmt.Sprintf("v1:identity:user:dd-%s-%s", tag, suffix)
		pkg = fmt.Sprintf("v1:platform:package:dd-%s-%s", tag, suffix)
		ids = append(ids, owner, pkg)
		seedTiePrincipal(t, eng, owner, role)
		payload := fmt.Sprintf(`{"name":%q,"ownerUserId":%q,"sourceKind":"repo","repoUrl":"https://github.com/acme/%s","status":"active","updateAvailable":false,"accountId":%q,"dslDomains":["acme","beta"]}`,
			pkg, owner, memql.BareShortId(pkg), tiedTo)
		if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`insert("v1:platform:package", id=%s, payload=%s)`, langparser.QuoteString(pkg), payload)); err != nil {
			t.Fatalf("seed package: %v", err)
		}
		return owner, pkg
	}
	// borrowed is the owner's authority exactly as the bff borrows it, with the
	// engine's origin stamped on the read.
	borrowed := func(owner string) context.Context {
		return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), owner))
	}

	// A package that ships DSL was deployed by an owner or a developer, and a
	// new package is always tied to an organization. This is the case a gate
	// can exist in, and the tier admits it.
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		t.Run(string(role)+" owner of a package tied to an organization", func(t *testing.T) {
			owner, pkg := seed(t, string(role)+"-tied", role, account)
			row, err := s.packageById(borrowed(owner), pkg)
			if err != nil || row == nil {
				t.Fatalf("the owner's borrowed authority cannot read the package they own: %v (%v). "+
					"Every site of this package would answer 404 to every code, because the bff would see no domains", err, row)
			}
			if list, _ := dslDomainsOf(row); !reflect.DeepEqual(list, []string{"acme", "beta"}) {
				t.Fatalf("dslDomains = %v, want [acme beta]", list)
			}
		})
	}

	// THE BOUNDARY, recorded so it is not rediscovered. The organization
	// conjunct of the tier admits a staff reader (owner, developer, admin) only
	// to rows TIED to an organization, so a borrowed staff owner reads none of
	// their untied packages -- and a package without an organization cannot be
	// written by the pipeline either (organization_required), so it has no
	// list to read. Both fail the same way, closed. If the engine ever lets a
	// borrowed staff owner read an untied row, this is the line that changes.
	t.Run("a staff owner of an untied package reads nothing, and the gate fails closed", func(t *testing.T) {
		owner, pkg := seed(t, "developer-untied", auth.RoleDeveloper, "")
		row, err := s.packageById(borrowed(owner), pkg)
		if err != nil {
			t.Fatal(err)
		}
		if row != nil {
			t.Fatalf("a borrowed developer now reads an untied package they own (%v): the boundary this test records has moved, "+
				"and the access handler's comment on it with it", row)
		}
		// The same owner, acting as themselves, still reads it: the refusal is
		// about borrowed authority, not about the row.
		if own, err := s.packageById(tieActorCtx(owner, auth.RoleDeveloper), pkg); err != nil || own == nil {
			t.Fatalf("the control failed: the developer cannot read their own untied package as themselves: %v (%v)", err, own)
		}
	})

	// The pipeline creates a site under its package's owner, which is every case
	// above. A site that is somebody else's meets the same tier: a staff reader
	// is a standing member of every organization, so a tied package is theirs to
	// read whoever owns it; a person with no standing in it reads nothing.
	t.Run("a staff site owner who does not own the package reads it when it is tied", func(t *testing.T) {
		_, pkg := seed(t, "someone-elses", auth.RoleWriter, account)
		staff, _ := seed(t, "staff-reader", auth.RoleDeveloper, account)
		row, err := s.packageById(borrowed(staff), pkg)
		if err != nil || row == nil {
			t.Fatalf("a developer cannot read a tied package they do not own under borrowed authority: %v (%v)", err, row)
		}
		if list, _ := dslDomainsOf(row); !reflect.DeepEqual(list, []string{"acme", "beta"}) {
			t.Fatalf("dslDomains = %v, want [acme beta]", list)
		}
		stranger, _ := seed(t, "stranger", auth.RoleWriter, "")
		if row, err := s.packageById(borrowed(stranger), pkg); err != nil || row != nil {
			t.Fatalf("a writer with no standing in the organization reads the package (%v, %v), so the read above proves nothing", row, err)
		}
	})

	// A tied package is the owner's only while they are still a member of its
	// organization: ownership alone does not bypass that boundary.
	t.Run("an owner no longer in the package's organization reads nothing", func(t *testing.T) {
		owner, pkg := seed(t, "writer-tied", auth.RoleWriter, account)
		row, err := s.packageById(borrowed(owner), pkg)
		if err != nil {
			t.Fatal(err)
		}
		if row != nil {
			t.Fatalf("a writer outside the package's organization reads it by owning it (%v): the organization boundary has moved", row)
		}
	})
}
