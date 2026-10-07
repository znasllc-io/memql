package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

type candidateDraftFixture struct {
	asset                                                 *candidateAssetServer
	metadata                                              githubrelease.Metadata
	body                                                  string
	created, tagged, published                            bool
	creates, tags, promotes                               int
	loseCreate, losePromotion, hideCreated, hidePublished bool
}

func candidateDraftWorkflowFixture(t *testing.T, db *sql.DB) (*candidatePreparer, pl.ReleaseCandidate, *candidateDraftFixture) {
	t.Helper()
	p, c, library, asset := candidateFilePublicationFixture(t, db)
	f := &candidateDraftFixture{asset: asset, metadata: githubrelease.Metadata{Name: "Engine 0.25.0", Body: "Exact reviewed release notes."}}
	target := p.targetReader.files["registry"]
	target.ReleaseID = 0
	target.Draft = &f.metadata
	p.targetReader.files[target.ID] = target
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	p.targetReader.secret = func(ctx context.Context, name string) (string, error) {
		if name != target.CredentialSecret {
			return "", errors.New("wrong secret reference")
		}
		var state string
		err := db.QueryRowContext(ctx, `SELECT state FROM release_draft_intents WHERE resource_key=$1`, draftResourceKey(target)).Scan(&state)
		if err != nil || state == "" {
			return "", errors.New("credentials requested before durable draft intent")
		}
		return "fixture-asset-token", nil
	}
	asset.hook = func(w http.ResponseWriter, r *http.Request) bool {
		asset.mu.Lock()
		defer asset.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-asset-token" {
			t.Error("draft credential not scoped")
			w.WriteHeader(401)
			return true
		}
		base := "/repos/acme/engine"
		releasePath := base + "/releases/73"
		response := func() map[string]any {
			return map[string]any{"id": 73, "draft": !f.published, "tag_name": target.Tag, "name": f.metadata.Name, "body": f.body, "prerelease": f.metadata.Prerelease, "upload_url": target.UploadOrigin + releasePath + "/assets{?name,label}"}
		}
		lose := func() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
			} else {
				_ = conn.Close()
			}
		}
		switch {
		case r.Method == "GET" && r.URL.Path == base+"/git/ref/tags/"+target.Tag:
			if !f.tagged {
				w.WriteHeader(404)
				return true
			}
			return false
		case r.Method == "POST" && r.URL.Path == base+"/git/refs":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["ref"] != "refs/tags/"+target.Tag || body["sha"] != target.SourceCommit {
				t.Error("tag scope changed")
			}
			f.tags++
			f.tagged = true
			w.WriteHeader(201)
			return true
		case r.Method == "GET" && r.URL.Path == base+"/releases":
			out := []any{}
			if f.created && !f.hideCreated {
				out = append(out, response())
			}
			_ = json.NewEncoder(w).Encode(out)
			return true
		case r.Method == "POST" && r.URL.Path == base+"/releases":
			var state, marker string
			if err := db.QueryRow(`SELECT state,intent_id FROM release_draft_intents WHERE resource_key=$1`, draftResourceKey(target)).Scan(&state, &marker); err != nil || state != "creating" {
				t.Error("draft effect preceded its durable fence", state, err)
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["draft"] != true || body["tag_name"] != target.Tag || body["target_commitish"] != target.SourceCommit || body["name"] != f.metadata.Name || body["body"] != f.metadata.Body+"\n\n<!-- memql-release-intent:"+marker+" -->" {
				t.Error("draft scope changed")
			}
			f.body, _ = body["body"].(string)
			f.creates++
			f.created = true
			if f.loseCreate {
				lose()
			} else {
				w.WriteHeader(201)
			}
			return true
		case r.Method == "GET" && r.URL.Path == releasePath:
			if !f.created {
				w.WriteHeader(404)
			} else if f.published && f.hidePublished {
				w.WriteHeader(503)
			} else {
				_ = json.NewEncoder(w).Encode(response())
			}
			return true
		case r.Method == "PATCH" && r.URL.Path == releasePath:
			var state string
			if err := db.QueryRow(`SELECT state FROM release_draft_intents WHERE resource_key=$1`, draftResourceKey(target)).Scan(&state); err != nil || state != "promoting" {
				t.Error("promotion preceded durable fence", state, err)
			}
			f.promotes++
			f.published = true
			if f.losePromotion {
				lose()
			} else {
				_ = json.NewEncoder(w).Encode(response())
			}
			return true
		}
		return false
	}
	_ = library
	return p, c, f
}

func draftRequest(pub candidatePublishRequest) candidateDraftRequest {
	return candidateDraftRequest{CandidateID: pub.CandidateID, ApprovalID: pub.ApprovalID, TargetID: pub.TargetID}
}

func freshDraftHost(p *candidatePreparer, db *sql.DB) *candidatePreparer {
	other := *p
	other.ledger = &candidateLedger{db: func() *sql.DB { return db }}
	other.evidence = &candidateEvidenceFixture{modes: []string{"full"}}
	return &other
}

func TestCandidateDraftWorkflowRecoversAcrossHostsWithoutRepeatingEffects(t *testing.T) {
	db := candidateTestDB(t)
	p, c, f := candidateDraftWorkflowFixture(t, db)
	pub := preparePublication(t, db, p, c)
	request := draftRequest(pub)
	f.loseCreate, f.hideCreated = true, true
	if _, err := p.draft(ownerCtx(), request, false); !errors.Is(err, githubrelease.ErrUncertain) {
		t.Fatal("lost create did not remain uncertain", err)
	}
	history, err := p.ledger.drafts(ownerCtx(), pub.CandidateID)
	if err != nil || len(history) != 1 || history[0].State != "creating" || history[0].ReleaseID != 0 {
		t.Fatal(history, err)
	}
	other := freshDraftHost(p, db)
	if _, err := other.draft(ownerCtx(), request, false); !errors.Is(err, githubrelease.ErrUncertain) {
		t.Fatal("absent recovery retried creation", err)
	}
	if f.creates != 1 || f.tags != 1 {
		t.Fatal("effect repeated", f.creates, f.tags)
	}
	if _, err := other.retire(ownerCtx(), pub.CandidateID); err == nil {
		t.Fatal("uncertain draft candidate retired")
	}
	f.asset.mu.Lock()
	f.hideCreated = false
	f.asset.mu.Unlock()
	binding, err := other.draft(ownerCtx(), request, false)
	if err != nil || binding.State != "ready" || binding.ReleaseID != 73 {
		t.Fatal(binding, err)
	}
	if _, err := other.draft(ownerCtx(), request, true); err == nil || f.promotes != 0 {
		t.Fatal("unpublished assets promoted", err)
	}
	if _, err := other.publish(ownerCtx(), pub); err != nil {
		t.Fatal(err)
	}
	f.asset.mu.Lock()
	f.losePromotion, f.hidePublished = true, true
	f.asset.mu.Unlock()
	if _, err := p.draft(ownerCtx(), request, true); !errors.Is(err, githubrelease.ErrUncertain) {
		t.Fatal("lost promotion did not remain uncertain", err)
	}
	history, err = p.ledger.drafts(ownerCtx(), pub.CandidateID)
	if err != nil || len(history) != 1 || history[0].State != "promoting" {
		t.Fatal(history, err)
	}
	if _, err := freshDraftHost(p, db).draft(ownerCtx(), request, true); err == nil {
		t.Fatal("unverifiable promotion reported success")
	}
	if f.promotes != 1 {
		t.Fatal("promotion PATCH repeated")
	}
	f.asset.mu.Lock()
	f.hidePublished = false
	f.asset.mu.Unlock()
	published, err := freshDraftHost(p, db).draft(ownerCtx(), request, true)
	if err != nil || published.State != "published" || published.Receipt == nil || published.Receipt.Draft || len(published.Receipt.Assets) != 1 {
		t.Fatal(published, err)
	}
	if f.creates != 1 || f.promotes != 1 || f.asset.uploads != 1 {
		t.Fatal("effects repeated", f.creates, f.promotes, f.asset.uploads)
	}
	if _, err := freshDraftHost(p, db).draft(ownerCtx(), request, true); err != nil {
		t.Fatal("completed release could not reconcile", err)
	}
	if f.promotes != 1 {
		t.Fatal("completed promotion repeated")
	}
}

func TestCandidateDraftClaimIsUniqueAndBoundToApproval(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _ := candidateDraftWorkflowFixture(t, db)
	pub := preparePublication(t, db, p, c)
	rec, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.targetReader.draftPlan(ownerCtx(), rec, pub.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, "other-approval", plan); err == nil {
		t.Fatal("wrong approval admitted")
	}
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, pub.ApprovalID, plan); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan bool, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := freshDraftHost(p, db).ledger.claimDraftEffect(ownerCtx(), rec.ID, pub.ApprovalID, plan, false)
			claims <- claimed
			failures <- err
		}()
	}
	wg.Wait()
	close(claims)
	close(failures)
	count := 0
	for claimed := range claims {
		if claimed {
			count++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatal("draft was claimed more than once", count)
	}
	changed := plan
	changed.Target.Tag = "different"
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, pub.ApprovalID, changed); err == nil {
		t.Fatal("plan changed under same resource identity")
	}
}

func TestCandidatePromotionFencesNativeUploads(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _, _ := candidateFilePublicationFixture(t, db)
	pub := preparePublication(t, db, p, c)
	rec, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.targetReader.draftPlan(ownerCtx(), rec, pub.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, pub.ApprovalID, plan); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ledger.claimDraftEffect(ownerCtx(), rec.ID, pub.ApprovalID, plan, true); err == nil {
		t.Fatal("missing publication admitted")
	}
	if _, err := p.publish(ownerCtx(), pub); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	written := make(chan error, 1)
	claimed := make(chan error, 1)
	go func() {
		written <- p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(id int64) error {
			close(entered)
			<-release
			if id != 73 {
				return errors.New("binding changed")
			}
			return nil
		})
	}()
	<-entered
	go func() {
		_, ok, err := freshDraftHost(p, db).ledger.claimDraftEffect(ownerCtx(), rec.ID, pub.ApprovalID, plan, true)
		if err == nil && !ok {
			err = errors.New("promotion not claimed")
		}
		claimed <- err
	}()
	select {
	case err := <-claimed:
		t.Fatal("promotion escaped active upload fence", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-claimed; err != nil {
		t.Fatal(err)
	}
	if err := p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(int64) error { t.Fatal("upload entered promoting draft"); return nil }); err == nil {
		t.Fatal("promotion fence missing")
	}
}

func TestCandidateDraftRejectsChangedMetadataAndUnscopedPorts(t *testing.T) {
	db := candidateTestDB(t)
	p, c, f := candidateDraftWorkflowFixture(t, db)
	pub := preparePublication(t, db, p, c)
	request := draftRequest(pub)
	target := p.targetReader.files[pub.TargetID]
	metadata := *target.Draft
	metadata.Body += " changed"
	target.Draft = &metadata
	p.targetReader.files[pub.TargetID] = target
	if _, err := p.draft(ownerCtx(), request, false); err == nil {
		t.Fatal("changed metadata used old candidate")
	}
	if f.creates != 0 || f.tags != 0 {
		t.Fatal("changed target mutated provider")
	}
	rec, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	s := &candidateDraftScope{p: p, record: rec, request: request}
	for _, name := range []string{"releaseDraftBegin", "releaseDraftEnsureTag", "releaseDraftFind", "releaseDraftClaimCreate", "releaseDraftCreate", "releaseDraftRecord", "releasePromotionClaim", "releasePromotionWrite", "releasePromotionRecord"} {
		if _, err := s.operations()[name](ownerCtx(), nil); err == nil {
			t.Fatalf("%s admitted missing native scope", name)
		}
	}
	if _, err := p.ledger.drafts(context.Background(), strings.Repeat("a", 64)); err == nil {
		t.Fatal("unowned history admitted")
	}
}

func TestCandidateDraftRollbackPreservesStartedEffect(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _ := candidateDraftWorkflowFixture(t, db)
	pub := preparePublication(t, db, p, c)
	rec, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.targetReader.draftPlan(ownerCtx(), rec, pub.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, pub.ApprovalID, plan); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := p.ledger.claimDraftEffect(ownerCtx(), rec.ID, pub.ApprovalID, plan, false); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	body, err := os.ReadFile("../../component/database/memory-nodes/migrations/20261007050000_release_draft_intents.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(t.Context(), string(body))
	_ = tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "release draft journal is not empty") {
		t.Fatal("rollback discarded effect history", err)
	}
	history, err := p.ledger.drafts(ownerCtx(), rec.ID)
	if err != nil || len(history) != 1 || history[0].State != "creating" {
		t.Fatal(history, err)
	}
}
