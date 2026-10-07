package release

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

func candidatePublicationFixture(t *testing.T, db *sql.DB, origin string) (*candidatePreparer, pl.ReleaseCandidate, *candidateLibraryFixture, *candidateEvidenceFixture) {
	t.Helper()
	p, c, library, evidence := candidatePreparationFixture(t, db)
	target := p.targetReader.targets["registry"]
	target.Origin, target.AllowLoopbackHTTP = origin, true
	// Every test has a separate repository even on the disposable live registry.
	target.Repository = "candidate/" + c.Components[0].Artifacts[0].Receipt.WorkRunID
	p.targetReader.targets[target.ID] = target
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	return p, c, library, evidence
}

func preparePublication(t *testing.T, db *sql.DB, p *candidatePreparer, c pl.ReleaseCandidate) candidatePublishRequest {
	t.Helper()
	r, err := p.prepare(ownerCtx(), c)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, r.ID)
	a, err := p.approve(ownerCtx(), r.ID)
	if err != nil || a.State != "approved" || a.ApprovalID == "" {
		t.Fatal("separate approval failed", err)
	}
	return candidatePublishRequest{CandidateID: r.ID, ApprovalID: a.ApprovalID, TargetID: "registry", Component: "engine", Artifact: "image"}
}

func assertNoPublicationScratch(t *testing.T, scratch string) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(scratch, "memql-verified-oci-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("publication leaked verified image scratch", entries, err)
	}
}

func TestCandidatePublicationRequiresOwnerBeforePorts(t *testing.T) {
	p := &candidatePreparer{ledger: &candidateLedger{db: func() *sql.DB { t.Fatal("non-owner reached journal"); return nil }}}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		if _, err := p.approve(ctx, "candidate"); err == nil {
			t.Fatal("non-owner approved candidate")
		}
		if _, err := p.publish(ctx, candidatePublishRequest{}); err == nil {
			t.Fatal("non-owner published candidate")
		}
	}
	for _, capability := range workflowhost.ScopedCapabilities((&candidatePublishScope{}).operations()) {
		if _, err := capability.Handler(ownerCtx(), map[string]any{"approved": true, "verified": true}, 0); err == nil {
			t.Fatal("serialized authority reached publication operation")
		}
	}
}

func TestCandidatePublicationLostReplyAndSecondHost(t *testing.T) {
	db := candidateTestDB(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var requests, writes, lost atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
			writes.Add(1)
		}
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/") {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != http.StatusCreated {
				t.Errorf("registry refused manifest: %d", response.Code)
			}
			lost.Add(1)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	p, c, library, _ := candidatePublicationFixture(t, db, server.URL)
	request := preparePublication(t, db, p, c)
	if requests.Load() != 0 {
		t.Fatal("preparation or approval wrote to registry")
	}
	first, err := p.publish(ownerCtx(), request)
	if err != nil || first.State != "complete" || lost.Load() == 0 || writes.Load() == 0 {
		t.Fatal("lost manifest response did not reconcile", first, err)
	}
	assertNoPublicationScratch(t, scratch)
	before := requests.Load()
	// A new host holds no publisher handle, proof or scope flags from the first.
	other := *p
	other.ledger = &candidateLedger{db: func() *sql.DB { return db }}
	other.evidence = &candidateEvidenceFixture{modes: []string{"full"}}
	repeated, err := other.publish(ownerCtx(), request)
	if err != nil || repeated.EffectID != first.EffectID || repeated.State != "complete" || requests.Load() != before {
		t.Fatal("completed intent was republished", repeated, err)
	}
	if library.opens != library.closes {
		t.Fatal("artifact stream leaked")
	}
	assertNoPublicationScratch(t, scratch)
}

func TestCandidatePublicationUncertainReadbackRetainsIntentAndPins(t *testing.T) {
	db := candidateTestDB(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var corrupt atomic.Bool
	corrupt.Store(true)
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
			writes.Add(1)
		}
		if corrupt.Load() && r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			_, _ = io.WriteString(w, "corrupted registry bytes")
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	p, c, library, _ := candidatePublicationFixture(t, db, server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); !errors.Is(err, ociregistry.ErrUncertain) {
		t.Fatal("corrupt readback was not uncertain", err)
	}
	var effect, state string
	if err := db.QueryRow(`SELECT effect_id,state FROM release_publication_intents WHERE candidate_id=$1`, request.CandidateID).Scan(&effect, &state); err != nil || state != "pending" {
		t.Fatal("uncertain effect did not remain pending", state, err)
	}
	if _, err := p.retire(ownerCtx(), request.CandidateID); err == nil || !library.pins[request.CandidateID] || len(library.released) != 0 {
		t.Fatal("uncertain publication lost its recovery evidence", err)
	}
	assertNoPublicationScratch(t, scratch)
	before := writes.Load()
	corrupt.Store(false)
	other := *p
	other.ledger = &candidateLedger{db: func() *sql.DB { return db }}
	other.evidence = &candidateEvidenceFixture{modes: []string{"full"}}
	recovered, err := other.publish(ownerCtx(), request)
	if err != nil || recovered.EffectID != effect || recovered.State != "complete" || writes.Load() != before {
		t.Fatal("new host failed to reconcile existing bytes", recovered, err)
	}
	if library.opens != library.closes {
		t.Fatal("artifact stream leaked")
	}
	assertNoPublicationScratch(t, scratch)
}

func TestCandidatePublicationRefusesChangedOrUnapprovedInputsBeforeRegistry(t *testing.T) {
	db := candidateTestDB(t)
	for _, failure := range []string{"unapproved", "wrong approval", "target", "evidence", "source rules", "engine", "bytes", "publication workflow"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			p, c, library, evidence := candidatePublicationFixture(t, db, server.URL)
			record, err := p.prepare(ownerCtx(), c)
			if err != nil {
				t.Fatal(err)
			}
			cleanupCandidate(t, db, record.ID)
			request := candidatePublishRequest{CandidateID: record.ID, TargetID: "registry", Component: "engine", Artifact: "image"}
			if failure != "unapproved" {
				approved, err := p.approve(ownerCtx(), record.ID)
				if err != nil {
					t.Fatal(err)
				}
				request.ApprovalID = approved.ApprovalID
			}
			definition, err := workflowhost.Load(candidatePublishWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "wrong approval":
				request.ApprovalID = "not-the-approved-identity"
			case "target":
				target := p.targetReader.targets["registry"]
				target.Repository = "different/image"
				p.targetReader.targets["registry"] = target
			case "evidence":
				evidence.failAfter = evidence.calls + 1
			case "source rules":
				source := p.versionReader.sources["engine"]
				source.Path = "OTHER_VERSION"
				p.versionReader.sources["engine"] = source
			case "engine":
				p.engineRevision = func() string { return strings.Repeat("e", 40) }
			case "bytes":
				library.body = []byte("corrupt archive")
			case "publication workflow":
				definition, err = automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
				if err != nil {
					t.Fatal(err)
				}
				definition.Name = "changedPublicationWorkflow"
			}
			if _, err := p.publishWithWorkflow(ownerCtx(), request, definition); err == nil {
				t.Fatal("changed input published")
			}
			if requests.Load() != 0 {
				t.Fatal("rejected input reached registry")
			}
			var intents, candidates int
			if err := db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id=$1`, record.ID).Scan(&intents); err != nil {
				t.Fatal(err)
			}
			if intents != 0 {
				t.Fatal("rejected input created publication intent")
			}
			if err := db.QueryRow(`SELECT count(*) FROM release_candidates WHERE manifest->'components'->0->'artifacts'->0->'receipt'->>'workRunId'=$1`, c.Components[0].Artifacts[0].Receipt.WorkRunID).Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if candidates != 1 || len(library.pins) != 1 || library.opens != library.closes {
				t.Fatal("reverification created new candidate/pin or leaked stream")
			}
		})
	}
}

func TestCandidateApprovalReverifiesWithoutCreatingReplacementCandidate(t *testing.T) {
	db := candidateTestDB(t)
	for _, failure := range []string{"engine", "evidence", "artifact"} {
		t.Run(failure, func(t *testing.T) {
			p, c, library, evidence := candidatePreparationFixture(t, db)
			record, err := p.prepare(ownerCtx(), c)
			if err != nil {
				t.Fatal(err)
			}
			cleanupCandidate(t, db, record.ID)
			switch failure {
			case "engine":
				p.engineRevision = func() string { return strings.Repeat("e", 40) }
			case "evidence":
				evidence.failAfter = evidence.calls + 1
			case "artifact":
				library.body = []byte("changed bytes")
			}
			if _, err := p.approve(ownerCtx(), record.ID); err == nil {
				t.Fatal("changed proof was approved")
			}
			var approvals int
			if err := db.QueryRow(`SELECT count(*) FROM release_candidate_approvals WHERE candidate_id=$1`, record.ID).Scan(&approvals); err != nil {
				t.Fatal(err)
			}
			if approvals != 0 || len(library.pins) != 1 || library.opens != library.closes {
				t.Fatal("failed approval changed authority or leaked stream")
			}
		})
	}
}

func TestCandidatePublicationWorkflowCannotOmitMandatoryEffects(t *testing.T) {
	db := candidateTestDB(t)
	prepare, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"CheckCandidate", "Begin", "VerifyImage", "WriteImage", "RecordReceipt"} {
		t.Run(omitted, func(t *testing.T) {
			var lines []string
			for _, operation := range []string{"CheckCandidate", "Begin", "VerifyImage", "WriteImage", "RecordReceipt"} {
				if operation != omitted {
					lines = append(lines, "builtin releasePublication"+operation+"()")
				}
			}
			definition, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation incompletePublication {\n"+strings.Join(lines, "\n")+"\n}", "publication-test.memql")
			if err != nil {
				t.Fatal(err)
			}
			definition.Trusted = true // An alternate installed recipe cannot omit native invariants.
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
			var writes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
					writes.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			p, c, library, _ := candidatePublicationFixture(t, db, server.URL)
			record, err := p.prepareWithExpectedIdentity(ownerCtx(), c, prepare, "", definition)
			if err != nil {
				t.Fatal(err)
			}
			cleanupCandidate(t, db, record.ID)
			approved, err := p.ledger.approve(ownerCtx(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := candidatePublishRequest{CandidateID: record.ID, ApprovalID: approved.ApprovalID, TargetID: "registry", Component: "engine", Artifact: "image"}
			if _, err := p.publishWithWorkflow(ownerCtx(), request, definition); err == nil {
				t.Fatal("incomplete workflow reported success")
			}
			var completed int
			if err := db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id=$1 AND state='complete'`, record.ID).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if completed != 0 || library.opens != library.closes {
				t.Fatal("incomplete workflow completed or leaked stream")
			}
			if omitted != "RecordReceipt" && writes.Load() != 0 {
				t.Fatal("workflow bypassed mandatory write prerequisites")
			}
			assertNoPublicationScratch(t, scratch)
		})
	}
}

func TestCandidatePublicationWithDisposableDistribution(t *testing.T) {
	origin := os.Getenv("MEMQL_OCI_TEST_REGISTRY")
	if origin == "" {
		t.Skip("set MEMQL_OCI_TEST_REGISTRY to a disposable loopback registry")
	}
	db := candidateTestDB(t)
	p, c, library, _ := candidatePublicationFixture(t, db, origin)
	if archive := os.Getenv("MEMQL_CANDIDATE_TEST_OCI_ARCHIVE"); archive != "" {
		info, err := os.Stat(archive)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<30 {
			t.Fatal("live OCI proof requires a bounded regular archive", err)
		}
		body, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		a := &c.Components[0].Artifacts[0]
		a.Size, a.Digest, a.ImageDigest = int64(len(body)), os.Getenv("MEMQL_CANDIDATE_TEST_ARCHIVE_DIGEST"), os.Getenv("MEMQL_CANDIDATE_TEST_IMAGE_DIGEST")
		library.body, library.receipt.Size, library.receipt.SHA256 = body, a.Size, strings.TrimPrefix(a.Digest, "sha256:")
	}
	request := preparePublication(t, db, p, c)
	result, err := p.publish(ownerCtx(), request)
	if err != nil || result.State != "complete" {
		t.Fatal(result, err)
	}
	if library.opens != library.closes {
		t.Fatal("artifact stream leaked")
	}
	t.Logf("verified candidate %s, publication %s, image %s", result.CandidateID, result.EffectID, result.Artifact.ImageDigest)
}
