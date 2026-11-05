package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// brandkit_db_test.go -- an organization's brand kit against real engines: the
// parts brandkit_test.go's fake cannot say. Two engines share one database, as
// two replicas do, each with its own worker registered as the integration, so
// the exact-revision check and the gate are exercised across them, the concept's
// own guards answer raw writes, and the account tier decides who reads the kit.

func TestBrandKitAcrossReplicasAndOrganizations(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	org, rivalOrg := w.suffix+"-org", w.suffix+"-rival-org"
	w.createOrganization(t, org, "Brand client")
	w.createOrganization(t, rivalOrg, "Rival client")
	for name, role := range map[string]string{"colleague": "writer", "viewer": "reader", "rival": "writer"} {
		w.seedUser(t, w.suffix+"-"+name, role)
	}
	w.seedMember(t, w.suffix+"-colleague", org)
	w.seedMember(t, w.suffix+"-viewer", org)
	w.seedMember(t, w.suffix+"-rival", rivalOrg)
	colleague := layoutActor(w.suffix+"-colleague", auth.RoleWriter)
	viewer := layoutActor(w.suffix+"-viewer", auth.RoleReader)
	rival := layoutActor(w.suffix+"-rival", auth.RoleWriter)

	save := func(r int, ctx context.Context, account string, kit map[string]any, expected string) (string, error) {
		t.Helper()
		nodes, err := w.workers[r].handleSaveBrandKit(ctx, map[string]any{"accountId": account, "kit": kit, "expectedRevision": expected}, 0)
		if err != nil {
			return "", err
		}
		return decodeBrandAnswer(t, nodes, "campaignBrandKitSaved").Revision, nil
	}
	mustSave := func(r int, ctx context.Context, kit map[string]any, expected string) string {
		t.Helper()
		revision, err := save(r, ctx, org, kit, expected)
		if err != nil {
			t.Fatalf("save on replica %d: %v", r, err)
		}
		return revision
	}
	// read is the kit as a caller reads it on a replica, from the database: each
	// engine keeps its own result cache.
	read := func(r int, ctx context.Context) (emaildoc.BrandKit, string, bool) {
		t.Helper()
		kit, revision, ok, err := w.workers[r].brandKitFor(memql.ContextWithFreshRead(ctx), org)
		if err != nil {
			t.Fatalf("brandKitFor on replica %d: %v", r, err)
		}
		return kit, revision.UTC().Format(time.RFC3339Nano), ok
	}

	// Created on one replica; the same call retried on the other (an uncertain
	// response is retried) is the same revision, not a second version.
	kit := testBrandKit(t)
	first := mustSave(0, w.owner, kit, "")
	if retry := mustSave(1, w.owner, kit, ""); retry != first {
		t.Fatalf("a creation retry on the other replica wrote another version: %s then %s", first, retry)
	}
	// What was stored reads back whole on the other replica, for a colleague.
	got, revision, ok := read(1, colleague)
	if !ok || revision != first || got.Follow != "Find us online" || got.Signature == nil || got.Signature.NameText != "Ada Lovelace" {
		t.Fatalf("the colleague reads ok=%v revision=%s kit=%+v", ok, revision, got)
	}

	// A client cannot write the row any other way: a raw insert meets the
	// concept's @serverWritten guard, and the mutations are server-only.
	_, err := w.engines[0].Execute(w.owner, fmt.Sprintf(`insert("v1:campaigns:brandKit", id=%s, payload={"accountId":%s,"kit":{}})`,
		langparser.QuoteString(brandKitID(org)), langparser.QuoteString(org)))
	if err == nil || !strings.Contains(err.Error(), "@serverWritten") {
		t.Fatalf("a raw insert was not refused by the concept: %v", err)
	}
	bypass, err := langparser.RenderCall("updateBrandKit", map[string]any{
		"brandKitId": brandKitID(org), "kit": canonicalKit(t, kit), "versionTime": "2031-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engines[0].Execute(w.owner, "mutation "+bypass); err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Fatalf("a client called the server-only mutation: %v", err)
	}
	if _, revision, _ := read(0, w.owner); revision != first {
		t.Fatalf("a refused write changed the kit: revision %s", revision)
	}

	// Two replicas saving from the same revision: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := range w.workers {
		changed := testBrandKit(t)
		changed["follow"] = fmt.Sprintf("Replica %d", n)
		wg.Add(1)
		go func(n int, changed map[string]any) {
			defer wg.Done()
			_, err := save(n, w.owner, org, changed, first)
			results <- err
		}(n, changed)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case err.Error() == errBrandKitChanged.Error():
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("got %d successes and %d conflicts", successes, conflicts)
	}
	_, current, _ := read(1, w.owner)
	if currentAt, firstAt := templateTime(current), templateTime(first); !currentAt.After(firstAt) {
		t.Fatalf("the winner was stamped %s, not after %s", current, first)
	}

	// A colleague saves with the current revision; whoever first saved the kit
	// stays its owner. A reader may read it, and not save it.
	edit := testBrandKit(t)
	edit["follow"] = "Edited by a colleague"
	edited := mustSave(1, colleague, edit, current)
	row, found, err := w.workers[0].store.BrandKitForAccount(memql.ContextWithFreshRead(w.owner), org)
	if err != nil || !found || bare(str(row, "ownerUserId")) != w.ownerID || bare(str(row, "accountId")) != org {
		t.Fatalf("a colleague's save changed the owner or the organization: %v %v %v", row, found, err)
	}
	if got, revision, ok := read(0, viewer); !ok || revision != edited || got.Follow != "Edited by a colleague" {
		t.Fatalf("the reader reads ok=%v revision=%s follow=%q", ok, revision, got.Follow)
	}
	if _, err := save(0, viewer, org, testBrandKit(t), edited); err == nil || err.Error() != errBrandNeedsWrite.Error() {
		t.Fatalf("a reader saved the kit: %v", err)
	}

	// A member of another organization reads no kit and cannot save it, naming
	// either organization; their own organization's kit is a different row.
	if _, _, ok, err := w.workers[1].brandKitFor(memql.ContextWithFreshRead(rival), org); ok || err != nil {
		t.Fatalf("a member of another organization read the kit: ok=%v err=%v", ok, err)
	}
	if _, err := save(1, rival, org, testBrandKit(t), edited); err == nil {
		t.Fatal("a member of another organization saved its kit")
	}
	if _, err := save(1, rival, org, testBrandKit(t), ""); err == nil {
		t.Fatal("a member of another organization created over its kit")
	}
	if _, err := save(1, rival, rivalOrg, testBrandKit(t), ""); err != nil {
		t.Fatalf("a member could not save their own organization's kit: %v", err)
	}
	// What the engine itself answers, apart from the handler: the kit's id with
	// the rival's own organization, even with internal origin and every argument.
	overwrite, err := langparser.RenderCall("createBrandKit", map[string]any{
		"brandKitId": brandKitID(org), "accountId": rivalOrg, "kit": canonicalKit(t, testBrandKit(t)), "versionTime": "2031-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engines[1].Execute(auth.ContextWithInternalOrigin(rival), "mutation "+overwrite); err == nil {
		t.Error("the engine let a member of another organization overwrite a kit by id")
	} else {
		t.Logf("the engine refuses the overwrite: %v", err)
	}
	if got, revision, _ := read(0, w.owner); revision != edited || got.Follow != "Edited by a colleague" {
		t.Fatalf("the kit changed after the attempts from outside: revision %s follow %q", revision, got.Follow)
	}

	// The builtins run the whole way through the engine, as a client calls them:
	// the declaration, the executor name and the handler.
	saveCall, err := langparser.RenderCall("campaignSaveBrandKit", map[string]any{"accountId": org, "kit": testBrandKit(t), "expectedRevision": edited})
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(w.run(t, w.owner, "builtin "+saveCall))
	if len(rows) != 1 || str(rows[0], "revision") == "" || rows[0]["warnings"] == nil {
		t.Fatalf("campaignSaveBrandKit answered %v", rows)
	}
	previewCall, err := langparser.RenderCall("campaignBrandPreview", map[string]any{"accountId": org, "kit": testBrandKit(t), "layoutId": "builtin:letter"})
	if err != nil {
		t.Fatal(err)
	}
	rows = memql.MaterializeRows(w.run(t, viewer, "builtin "+previewCall))
	if len(rows) != 1 || !strings.Contains(str(rows[0], "html"), "Founder of Acme") {
		raw, _ := json.Marshal(rows)
		t.Fatalf("campaignBrandPreview answered %.300s", raw)
	}
}
