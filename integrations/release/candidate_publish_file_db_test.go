package release

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

type candidateAssetServer struct {
	mu                sync.Mutex
	requests, uploads int
	body              []byte
	lost, corrupt     bool
}

func candidateFilePublicationFixture(t *testing.T, db *sql.DB) (*candidatePreparer, pl.ReleaseCandidate, *candidateLibraryFixture, *candidateAssetServer) {
	t.Helper()
	p, c, library, _ := candidatePreparationFixture(t, db)
	c.Components[0].Artifacts[0].Kind = "file"
	c.Components[0].Artifacts[0].ImageDigest = ""
	var origin string
	f := &candidateAssetServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		if r.Header.Get("Authorization") != "Bearer fixture-asset-token" {
			t.Error("missing scoped asset credential")
			w.WriteHeader(401)
			return
		}
		const releasePath = "/repos/acme/engine/releases/73"
		switch {
		case r.Method == "GET" && r.URL.Path == releasePath:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 73, "draft": true, "tag_name": "v0.25.0", "upload_url": origin + releasePath + "/assets{?name,label}"})
		case r.Method == "GET" && r.URL.Path == "/repos/acme/engine/git/ref/tags/v0.25.0":
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/v0.25.0", "object": map[string]any{"type": "commit", "sha": c.Components[0].Commit}})
		case r.Method == "GET" && r.URL.Path == releasePath+"/assets":
			rows := []map[string]any{}
			if f.body != nil {
				rows = append(rows, map[string]any{"id": 91, "name": "engine.tar", "state": "uploaded", "size": len(f.body), "digest": c.Components[0].Artifacts[0].Digest})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == "POST" && r.URL.Path == releasePath+"/assets":
			f.uploads++
			if f.body != nil {
				w.WriteHeader(422)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || r.URL.Query().Get("name") != "engine.tar" || !bytes.Equal(body, library.body) {
				t.Error("publication did not use exact verified file", err)
				w.WriteHeader(400)
				return
			}
			f.body = body
			if f.lost {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(201)
		case r.Method == "GET" && r.URL.Path == "/repos/acme/engine/releases/assets/91":
			if f.corrupt {
				_, _ = w.Write([]byte("corrupt"))
				return
			}
			_, _ = w.Write(f.body)
		default:
			t.Error("unexpected asset effect", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	origin = server.URL
	target := candidateFileTarget{ID: "registry", Component: "engine", Artifact: "image", APIOrigin: origin, UploadOrigin: origin,
		Repository: "acme/engine", SourceCommit: c.Components[0].Commit, Tag: "v0.25.0", ReleaseID: 73, AssetName: "engine.tar", CredentialSecret: "RELEASE_ASSET_TOKEN", AllowLoopbackHTTP: true}
	p.targetReader.targets = nil
	p.targetReader.files = map[string]candidateFileTarget{target.ID: target}
	p.targetReader.secret = func(ctx context.Context, name string) (string, error) {
		if name != target.CredentialSecret {
			t.Fatal("unapproved credential reference")
		}
		var effects int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM release_publication_intents i JOIN release_candidates c USING(candidate_id) WHERE c.manifest->'components'->0->'artifacts'->0->'receipt'->>'workRunId'=$1`, library.receipt.WorkRunID).Scan(&effects); err != nil || effects != 1 {
			t.Fatal("credential fetched before durable publication authority", err, effects)
		}
		return "fixture-asset-token", nil
	}
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	return p, c, library, f
}

func assertNoCandidateFileScratch(t *testing.T, scratch string) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(scratch, "memql-verified-release-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("verified file scratch leaked", entries, err)
	}
}

func TestCandidateFilePublicationLostReplyAndUncertainRecovery(t *testing.T) {
	db := candidateTestDB(t)
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost reply", true: "uncertain readback"}[corrupt], func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			p, c, library, server := candidateFilePublicationFixture(t, db)
			server.lost, server.corrupt = true, corrupt
			request := preparePublication(t, db, p, c)
			if server.requests != 0 {
				t.Fatal("prepare/approve contacted publication service")
			}
			first, err := p.publish(ownerCtx(), request)
			if corrupt {
				if !errors.Is(err, githubrelease.ErrUncertain) {
					t.Fatal("uncertain file reported a known outcome", err)
				}
			} else if err != nil || first.State != "complete" {
				t.Fatal("lost response was not reconciled", first, err)
			}
			assertNoCandidateFileScratch(t, scratch)
			var effect, state string
			if err := db.QueryRow(`SELECT effect_id,state FROM release_publication_intents WHERE candidate_id=$1`, request.CandidateID).Scan(&effect, &state); err != nil {
				t.Fatal(err)
			}
			if corrupt && state != "pending" {
				t.Fatal("uncertain publication completed", state)
			}
			if _, err := p.retire(ownerCtx(), request.CandidateID); err == nil || len(library.pins) == 0 {
				t.Fatal("publication lost its retained evidence")
			}
			server.mu.Lock()
			server.corrupt = false
			server.mu.Unlock()
			other := *p
			other.ledger = &candidateLedger{db: func() *sql.DB { return db }}
			other.evidence = &candidateEvidenceFixture{modes: []string{"full"}}
			completed, err := other.publish(ownerCtx(), request)
			if err != nil || completed.State != "complete" || completed.EffectID != effect || server.uploads != 1 {
				t.Fatal("second host did not recover same file effect", completed, server.uploads, err)
			}
			if library.opens != library.closes {
				t.Fatal("file stream leaked")
			}
			assertNoCandidateFileScratch(t, scratch)
		})
	}
}

func TestCandidateFilePublicationRefusesChangedAuthorityAndMissingVerification(t *testing.T) {
	db := candidateTestDB(t)
	for _, failure := range []string{"approval", "target", "corrupt bytes", "omitted verification", "omitted write"} {
		t.Run(failure, func(t *testing.T) {
			p, c, library, server := candidateFilePublicationFixture(t, db)
			definition, err := workflowhost.Load(candidatePublishWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			definition, err = automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "omitted verification" || failure == "omitted write" {
				// Bind the alternative installed recipe BEFORE approval. Its policy
				// hash is valid, but it cannot remove mandatory native integrity.
				body := `builtin releasePublicationCheckCandidate()
effect := builtin releasePublicationBegin()
if effect.complete { return }
builtin releasePublicationVerifyFile()
builtin releasePublicationWriteFile()
builtin releasePublicationRecordReceipt()`
				if failure == "omitted verification" {
					body = strings.Replace(body, "builtin releasePublicationVerifyFile()", "", 1)
				} else {
					body = strings.Replace(body, "builtin releasePublicationWriteFile()", "", 1)
				}
				definition, err = automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation incompleteFilePublication {\n"+body+"\n}", "file-publication-test.memql")
				if err != nil {
					t.Fatal(err)
				}
				definition.Trusted = true
			}
			prep, err := workflowhost.Load(candidatePrepareWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			record, err := p.prepareWithExpectedIdentity(ownerCtx(), c, prep, "", definition)
			if err != nil {
				t.Fatal(err)
			}
			cleanupCandidate(t, db, record.ID)
			approved, err := p.ledger.approve(ownerCtx(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := candidatePublishRequest{CandidateID: record.ID, ApprovalID: approved.ApprovalID, TargetID: "registry", Component: "engine", Artifact: "image"}
			switch failure {
			case "approval":
				request.ApprovalID = "wrong"
			case "target":
				target := p.targetReader.files["registry"]
				target.ReleaseID++
				p.targetReader.files["registry"] = target
			case "corrupt bytes":
				library.body[0] ^= 1
			}
			if _, err := p.publishWithWorkflow(ownerCtx(), request, definition); err == nil {
				t.Fatal("unsafe file publication accepted")
			}
			if server.requests != 0 {
				t.Fatal("invalid file authority contacted remote publication", server.requests)
			}
			if library.opens != library.closes {
				t.Fatal("rejected file stream leaked")
			}
		})
	}
}
