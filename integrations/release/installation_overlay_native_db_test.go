package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// The producer and reader are different engines over a disposable database.
// Real journal receipts replace the publication fixture's synthetic evidence;
// only the external artifact/registry/GitHub transports remain local fixtures.
func overlayNativeJournal(t *testing.T, producer, reader *memql.MemQLEngine, p *candidatePreparer, c *pl.ReleaseCandidate, library *candidateLibraryFixture) {
	t.Helper()
	run := assemblyCompletedRun(t, producer, c.Components[0].Repository, c.Components[0].Commit, "full", "", []string{library.receipt.IntentID})
	evidence := candidateEvidenceReader{engine: reader}
	library.receipt.WorkRunID, library.receipt.StepKey = run, "build.image"
	c.Evidence = nil
	for _, key := range []string{"build.image", "test.full"} {
		actual, err := evidence.read(ownerCtx(), run, key)
		require.NoError(t, err)
		c.Evidence = append(c.Evidence, pl.ReleaseEvidence{Name: key, Component: "engine", WorkRunID: run, StepKey: key, Attempt: actual.Attempt, ReceiptID: actual.ReceiptID, ReceiptDigest: actual.ReceiptDigest, DefinitionDigest: actual.DefinitionDigest, ArtifactIntentIDs: actual.ArtifactIntentIDs})
		if key == "build.image" {
			library.receipt.Attempt = actual.Attempt
			c.Components[0].Artifacts[0].Receipt = pl.ReleaseReceiptReference{WorkRunID: run, StepKey: key, Attempt: actual.Attempt, IntentID: library.receipt.IntentID, ReceiptDigest: actual.ReceiptDigest, DefinitionDigest: actual.DefinitionDigest}
		}
	}
	p.evidence = evidence
}

func overlayNativeJournalFixture(t *testing.T, combined ...bool) (*overlayScope, *overlayHub, *memql.MemQLEngine, string) {
	t.Helper()
	s, h := overlayFixture(t)
	producer, reader := assemblyEngine(t, s.db), assemblyEngine(t, s.db)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trust := map[string]ed25519.PublicKey{"fixture-key": pub}
	registryServer := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(registryServer.Close)
	p, c, library, _ := candidatePublicationFixture(t, s.db, registryServer.URL)
	overlayNativeJournal(t, producer, reader, p, &c, library)
	files, fc, _ := candidateDraftWorkflowFixture(t, s.db)
	if len(combined) != 0 && combined[0] {
		// Independent source artifacts have different durable intent identities.
		// Both reused single-artifact helpers otherwise choose the same fixture ID.
		files.library.(*candidateLibraryFixture).receipt.IntentID = strings.Repeat("e", 64)
		fc.Components[0].Artifacts[0].Receipt.IntentID = strings.Repeat("e", 64)
	}
	overlayNativeJournal(t, producer, reader, files, &fc, files.library.(*candidateLibraryFixture))
	companionArtifact := "image"
	if len(combined) != 0 && combined[0] {
		companionArtifact = "docs"
		fileTarget := files.targetReader.files["registry"]
		fileTarget.ID, fileTarget.Artifact = "files", companionArtifact
		_, _, digest, err := fileTarget.snapshot()
		require.NoError(t, err)
		fc.Components[0].Artifacts[0].Name = companionArtifact
		fc.Destinations[0].TargetID, fc.Destinations[0].Artifact, fc.Destinations[0].TargetDigest = "files", companionArtifact, digest
		for n := range fc.Evidence {
			fc.Evidence[n].Name = "files." + fc.Evidence[n].Name
		}
		c.Components[0].Artifacts = append(c.Components[0].Artifacts, fc.Components[0].Artifacts...)
		c.Evidence = append(c.Evidence, fc.Evidence...)
		c.Destinations = append(c.Destinations, fc.Destinations...)
		p.library = overlayCombinedLibrary{library, files.library.(*candidateLibraryFixture)}
		p.targetReader.files = map[string]candidateFileTarget{"files": fileTarget}
		p.targetReader.secret = files.targetReader.secret
	}
	primary := preparePublication(t, s.db, p, c)
	_, err = p.publish(ownerCtx(), primary)
	require.NoError(t, err)
	var companion candidatePublishRequest
	if companionArtifact == "docs" {
		files = p
		companion = primary
		companion.TargetID, companion.Artifact = "files", companionArtifact
	} else {
		companion = preparePublication(t, s.db, files, fc)
	}
	_, err = files.draft(ownerCtx(), draftRequest(companion), false)
	require.NoError(t, err)
	_, err = files.publish(ownerCtx(), companion)
	require.NoError(t, err)
	_, err = files.draft(ownerCtx(), draftRequest(companion), true)
	require.NoError(t, err)
	_, err = files.ledger.sealCatalog(ownerCtx(), companion.CandidateID, companion.ApprovalID, "fixture", "fixture-key", key, trust)
	require.NoError(t, err)
	_, err = p.ledger.sealCatalog(ownerCtx(), primary.CandidateID, primary.ApprovalID, "fixture", "fixture-key", key, trust)
	require.NoError(t, err)
	_, verified, err := readCatalog(ownerCtx(), s.db, primary.CandidateID, "fixture", trust)
	require.NoError(t, err)
	target := p.targetReader.targets["registry"]
	cfg := s.configuration
	cfg.FormatVersion, cfg.Path, cfg.Images = 3, "", nil
	cfg.NativeEvidence = &overlayNativeConfiguration{Component: "engine", CompanionComponent: "engine", CompanionArtifact: companionArtifact, Images: []overlayImage{{Name: "bff", Component: "engine", Artifact: "image", Repository: strings.TrimPrefix(target.Origin, "https://") + "/" + target.Repository}}}
	cfg.Files = []overlayFile{
		{Path: "clients/docs-release.json", Codec: "jsonScalars", JSONScalars: []overlayJSONScalar{{"/version", "componentVersion"}, {"/sha256", "companionSHA256"}, {"/size", "companionSize"}}},
		{Path: "deploy/notifications/config.json", Codec: "jsonScalars", JSONScalars: []overlayJSONScalar{{"/version", "componentTag"}}},
		{Path: "deploy/releases/engine-{version}-manifest.json", Codec: "nativeManifest"},
	}
	h.beforeFiles = map[string]string{"clients/docs-release.json": `{"version":"old","sha256":"old","size":1,"untouched":true}`, "deploy/notifications/config.json": `{"version":"old","channels":["unchanged"]}`}
	h.expectedCreates = map[string]bool{"deploy/releases/engine-0.25.0-manifest.json": true}
	configBytes, err := json.Marshal(cfg)
	require.NoError(t, err)
	catalogBytes, err := json.Marshal(catalogConfiguration{FormatVersion: 1, Publisher: "fixture", PublicKeys: map[string]string{"fixture-key": base64.StdEncoding.EncodeToString(pub)}})
	require.NoError(t, err)
	s.integration.resolver.systemVariable = func(ctx context.Context, name string) (string, error) {
		require.True(t, memql.FreshReadFromContext(ctx))
		switch name {
		case InstallationOverlayVariable:
			return string(configBytes), nil
		case ReleaseCatalogConfigurationVariable:
			return string(catalogBytes), nil
		}
		return "", errors.New("unapproved variable read")
	}
	s.integration.resolver.systemSecret = func(context.Context, string) (string, error) {
		t.Fatal("overlay requested signing/private credentials")
		return "", errors.New("forbidden")
	}
	s.integration.store = NewStore(reader)
	s.configuration = cfg
	s.request.CandidateID, s.request.CatalogDigest = primary.CandidateID, verified.Digest()
	s.source.Publication = verified
	return s, h, producer, companion.CandidateID
}

func TestInstallationOverlayNativeJournalAndAbsenceSurviveAnotherReplica(t *testing.T) {
	s, h, _, companion := overlayNativeJournalFixture(t)
	_, err := s.build(ownerCtx(), nil)
	require.ErrorContains(t, err, "selected native")
	_, err = s.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	require.Len(t, s.nativeCompanions, 1)
	require.Equal(t, 2, s.nativePrimary.Runs[0].StepsPassed)
	chosen := s.nativeCompanions[companion]
	_, err = s.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest})
	require.NoError(t, err)
	_, err = s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	saved, err := s.load(ownerCtx(), s.id)
	require.NoError(t, err)
	require.Equal(t, "absent", saved.Record.FilePreconditions[2].State)
	require.Empty(t, saved.Record.files()[2].BeforeSHA256)
	require.JSONEq(t, `{"version":"v0.25.0","channels":["unchanged"]}`, saved.Record.files()[1].Content)
	// A fresh integration/engine has no first-host facts or selected companion.
	peerIntegration := &Integration{resolver: s.integration.resolver, store: NewStore(assemblyEngine(t, s.db)), candidateDB: s.integration.candidateDB}
	peerIntegration.installationOverlay.Store(s.integration.installationOverlay.Load())
	peerIntegration.installation.Store(s.integration.installation.Load())
	peer := &overlayScope{integration: peerIntegration, db: s.db, configuration: s.configuration, request: s.request, owner: s.owner, id: s.id, snapshot: saved.Record.Snapshot, record: &saved.Record, source: s.source}
	_, err = peer.propose(ownerCtx(), nil)
	require.Error(t, err)
	require.Zero(t, h.refs)
	_, err = peer.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest})
	require.NoError(t, err)
	_, err = peer.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	// Both the contents API and complete immutable Git tree must agree on absence.
	h.treeOnlyFile = "deploy/releases/engine-0.25.0-manifest.json"
	_, err = peer.propose(ownerCtx(), nil)
	require.Error(t, err)
	require.Zero(t, h.refs)
	require.Zero(t, h.prs)
	h.treeOnlyFile = ""
	_, err = peer.propose(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.propose(ownerCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	require.Equal(t, saved.Record.files()[2].Content, h.afterFiles["deploy/releases/engine-0.25.0-manifest.json"])
}

func TestInstallationOverlayNativeFactsRefuseForeignOrAlteredEvidence(t *testing.T) {
	s, h, _, companion := overlayNativeJournalFixture(t)
	other := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "other-owner", Role: auth.RoleOwner})
	_, err := s.nativeEvidence(other, nil)
	require.Error(t, err)
	original := s.request.CatalogDigest
	s.request.CatalogDigest = "sha256:" + strings.Repeat("f", 64)
	_, err = s.nativeEvidence(ownerCtx(), nil)
	require.Error(t, err)
	s.request.CatalogDigest = original
	_, err = s.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	chosen := s.nativeCompanions[companion]
	_, err = s.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": "sha256:" + strings.Repeat("f", 64)})
	require.Error(t, err)
	_, err = s.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest, "approved": true})
	require.Error(t, err)
	_, err = s.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest})
	require.NoError(t, err)
	_, err = s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	saved, err := s.load(ownerCtx(), s.id)
	require.NoError(t, err)
	peer := &overlayScope{integration: s.integration, db: s.db, configuration: s.configuration, request: s.request, owner: s.owner, id: s.id, snapshot: saved.Record.Snapshot, record: &saved.Record, source: s.source}
	// A valid signature alone cannot replace the original same-owner approval.
	var approvedBy string
	err = s.db.QueryRow(`SELECT approved_by FROM release_candidate_approvals WHERE candidate_id=$1`, companion).Scan(&approvedBy)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE release_candidate_approvals SET approved_by='other-owner' WHERE candidate_id=$1`, companion)
	require.NoError(t, err)
	_, err = peer.nativeEvidence(ownerCtx(), nil)
	require.Error(t, err)
	_, err = s.db.Exec(`UPDATE release_candidate_approvals SET approved_by=$2 WHERE candidate_id=$1`, companion, approvedBy)
	require.NoError(t, err)
	_, err = peer.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	require.Zero(t, h.refs)
	require.Zero(t, h.prs)
	require.Zero(t, h.writes)
}
