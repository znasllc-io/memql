package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCatalogOCIReplicaSealHistoryAndRoleBoundary(t *testing.T) {
	db := candidateTestDB(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	p, c, _, _ := candidatePublicationFixture(t, db, server.URL)
	request := preparePublication(t, db, p, c)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	trust := map[string]ed25519.PublicKey{"fixture-key": pub}
	seal := func(l *candidateLedger) ([]byte, error) {
		return l.sealCatalog(ownerCtx(), request.CandidateID, request.ApprovalID, "fixture", "fixture-key", key, trust)
	}
	if _, err := seal(p.ledger); err == nil {
		t.Fatal("unpublished candidate sealed")
	}
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	// A changed operator configuration cannot rewrite historical addresses.
	original := p.targetReader.targets["registry"]
	changed := original
	changed.Origin = "https://different.example"
	p.targetReader.targets["registry"] = changed
	var wg sync.WaitGroup
	results := make(chan []byte, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			otherDB := candidateTestDB(t)
			b, e := seal(&candidateLedger{db: func() *sql.DB { return otherDB }})
			if e != nil {
				failures <- e
			} else {
				results <- b
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	var first []byte
	for b := range results {
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Fatal("replica changed catalog envelope")
		}
	}
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper} {
		v, e := PublishedCatalog(actorContext(role), db, request.CandidateID, "fixture", trust)
		if e != nil {
			t.Fatal(role, e)
		}
		r, e := v.Release()
		if e != nil {
			t.Fatal(e)
		}
		if r.Components[0].Artifacts[0].Locations[0].Origin != original.Origin {
			t.Fatal("catalog followed new configuration")
		}
	}
	// A fresh serving node has only its database and public trust configuration.
	// Reads must not resolve a signing secret, candidate config or Library store.
	cfgBytes, err := json.Marshal(catalogConfiguration{FormatVersion: 1, Publisher: "fixture", PublicKeys: map[string]string{"fixture-key": base64.StdEncoding.EncodeToString(pub)}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewIntegration(nil, &tripwireEngine{t: t}, resolver{
		systemVariable: func(ctx context.Context, name string) (string, error) {
			if name != ReleaseCatalogConfigurationVariable || !memql.FreshReadFromContext(ctx) {
				t.Fatal("catalog configuration was not a fresh exact read")
			}
			return string(cfgBytes), nil
		},
		systemSecret: func(context.Context, string) (string, error) {
			t.Fatal("catalog read requested secrets")
			return "", nil
		},
	})
	reader.candidateDB = func() *sql.DB { return db }
	if _, err := reader.handleCatalogGet(actorContext(auth.RoleDeveloper), map[string]any{"candidateId": request.CandidateID}, 0); err != nil {
		t.Fatal("public catalog handler required private dependencies", err)
	}
	if _, err := reader.handleCatalogList(actorContext(auth.RoleAdmin), map[string]any{"limit": 1}, 0); err != nil {
		t.Fatal("public discovery required private dependencies", err)
	}
	rollback, err := os.ReadFile("../../component/database/memory-nodes/migrations/20261007060000_release_catalog.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(t.Context(), string(rollback))
	_ = tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "release catalog evidence is not empty") {
		t.Fatal("schema rollback lost publication evidence", err)
	}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleReader), actorContext(auth.RoleWriter)} {
		if _, err := PublishedCatalog(ctx, nil, request.CandidateID, "fixture", trust); err == nil {
			t.Fatal("unauthorized catalog read")
		}
	}
	page, err := listCatalog(actorContext(auth.RoleDeveloper), db, "fixture", trust, "", 1)
	if err != nil || len(page.Releases) != 1 {
		t.Fatal("published catalog discovery failed", err)
	}
	if bytes.Contains(first, []byte("workRunId")) || bytes.Contains(first, []byte("ownerUserId")) {
		t.Fatal("envelope exposed private source")
	}
	v, err := pl.VerifyPublishedRelease(first, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := v.Release()
	projection, _ := json.Marshal(r)
	for _, private := range []string{"workRunId", "ownerUserId", "credentialSecret", "receiptDigest"} {
		if bytes.Contains(projection, []byte(private)) {
			t.Fatal("public projection leaked", private)
		}
	}
	// Native receipt corruption cannot be legitimized by an existing signature.
	_, err = db.Exec(`UPDATE release_publication_intents SET receipt=jsonb_set(receipt,'{archiveDigest}',to_jsonb($2::text)) WHERE candidate_id=$1`, request.CandidateID, "sha256:"+strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seal(p.ledger); err == nil {
		t.Fatal("catalog seal ignored damaged native evidence")
	}
	// Portable signature remains historical, independent of private storage.
	if _, err := PublishedCatalog(actorContext(auth.RoleDeveloper), db, request.CandidateID, "fixture", trust); err != nil {
		t.Fatal("public read reached private authority", err)
	}
	_, err = db.Exec(`UPDATE release_catalog SET envelope=$2 WHERE candidate_id=$1`, request.CandidateID, []byte(`{"payloadType":"tampered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishedCatalog(actorContext(auth.RoleDeveloper), db, request.CandidateID, "fixture", trust); err == nil {
		t.Fatal("corrupted signed catalog accepted")
	}
}

func TestCatalogFileRequiresPublishedNativeRelease(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _ := candidateDraftWorkflowFixture(t, db)
	request := preparePublication(t, db, p, c)
	draftRequest := candidateDraftRequest{CandidateID: request.CandidateID, ApprovalID: request.ApprovalID, TargetID: request.TargetID}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	trust := map[string]ed25519.PublicKey{"fixture-key": pub}
	seal := func() ([]byte, error) {
		return p.ledger.sealCatalog(ownerCtx(), request.CandidateID, request.ApprovalID, "fixture", "fixture-key", key, trust)
	}
	if _, err := p.draft(ownerCtx(), draftRequest, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := seal(); err == nil {
		t.Fatal("draft upload became published catalog")
	}
	if _, err := p.draft(ownerCtx(), draftRequest, true); err != nil {
		t.Fatal(err)
	}
	body, err := seal()
	if err != nil {
		t.Fatal(err)
	}
	v, err := pl.VerifyPublishedRelease(body, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := v.Release()
	loc := r.Components[0].Artifacts[0].Locations[0]
	if loc.Kind != "github-release" || loc.ReleaseID != 73 || loc.AssetID <= 0 || loc.AssetName == "" || loc.Tag == "" {
		t.Fatal("file catalog lacks exact remote identity", loc)
	}
}

func TestCatalogSealOwnerGateBeforeStorageAndSecrets(t *testing.T) {
	i := walledIntegration(t)
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		if _, err := i.handleCatalogSeal(ctx, map[string]any{"candidateId": "anything", "approvalId": "approved"}, 0); err == nil {
			t.Fatal("non-owner sealed publication")
		}
	}
}
