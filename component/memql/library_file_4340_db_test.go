package memql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// library_file_4340_db_test.go -- the Library file row and the artifact
// index tier, end to end against a real store (memql#4340).
//
// Postgres-gated like its neighbours: sharedReadMergeEngine skips when no
// database is reachable, and CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1 so a skip there is a failure rather than a green.
//
// Four claims, and every one of them is unmeasurable without a store:
//
//  1. Promotion is IDEMPOTENT. createArtifact derives the row id from
//     sourceConceptRef, so promoting a file twice must version one row
//     rather than list two. A unit test over a stub cannot prove this --
//     the derivation is a DSL expression, and a stub that reproduced it
//     in Go would be asserting its own copy.
//  2. ARCHIVE hides the artifact from the default list -- and, separately,
//     that the predicate doing the hiding does not ALSO hide every row
//     written before the field existed. `archived != true` and
//     `archived == false` read as interchangeable and are not: they
//     disagree about a payload with no `archived` member, which is what
//     every artifact row promoted before memql#4340 is. Only Postgres
//     answers which way, so the answer is pinned here rather than reasoned
//     about (it keeps them; `== false` would not).
//  3. A SECOND USER sees none of it -- through the named reads AND
//     through a raw query string, which is the path with no declared
//     binding for the tier to be resolved from and therefore the one the
//     per-row gate exists for.
//  4. A CLUSTER OWNER sees all of it. That is the composite half of the
//     tier (memql#4312) and it is observable ONLY on the unbound path:
//     every named read here also carries an authored
//     `ownerUserId==actor.userId` conjunct, which ANDs with the injected
//     term and keeps a cluster owner scoped to their own rows. Asserting
//     the bypass on `libraryFilesForOwner()` would assert nothing.

// libraryFileArgs is one uploaded markdown file, minus the ids the caller
// supplies per test.
func libraryFileArgs(fileId, owner string) map[string]any {
	return map[string]any{
		"fileId":   fileId,
		"name":     "notes.md",
		"mimeType": "text/markdown",
		"size":     1234,
		"sha256":   "6dcd4ce23d88e2ee9568ba546c007c63d9131c1b",
		"blobUrl":  "library/" + owner + "/" + fileId + "/notes.md",
		"source":   "uploaded",
		"format":   "markdown",
	}
}

// TestLibraryFileIsOwnedAndPromotionIsIdempotent covers claims 1 and 3
// for the file row plus the index row it is promoted to.
func TestLibraryFileIsOwnedAndPromotionIsIdempotent(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4340own")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	fileId := "libfile-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	// createLibraryFile stamps ownerUserId from actor.userId, so the row
	// is genuinely A's -- no argument names an owner.
	runMutation(t, ctxA, eng, "createLibraryFile", libraryFileArgs(fileId, userA))

	// --- A reads their own file, through both named reads ---
	for name, q := range map[string]string{
		"by id":     fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId),
		"for owner": `libraryFilesForOwner()`,
	} {
		res, err := eng.Execute(ctxA, q)
		if err != nil {
			t.Fatalf("A reading %s: %v", name, err)
		}
		if blob := resultBlob(t, res); !strings.Contains(blob, fileId) {
			t.Fatalf("A cannot read their own file through %s: %s", name, blob)
		}
	}

	// --- B sees nothing, through the named reads and the raw path ---
	for name, q := range map[string]string{
		"named by id":     fmt.Sprintf(`libraryFileById(fileId: "%s")`, fileId),
		"named for owner": `libraryFilesForOwner()`,
		"raw by id":       fmt.Sprintf(`row.id=="v1:library:file:%s"`, fileId),
		"raw by concept":  `concept=="v1:library:file"`,
	} {
		res, err := eng.Execute(ctxB, q)
		if err != nil {
			// A refusal is an acceptable outcome; being shown the row is not.
			t.Logf("B reading %s refused: %v", name, err)
			continue
		}
		if blob := resultBlob(t, res); strings.Contains(blob, fileId) {
			t.Fatalf("caller B was returned caller A's library file through %s. "+
				"v1:library:file declares @rowAuthz(owner=\"ownerUserId\", clusterOwner) and "+
				"B is neither.\n  B saw: %s", name, blob)
		}
	}

	// --- Promotion is idempotent ---
	//
	// The artifact id is concat("artifact-", hash(sourceConceptRef)), so a
	// second promotion of the same file versions the SAME row. Called
	// twice with the identical source ref, exactly as re-running
	// indexFileOnCreate would.
	sourceRef := "v1:library:file:" + fileId
	promote := map[string]any{
		"sourceConceptRef": sourceRef,
		"ownerUserId":      userA,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "notes.md",
		"format":           "markdown",
		"mimeType":         "text/markdown",
	}
	firstId := runMutation(t, ctxA, eng, "createArtifact", promote)
	secondId := runMutation(t, ctxA, eng, "createArtifact", promote)
	if firstId != secondId {
		t.Fatalf("promoting the same file twice produced two artifact ids (%s, %s) -- the id is "+
			"derived from sourceConceptRef precisely so a re-run versions one row",
			firstId, secondId)
	}

	res, err := eng.Execute(ctxA, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("A reading libraryArtifacts(): %v", err)
	}
	blob := resultBlob(t, res)
	if n := strings.Count(blob, sourceRef); n != 1 {
		t.Fatalf("libraryArtifacts() lists the source ref %s %d times after two promotions, "+
			"want exactly 1 -- promotion must be idempotent per source ref.\n  saw: %s",
			sourceRef, n, blob)
	}
}

// TestArchiveHidesTheArtifactAndArchivesTheFile is claim 2, in both
// halves: the index row drops out of the default list, and the backing
// file carries the archive too.
func TestArchiveHidesTheArtifactAndArchivesTheFile(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4340arch")
	userA := "user-a-" + suffix
	keptId := "libfile-kept-" + suffix
	goneId := "libfile-gone-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	runMutation(t, ctxA, eng, "createLibraryFile", libraryFileArgs(keptId, userA))
	runMutation(t, ctxA, eng, "createLibraryFile", libraryFileArgs(goneId, userA))

	promote := func(fileId string) string {
		return runMutation(t, ctxA, eng, "createArtifact", map[string]any{
			"sourceConceptRef": "v1:library:file:" + fileId,
			"ownerUserId":      userA,
			"lens":             "artifact",
			"kind":             "file",
			"source":           "uploaded",
			"title":            fileId,
			"format":           "markdown",
			"mimeType":         "text/markdown",
		})
	}
	keptArtifact := promote(keptId)
	goneArtifact := promote(goneId)

	// Both are in the default list before the archive. Asserted rather
	// than assumed: without it, an archive test passes for the wrong
	// reason on a list that was already empty.
	before, err := eng.Execute(ctxA, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("libraryArtifacts() before archive: %v", err)
	}
	beforeBlob := resultBlob(t, before)
	for _, id := range []string{keptArtifact, goneArtifact} {
		if !strings.Contains(beforeBlob, id) {
			t.Fatalf("artifact %s is missing from libraryArtifacts() BEFORE any archive -- the "+
				"archive assertion below would then prove nothing.\n  saw: %s", id, beforeBlob)
		}
	}

	runMutation(t, ctxA, eng, "archiveArtifact", map[string]any{"artifactId": goneArtifact})

	after, err := eng.Execute(ctxA, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("libraryArtifacts() after archive: %v", err)
	}
	afterBlob := resultBlob(t, after)
	if strings.Contains(afterBlob, goneArtifact) {
		t.Fatalf("the archived artifact %s is still in the default Library list -- "+
			"libraryArtifacts()'s `archived != true` conjunct is what makes archiveArtifact a "+
			"soft delete rather than a flag nothing reads.\n  saw: %s", goneArtifact, afterBlob)
	}
	if !strings.Contains(afterBlob, keptArtifact) {
		t.Fatalf("archiving one artifact removed the OTHER one (%s) from the default list -- "+
			"`archived != true` must exclude only rows carrying archived=true.\n  saw: %s",
			keptArtifact, afterBlob)
	}

	// The archived row is still readable by id -- soft, not destroyed.
	byId, err := eng.Execute(ctxA, fmt.Sprintf(`libraryArtifactById(artifactId: "%s")`, goneArtifact))
	if err != nil {
		t.Fatalf("libraryArtifactById on the archived row: %v", err)
	}
	if blob := resultBlob(t, byId); !strings.Contains(blob, goneArtifact) {
		t.Fatalf("the archived artifact is unreachable by id -- archive is a soft delete and the "+
			"row, its labels and its provenance all survive.\n  saw: %s", blob)
	}

	// And the backing file archives with it. The cascade in production is
	// archiveFileOnArtifactArchive, a node.updated trigger on the index
	// row; this engine runs no automation scheduler, so what is asserted
	// here is the mutation it drives -- CALLED THE WAY THE AUTOMATION
	// CALLS IT.
	//
	// That distinction is the point of the argument below. The automation
	// has only the artifact's `sourceConceptRef` to pass, which is the
	// CANONICAL "v1:library:file:<id>" the promotion wrote, not the bare
	// id every other caller uses. If update() did not resolve that
	// spelling to the same row, the cascade would be a silent no-op --
	// the artifact would archive, the file would not, and nothing would
	// report it. Passing the bare id here would test a call nothing makes.
	runMutation(t, ctxA, eng, "archiveLibraryFile", map[string]any{
		"fileId": "v1:library:file:" + goneId,
	})
	fileRes, err := eng.Execute(ctxA, fmt.Sprintf(`libraryFileById(fileId: "%s")`, goneId))
	if err != nil {
		t.Fatalf("libraryFileById after archive: %v", err)
	}
	if blob := resultBlob(t, fileRes); !strings.Contains(blob, `"archived":true`) {
		t.Fatalf("archiveLibraryFile did not set archived on the backing file row.\n  saw: %s", blob)
	}

	// The read-merge kept every byte fact: an archive must not blank the
	// row it archives.
	if blob := resultBlob(t, fileRes); !strings.Contains(blob, "notes.md") {
		t.Fatalf("archiveLibraryFile lost the file's name -- update{} is a read-merge and the "+
			"minimal argument list must leave every other field standing.\n  saw: %s", blob)
	}
}

// TestClusterOwnerSeesEveryLibraryRow is claim 4: the composite half of
// the tier.
//
// Measured on the UNBOUND path deliberately. Every named read over these
// concepts also carries an authored `ownerUserId==actor.userId` conjunct,
// which ANDs with the injected term -- so a cluster owner calling
// libraryFilesForOwner() correctly sees only their own rows, and a test
// that asserted the bypass there would be asserting the authored filter,
// not the tier. A raw query string has no declared binding at all, so the
// per-row admission gate is the only thing deciding, and that gate is
// where ClusterOwnerBypass lives.
func TestClusterOwnerSeesEveryLibraryRow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4340admin")
	userA := "user-a-" + suffix
	fileId := "libfile-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxOwner := auth.ContextWithToken(
		auth.ContextWithAccess(t.Context(), &auth.AccessContext{
			UserId: "cluster-owner-" + suffix,
			Role:   auth.RoleOwner,
		}),
		&auth.TokenInfo{Subject: "cluster-owner-" + suffix},
	)

	runMutation(t, ctxA, eng, "createLibraryFile", libraryFileArgs(fileId, userA))

	// The CONTROL: a plain writer who owns nothing is denied the same
	// row through the same spelling. Without it, a passing owner read
	// could mean "the gate admits everyone", which is the failure this
	// test would otherwise call a success.
	ctxStranger := rowAuthzCallerCtx("user-c-" + suffix)
	strangerRes, err := eng.Execute(ctxStranger, `concept=="v1:library:file"`)
	if err == nil {
		if blob := resultBlob(t, strangerRes); strings.Contains(blob, fileId) {
			t.Fatalf("a non-owner, non-cluster-owner caller reached the row through the raw "+
				"path, so the owner assertion below proves nothing: %s", blob)
		}
	}

	ownerRes, err := eng.Execute(ctxOwner, `concept=="v1:library:file"`)
	if err != nil {
		t.Fatalf("cluster owner reading v1:library:file: %v", err)
	}
	if blob := resultBlob(t, ownerRes); !strings.Contains(blob, fileId) {
		t.Fatalf("a cluster owner cannot see another user's library file. That is the whole "+
			"point of the COMPOSITE tier (memql#4312): a plain owner= tier has no cluster-owner "+
			"bypass, so declaring the operator surface plain-owned hides every other user's rows "+
			"from the operator too.\n  owner saw: %s", blob)
	}
}

// TestArchivedFilterKeepsRowsWrittenBeforeTheFieldExisted pins the one
// property that decides whether introducing `archived` was a safe deploy
// or an outage.
//
// Every v1:library:artifact row promoted before memql#4340 has NO
// `archived` member in its payload. libraryArtifacts() filters on
// `archived != true`, and the obvious-looking alternative
// `archived == false` reads as the same predicate to anyone scanning the
// file. It is not: they disagree on exactly those rows, and the equality
// form would empty every existing Library on the deploy that added the
// field -- silently, with no error and no clue in the query.
//
// So the row is written the only way that reproduces the pre-4340 shape:
// a raw insert(), which short-circuits the mutation template and writes
// the payload AS SUPPLIED, so no `archived` member is stamped. A row
// written through createArtifact would carry the field and could not
// measure this.
func TestArchivedFilterKeepsRowsWrittenBeforeTheFieldExisted(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	suffix := uniqueSuffix("library4340legacy")
	userA := "user-a-" + suffix
	ctxA := rowAuthzCallerCtx(userA)
	sourceRef := "v1:library:file:legacy-" + suffix

	raw := fmt.Sprintf(
		`insert("v1:library:artifact", payload={"sourceConceptRef": %q, "ownerUserId": %q, `+
			`"lens": "artifact", "kind": "file", "source": "uploaded", "title": "pre-4340", `+
			`"updatedAt": "2026-01-01T00:00:00Z"}, id="artifact-legacy-%s")`,
		sourceRef, userA, suffix)
	if _, err := eng.Execute(ctxA, raw); err != nil {
		t.Fatalf("raw insert of a pre-4340 shaped row: %v", err)
	}

	res, err := eng.Execute(ctxA, `libraryArtifacts()`)
	if err != nil {
		t.Fatalf("libraryArtifacts(): %v", err)
	}
	if blob := resultBlob(t, res); !strings.Contains(blob, sourceRef) {
		t.Fatalf("a row with NO `archived` member has dropped out of libraryArtifacts(). "+
			"That is every artifact promoted before memql#4340, so this is an empty Library "+
			"for every existing user on the deploy that introduced the field. The filter must "+
			"stay `archived != true`; `archived == false` is the spelling that does this.\n"+
			"  saw: %s", blob)
	}
}
