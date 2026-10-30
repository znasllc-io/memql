package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
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
// The offline tests beside this one prove what the pipeline SENDS and when
// (analyze_access_gate_test.go, dsl_domains_deploy_test.go). What they cannot
// prove is what a row DOES with it: that the write
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
		if err := s.recordPackageDslDomains(ownerCtx, pkg, step.record); err != nil {
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

	// THE TWO WRITES ARE SEPARATE, and neither disturbs the other. An analysis
	// writes declares and must leave the list as the last deploy left it; a
	// deploy writes the list and must leave declares as the last analysis found.
	if err := s.recordPackageDslDomains(ownerCtx, pkg, []string{"acme", "realns"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordPackageDeployables(ownerCtx, pkg, declares); err != nil {
		t.Fatal(err)
	}
	row, err := s.packageById(ownerCtx, pkg)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if declared, ok := row["declares"].([]any); !ok || len(declared) != 1 {
		t.Fatalf("declares = %v, want the one deployable the analysis wrote", row["declares"])
	}
	if list, _ := dslDomainsOf(row); !reflect.DeepEqual(list, []string{"acme", "realns"}) {
		t.Fatalf("an analysis's declares write changed the list to %v, want [acme realns] as the deploy left it", list)
	}

	// Every read that returns a package returns the field, because they share
	// the one shape: the OS lists, and the bff's by-id read.
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

// NO CLIENT WRITE CAN NAME THE LIST (the second automated security review of
// Task 9).
//
// recordPackageDslDomains is @serverOnly, and @serverOnly bars the NAMED call
// only. A raw insert at an existing package id read-merges its delta onto the
// stored row, so
//
//	insert("v1:platform:package", id=<pkg>, payload={"dslDomains":["forged"]})
//
// rewrote the list for a cluster owner, a developer and a plain writer in the
// package's organization, on their own package and on a colleague's -- and the
// bff opens an access code only for a gate whose domain is in that list.
// @serverWritten would shut it, but a package has client-reachable writes (the
// compose flow, the OS's own settings), so the guard is on the field.
//
// What this pins is the guard's whole shape, not only the refusal: the refused
// write leaves the row as it was; a client's other writes to the same row are
// not touched by it; a client that re-sends the whole row without naming the
// field cannot change it, because the read-merge keeps what the delta omits;
// and the deploy's own write still lands.
func TestNoClientWriteCanNameTheDomainsAPackageShips(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	account := "dd-guard-acct-" + suffix
	if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "dd guard")`, langparser.QuoteString(account))); err != nil {
		t.Fatal(err)
	}
	group := "dd-guard-group-" + suffix
	seedTieGroup(t, eng, group, account)
	ids = append(ids, group)

	// person writes a user of the given role. A writer reaches a tied package
	// only through the organization, so a writer is a member of it; staff are
	// standing members of every organization by rule.
	person := func(t *testing.T, tag string, role auth.Role) string {
		t.Helper()
		user := fmt.Sprintf("v1:identity:user:dd-guard-%s-%s", tag, suffix)
		ids = append(ids, user)
		seedTiePrincipal(t, eng, user, role)
		if role == auth.RoleWriter {
			seedTieMembership(t, eng, group, user)
		}
		return user
	}
	// seed writes a package tied to the organization with a list a deploy would
	// have left, under the engine's own origin: the question is whether a CLIENT
	// can change it.
	seed := func(t *testing.T, tag, owner string) string {
		t.Helper()
		pkg := fmt.Sprintf("v1:platform:package:dd-guard-%s-%s", tag, suffix)
		ids = append(ids, pkg)
		payload := fmt.Sprintf(`{"name":%q,"ownerUserId":%q,"sourceKind":"repo","repoUrl":"https://github.com/acme/%s","status":"active","updateAvailable":false,"accountId":%q,"dslDomains":["real"]}`,
			pkg, owner, memql.BareShortId(pkg), account)
		if _, err := eng.Execute(tieSeederCtx(), fmt.Sprintf(`insert("v1:platform:package", id=%s, payload=%s)`, langparser.QuoteString(pkg), payload)); err != nil {
			t.Fatalf("seed package: %v", err)
		}
		return pkg
	}
	// stored is the list on the latest stored version of the row, read straight
	// off the table: the answer to "what is the list", with no read tier between.
	// A row that was never written has none, which is not a failure to read.
	stored := func(t *testing.T, pkg string) []string {
		t.Helper()
		var node concept.MemoryNode
		err := db.NewSelect().Model(&node).Where("concept = ?", "v1:platform:package").Where("id = ?", pkg).
			OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background())
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			t.Fatalf("read back %s: %v", pkg, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return rowStrings(payload, "dslDomains")
	}
	raw := func(ctx context.Context, pkg, payload string) error {
		_, err := eng.Execute(ctx, fmt.Sprintf(`insert("v1:platform:package", id=%s, payload=%s)`, langparser.QuoteString(pkg), payload))
		return err
	}

	colleague := person(t, "colleague", auth.RoleDeveloper)
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleWriter} {
		actor := person(t, string(role), role)
		ctx := tieActorCtx(actor, role)
		for whose, owner := range map[string]string{"their own package": actor, "a colleague's package": colleague} {
			t.Run(string(role)+" on "+whose, func(t *testing.T) {
				pkg := seed(t, string(role)+"-"+strings.ReplaceAll(whose, " ", "-"), owner)

				// THE FORGERY, by the door @serverOnly does not guard.
				for _, forged := range []string{`["forged-raw"]`, `[]`, `null`} {
					err := raw(ctx, pkg, `{"dslDomains":`+forged+`}`)
					if err == nil {
						t.Fatalf("a %s's raw insert of dslDomains=%s on %s landed", role, forged, whose)
					}
					// The GUARD's refusal, not a schema's that happens to name the
					// field: it is the sentence that tells a person what to remove.
					if !strings.Contains(err.Error(), "`dslDomains` refused") || !strings.Contains(err.Error(), "written only by a package deploy or rollback") {
						t.Fatalf("the write was refused, but not by the package guard: %v", err)
					}
					if got := stored(t, pkg); !reflect.DeepEqual(got, []string{"real"}) {
						t.Fatalf("after the refused write of %s the list is %v, want [real]", forged, got)
					}
				}
				// ...and by the door it does: the named mutation.
				if _, err := eng.Execute(ctx, fmt.Sprintf(`mutation recordPackageDslDomains(packageId: %s, dslDomains: ["forged-named"])`, langparser.QuoteString(pkg))); err == nil {
					t.Fatalf("a %s called the @serverOnly mutation", role)
				}

				// A CLIENT'S OTHER WRITES TO THE ROW ARE NOT THE GUARD'S. Naming the
				// field is what is refused, not touching the row. (latestKnownVersion
				// because no organization capability is held against it: a plain
				// writer's write to the row is refused by those, which is not this.)
				if err := raw(ctx, pkg, `{"latestKnownVersion":"sha-unrelated"}`); err != nil {
					t.Fatalf("an unrelated write to the row was refused: %v", err)
				}
				// A CLIENT THAT RE-SENDS THE ROW WITHOUT THE FIELD cannot change it:
				// the read-merge keeps what the delta omits.
				resend := fmt.Sprintf(`{"name":%q,"ownerUserId":%q,"sourceKind":"repo","repoUrl":"https://github.com/acme/%s","status":"active","updateAvailable":false,"accountId":%q}`,
					pkg, owner, memql.BareShortId(pkg), account)
				if err := raw(ctx, pkg, resend); err != nil {
					t.Fatalf("a whole-row write that omits the field was refused: %v", err)
				}
				if got := stored(t, pkg); !reflect.DeepEqual(got, []string{"real"}) {
					t.Fatalf("a client that re-sent the row without the field left the list %v, want [real]", got)
				}
			})
		}
	}

	// A new row is no door either: a client creating a package cannot give it a
	// list before any deploy has.
	t.Run("a client cannot create a package that already has a list", func(t *testing.T) {
		actor := person(t, "creator", auth.RoleDeveloper)
		pkg := fmt.Sprintf("v1:platform:package:dd-guard-created-%s", suffix)
		ids = append(ids, pkg)
		payload := fmt.Sprintf(`{"name":%q,"ownerUserId":%q,"sourceKind":"repo","repoUrl":"https://github.com/acme/created-%s","status":"active","updateAvailable":false,"accountId":%q,"dslDomains":["forged-new"]}`,
			pkg, actor, suffix, account)
		if err := raw(tieActorCtx(actor, auth.RoleDeveloper), pkg, payload); err == nil || !strings.Contains(err.Error(), "`dslDomains` refused") {
			t.Fatalf("a client creating a package carrying a list was not refused by the package guard: %v", err)
		}
		if got := stored(t, pkg); len(got) != 0 {
			t.Fatalf("the refused create left a row with the list %v", got)
		}
	})
}

// The deploy's own write is the other half of the guard: a field no client can
// name is still one the pipeline writes, under the engine's origin.
func TestTheDeploysOwnWriteOfTheDomainsStillLandsOnceClientsAreRefused(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "v1:identity:user:dd-own-" + suffix
	pkg := "v1:platform:package:dd-own-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{pkg, owner} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	seedTiePrincipal(t, eng, owner, auth.RoleOwner)
	ownerCtx := tieActorCtx(owner, auth.RoleOwner)
	createTiedPackage(t, eng, ownerCtx, pkg, "dd-own-"+suffix, "dd-own-acct-"+suffix)
	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}

	// The same person, as a client: refused.
	if _, err := eng.Execute(ownerCtx, fmt.Sprintf(`insert("v1:platform:package", id=%s, payload={"dslDomains":["forged"]})`, langparser.QuoteString(pkg))); err == nil {
		t.Fatal("setup: the owner's raw insert of the list landed")
	}
	// ...and as the deploy that records what it made live: lands.
	if err := s.recordPackageDslDomains(ownerCtx, pkg, []string{"acme", "beta"}); err != nil {
		t.Fatalf("the deploy's own write was refused: %v", err)
	}
	row, err := s.packageById(ownerCtx, pkg)
	if err != nil || row == nil {
		t.Fatalf("read the package: %v (%v)", err, row)
	}
	if list, _ := dslDomainsOf(row); !reflect.DeepEqual(list, []string{"acme", "beta"}) {
		t.Fatalf("dslDomains = %v, want the deploy's [acme beta]", list)
	}
}

// A ROLLBACK RESTORES THE LIST FROM A REPORT THAT REALLY WENT THROUGH THE
// STORE. The offline rollback cases hand it a report written as a Go map, which
// proves the arithmetic and nothing about the row: whether `namespaces` -- a
// field of a field of an object field -- comes back out of the engine's shape,
// and whether a developer who does not own the package can read the deployment
// they are rolling back to. Either failing is silent: the list restored is the
// empty one, and every access code on the package's sites answers 404.
//
// So this is the whole chain on real rows: a real analysis of a tree with a
// pinned namespace, its report recorded by the real store, the run closed as
// succeeded, then a rollback by a developer, and the package read back.
func TestARollbackRestoresTheDomainsFromTheReportItsDeploymentRecordedOnRealRows(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	owner := "v1:identity:user:dd-rb-owner-" + suffix
	developer := "v1:identity:user:dd-rb-dev-" + suffix
	pkg := "v1:platform:package:dd-rb-" + suffix
	deployment := "dd-rb-run-" + suffix
	account := "dd-rb-acct-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{pkg, owner, developer, deployment, "v1:platform:packageDeployment:" + deployment} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	seedTiePrincipal(t, eng, owner, auth.RoleDeveloper)
	seedTiePrincipal(t, eng, developer, auth.RoleDeveloper)
	ownerCtx := tieActorCtx(owner, auth.RoleDeveloper)
	developerCtx := tieActorCtx(developer, auth.RoleDeveloper)
	createTiedPackage(t, eng, ownerCtx, pkg, "dd-rb-"+suffix, account)
	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}

	// A pinned directory, because the spelling is the point: the pin is what
	// the concept ids say, so it is what the list must say.
	tree := validPackage()
	tree["dsl/pinneddir/concepts.memql"] = file(validConcepts)
	tree["dsl/pinneddir/memql.toml"] = file(languageLine)
	tree["dsl/pinneddir/namespace.pin"] = file("realns\n")
	rep, err := Analyze(tree, Options{SourceVersion: "sha-1"})
	if err != nil {
		t.Fatalf("analysis: %v", err)
	}
	if want := []string{"acme", "realns"}; !reflect.DeepEqual(dslDomainsFrom(rep), want) {
		t.Fatalf("setup: the analysis ships %v, want %v", dslDomainsFrom(rep), want)
	}

	if err := s.openDeployment(context.Background(), deploymentSeed{
		DeploymentId: deployment, PackageId: pkg, OwnerUserId: owner, AccountId: account, RequestedBy: owner, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("open the run: %v", err)
	}
	if err := s.recordReport(ownerCtx, deployment, rep, ""); err != nil {
		t.Fatalf("record the report: %v", err)
	}
	if err := s.closeDeployment(ownerCtx, deploymentClose{
		DeploymentId: deployment, Status: StatusSucceeded,
		DslVersion: "packages/acme/0123456789abcdef/ packages/pinneddir/0123456789abcdef/",
		FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("close the run: %v", err)
	}

	// The deployment as the person rolling back to it reads it.
	row, err := s.deploymentById(developerCtx, deployment)
	if err != nil || row == nil {
		t.Fatalf("a developer cannot read the deployment they are rolling back to: %v (%v)", err, row)
	}
	if got := dslDomainsFrom(reportFromRow(row)); !reflect.DeepEqual(got, []string{"acme", "realns"}) {
		t.Fatalf("the list read back out of the recorded report is %v, want [acme realns]: "+
			"the report's namespaces did not survive the row's shape", got)
	}

	// The cluster is on a newer tree, so the rollback has DSL to put back.
	stager := &fakeStager{active: map[string]string{"acme": "packages/acme/ffffffffffffffff/"}}
	roller := &fakeRoller{}
	deps := &Deps{Store: s, Stager: stager, Roller: roller, Publisher: &fakePublisher{}, Logger: discardLogger()}
	if _, err := Rollback(developerCtx, deps, RollbackRequest{
		PackageId: pkg, DeploymentId: deployment, Actor: Actor{UserId: developer, MayDeployDsl: true},
	}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if roller.rolls != 1 {
		t.Fatalf("setup: the rollback rolled %d times, want 1", roller.rolls)
	}

	after, err := s.packageById(ownerCtx, pkg)
	if err != nil || after == nil {
		t.Fatalf("read the package: %v (%v)", err, after)
	}
	if list, _ := dslDomainsOf(after); !reflect.DeepEqual(list, []string{"acme", "realns"}) {
		t.Fatalf("after the rollback the package ships %v, want [acme realns] restored from the deployment's own report", list)
	}
}
