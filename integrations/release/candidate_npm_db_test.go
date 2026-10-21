package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/npmregistry"
)

func npmCandidateFixture(t *testing.T, db *sql.DB) (*candidatePreparer, pl.ReleaseCandidate, *candidateLibraryFixture, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	p, c, library, _ := candidatePreparationFixture(t, db)
	var archive bytes.Buffer
	z := gzip.NewWriter(&archive)
	tarball := tar.NewWriter(z)
	manifest := `{"name":"@acme/sdk","version":"0.25.0"}`
	if err := tarball.WriteHeader(&tar.Header{Name: "package/package.json", Size: int64(len(manifest)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	library.body = archive.Bytes()
	h := sha256.Sum256(library.body)
	library.receipt.SHA256, library.receipt.Size = hex.EncodeToString(h[:]), int64(len(library.body))
	a := &c.Components[0].Artifacts[0]
	a.Kind, a.Platform, a.ImageDigest, a.Digest, a.Size = "file", "npm", "", "sha256:"+library.receipt.SHA256, library.receipt.Size
	var writes atomic.Int64
	var corrupt atomic.Bool
	var stored []byte
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer approved-npm-token" {
			t.Error("wrong credential")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/archive" {
			if corrupt.Load() {
				w.Write([]byte("wrong"))
			} else {
				w.Write(stored)
			}
			return
		}
		if r.URL.Path != "/@acme/sdk" {
			t.Error("wrong package target")
			w.WriteHeader(400)
			return
		}
		if r.Method == "GET" {
			versions := map[string]any{}
			if stored != nil {
				versions["0.25.0"] = map[string]any{"name": "@acme/sdk", "version": "0.25.0", "dist": map[string]string{"tarball": origin + "/archive"}}
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "@acme/sdk", "versions": versions})
			return
		}
		if r.Method != "PUT" {
			t.Error("unexpected method")
			w.WriteHeader(405)
			return
		}
		writes.Add(1)
		var body struct {
			Attachments map[string]struct{ Data string } `json:"_attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for _, a := range body.Attachments {
			var err error
			stored, err = base64.StdEncoding.DecodeString(a.Data)
			if err != nil {
				t.Error(err)
			}
		}
		if !bytes.Equal(stored, library.body) {
			t.Error("published another artifact")
		}
		w.WriteHeader(201)
	}))
	t.Cleanup(server.Close)
	origin = server.URL
	target := candidateNPMTarget{ID: "registry", Component: "engine", Artifact: "image", Origin: origin, PackageName: "@acme/sdk", Version: "0.25.0", DistTag: "latest", CredentialSecret: "NPM_PUBLISH_TOKEN", AllowLoopbackHTTP: true}
	p.targetReader = &candidateTargetReader{npm: map[string]candidateNPMTarget{target.ID: target}, secret: func(ctx context.Context, name string) (string, error) {
		if name != target.CredentialSecret {
			t.Fatal("credential reference broadened")
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM release_publication_intents WHERE target_snapshot->'npm'->>'origin'=$1 AND registry_tag_key=$2`, origin, npmVersionKey(target)).Scan(&count); err != nil || count != 1 {
			t.Fatal("credential resolved before durable exact version reservation", count, err)
		}
		return "approved-npm-token", nil
	}}
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	return p, c, library, &writes, &corrupt
}

func TestCandidateNPMPublicationRecoveryOnAnotherHostAndCatalog(t *testing.T) {
	db := candidateTestDB(t)
	p, c, library, writes, corrupt := npmCandidateFixture(t, db)
	request := preparePublication(t, db, p, c)
	corrupt.Store(true)
	if _, err := p.publish(ownerCtx(), request); !errors.Is(err, npmregistry.ErrUncertain) {
		t.Fatal("corrupt readback reported completion", err)
	}
	var state, effect string
	if err := db.QueryRow(`SELECT state,effect_id FROM release_publication_intents WHERE candidate_id=$1`, request.CandidateID).Scan(&state, &effect); err != nil || state == "complete" || effect == "" {
		t.Fatal("uncertain publication lost its durable intent", state, effect, err)
	}
	if len(library.pins) == 0 {
		t.Fatal("uncertain artifact was unpinned")
	}
	corrupt.Store(false)
	// A separate host sees only the same database, source and retained receipts.
	otherDB := candidateTestDB(t)
	other := *p
	other.ledger = &candidateLedger{db: func() *sql.DB { return otherDB }}
	result, err := other.publish(ownerCtx(), request)
	if err != nil || result.State != "complete" || result.EffectID != effect || writes.Load() != 1 {
		t.Fatal("recovery changed identity, bytes or write count", result, err, writes.Load())
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	trust := map[string]ed25519.PublicKey{"fixture": pub}
	envelope, err := other.ledger.sealCatalog(ownerCtx(), request.CandidateID, request.ApprovalID, "fixture", "fixture", key, trust)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := pl.VerifyPublishedRelease(envelope, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	release, err := verified.Release()
	if err != nil {
		t.Fatal(err)
	}
	loc := release.Components[0].Artifacts[0].Locations[0]
	if loc.Kind != "npm" || loc.Repository != "@acme/sdk" || loc.Tag != "0.25.0" {
		t.Fatal("catalog lost exact registry identity", loc)
	}
	if library.opens != library.closes {
		t.Fatal("artifact stream leaked")
	}
}

func TestCandidateNPMRefusesUnapprovedVersionAndTargetDrift(t *testing.T) {
	db := candidateTestDB(t)
	for _, name := range []string{"version", "drift", "receipt", "missing verification"} {
		t.Run(name, func(t *testing.T) {
			p, c, library, writes, _ := npmCandidateFixture(t, db)
			if name == "version" {
				target := p.targetReader.npm["registry"]
				target.Version = "9.9.9"
				p.targetReader.npm["registry"] = target
				_, _, digest, _ := target.snapshot()
				c.Destinations[0].TargetDigest = digest
				if _, err := p.prepare(ownerCtx(), c); err == nil {
					t.Fatal("unbound package version accepted")
				}
				return
			}
			request := preparePublication(t, db, p, c)
			switch name {
			case "drift":
				target := p.targetReader.npm["registry"]
				target.DistTag = "preview"
				p.targetReader.npm["registry"] = target
			case "receipt":
				library.receipt.SHA256 = strings.Repeat("0", 64)
			case "missing verification":
				scope := &candidatePublishScope{p: p, request: request, checked: true, intent: candidatePublication{EffectID: "intent", ApprovalID: request.ApprovalID, Target: c.Destinations[0], Artifact: c.Components[0].Artifacts[0]}}
				if _, err := scope.writeFile(ownerCtx(), nil); err == nil {
					t.Fatal("unverified package published")
				}
				return
			}
			if _, err := p.publish(ownerCtx(), request); err == nil {
				t.Fatal("changed authority reached publication")
			}
			if writes.Load() != 0 {
				t.Fatal("refusal wrote registry")
			}
		})
	}
}

func TestCandidateNPMVersionIsReservedAcrossCandidates(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _, writes, _ := npmCandidateFixture(t, db)
	first := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), first); err != nil {
		t.Fatal(err)
	}
	c.Evidence[0].Name = "another-reviewed-assembly"
	second := preparePublication(t, db, p, c)
	if first.CandidateID == second.CandidateID {
		t.Fatal("fixture did not create distinct approvals")
	}
	if _, err := p.publish(ownerCtx(), second); err == nil {
		t.Fatal("another candidate claimed an already-reserved package version")
	}
	if writes.Load() != 1 {
		t.Fatal("version reservation allowed another upload")
	}
}
