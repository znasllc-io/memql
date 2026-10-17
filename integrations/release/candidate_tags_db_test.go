package release

import (
	"database/sql"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCandidateVersionTagPublicationAndSecondHost(t *testing.T) {
	db := candidateTestDB(t)
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	p, c, _, _ := candidatePublicationFixture(t, db, server.URL)
	target := p.targetReader.targets["registry"]
	target.Tag = "v" + c.Components[0].Version
	p.targetReader.targets[target.ID] = target
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	request := preparePublication(t, db, p, c)
	first, err := p.publish(ownerCtx(), request)
	if err != nil || first.State != "complete" {
		t.Fatal("native tag publication failed", err)
	}
	var storedKey string
	if err := db.QueryRow(`SELECT registry_tag_key FROM release_publication_intents WHERE effect_id=$1`, first.EffectID).Scan(&storedKey); err != nil || storedKey != registryTagKey(target) {
		t.Fatal("tag was not durably reserved", err)
	}
	before := writes.Load()
	second := *p
	second.ledger = &candidateLedger{db: func() *sql.DB { return db }}
	recovered, err := second.publish(ownerCtx(), request)
	if err != nil || recovered.State != "complete" || recovered.EffectID != first.EffectID || writes.Load() != before {
		t.Fatal("second host did not recover the same complete effect", err)
	}
}

func TestRegistryTagReservationRejectsCompetingCandidatesAcrossHosts(t *testing.T) {
	db := candidateTestDB(t)
	first := &candidateLedger{db: func() *sql.DB { return db }}
	second := &candidateLedger{db: func() *sql.DB { return db }}
	target := registryTargetFixture()
	target.Tag = "v0.25.0"
	// Each test owns its repository so other test processes cannot compete.
	seed := candidateTestManifest()
	target.Repository = "claim/" + seed.Components[0].Artifacts[0].Receipt.WorkRunID
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(l *candidateLedger, c pl.ReleaseCandidate) candidatePublication {
		c.Destinations[0].TargetDigest = digest
		r, err := l.prepare(ownerCtx(), c)
		if err != nil {
			t.Fatal(err)
		}
		cleanupCandidate(t, db, r.ID)
		if _, err := l.ready(ownerCtx(), r.ID); err != nil {
			t.Fatal(err)
		}
		a, err := l.approve(ownerCtx(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := l.beginPublication(ownerCtx(), r.ID, a.ApprovalID, "registry", "engine", "image")
		if err != nil {
			t.Fatal(err)
		}
		return intent
	}
	intents := []candidatePublication{prepare(first, seed), prepare(second, candidateTestManifest())}
	hosts := []*candidateLedger{first, second}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := range hosts {
		wg.Go(func() {
			<-start
			errs[i] = hosts[i].freezePublicationTarget(ownerCtx(), intents[i], candidatePublicationTarget{Registry: &target})
		})
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatal("two candidates reserved one tag")
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatal("neither host reserved the tag", errs)
	}
	// Reconstruct the winning operation on the other host, without local state.
	if err := hosts[1-winner].freezePublicationTarget(ownerCtx(), intents[winner], candidatePublicationTarget{Registry: &target}); err != nil {
		t.Fatal("reservation cannot resume across hosts", err)
	}
}
