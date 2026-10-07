package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func fileTargetFixture() candidateFileTarget {
	return candidateFileTarget{ID: "asset", Component: "engine", Artifact: "archive", APIOrigin: "https://api.github.com", UploadOrigin: "https://uploads.github.com",
		DownloadOrigins: []string{"https://release-assets.githubusercontent.com"}, Repository: "acme/engine", SourceCommit: strings.Repeat("b", 40), Tag: "v0.25.0", ReleaseID: 73, AssetName: "engine.tar", CredentialSecret: "RELEASE_TOKEN"}
}

func TestCandidateFileTargetBindsAuthorityAndSourceBeforeCredentials(t *testing.T) {
	target := fileTargetFixture()
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"}
	for name, change := range map[string]func(*candidateFileTarget){
		"draft":                func(t *candidateFileTarget) { t.ReleaseID++ },
		"tag":                  func(t *candidateFileTarget) { t.Tag = "v0.25.1" },
		"commit":               func(t *candidateFileTarget) { t.SourceCommit = strings.Repeat("c", 40) },
		"repository":           func(t *candidateFileTarget) { t.Repository = "acme/another" },
		"filename":             func(t *candidateFileTarget) { t.AssetName = "other.tar" },
		"api":                  func(t *candidateFileTarget) { t.APIOrigin = "https://api.example.com" },
		"upload":               func(t *candidateFileTarget) { t.UploadOrigin = "https://uploads.example.com" },
		"download":             func(t *candidateFileTarget) { t.DownloadOrigins = []string{"https://cdn.example.com"} },
		"credential":           func(t *candidateFileTarget) { t.CredentialSecret = "OTHER_TOKEN" },
		"transport permission": func(t *candidateFileTarget) { t.AllowLoopbackHTTP = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := target
			change(&changed)
			_, _, next, err := changed.snapshot()
			if err != nil || next == digest {
				t.Fatal("changed authority reused candidate identity", err)
			}
			r := candidateTargetReader{files: map[string]candidateFileTarget{target.ID: changed}, secret: func(context.Context, string) (string, error) {
				t.Fatal("target drift reached credentials")
				return "", nil
			}}
			if _, err := r.filePublisher(ownerCtx(), d); err == nil {
				t.Fatal("changed target was admitted")
			}
		})
	}
	r := candidateTargetReader{files: map[string]candidateFileTarget{target.ID: target}}
	c := pl.ReleaseComponent{Name: "engine", Repository: target.Repository, Commit: target.SourceCommit}
	a := pl.ReleaseArtifact{Name: "archive", Kind: "file", Size: 10}
	if err := r.checkArtifact(ownerCtx(), d, c, a); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"repository", "commit", "kind", "size", "duplicate"} {
		other, artifact := c, a
		reader := r
		switch failure {
		case "repository":
			other.Repository = "acme/other"
		case "commit":
			other.Commit = strings.Repeat("d", 40)
		case "kind":
			artifact.Kind = "oci"
		case "size":
			artifact.Size = 2 << 30
		case "duplicate":
			reader.targets = map[string]candidateRegistryTarget{target.ID: {ID: target.ID}}
		}
		if err := reader.checkArtifact(ownerCtx(), d, other, artifact); err == nil {
			t.Fatal("invalid file source or destination admitted", failure)
		}
	}
}

func TestCandidateFileOnlyConfigurationAndGlobalTargetUniqueness(t *testing.T) {
	for _, failure := range []string{"valid", "duplicate file", "duplicate cross format", "repository", "plaintext", "missing credential"} {
		t.Run(failure, func(t *testing.T) {
			target := fileTargetFixture()
			cfg := candidateOperatorConfiguration{FormatVersion: 1, Sources: []candidateVersionSource{{Component: "engine", Repository: "acme/engine", Path: "VERSION"}}, ReleaseAssets: []candidateFileTarget{target}}
			switch failure {
			case "duplicate file":
				cfg.ReleaseAssets = append(cfg.ReleaseAssets, target)
			case "duplicate cross format":
				cfg.Registries = []candidateRegistryTarget{{ID: target.ID, Component: "engine", Artifact: "image", Origin: "https://registry.example.com", Repository: "acme/engine"}}
			case "repository":
				cfg.ReleaseAssets[0].Repository = "acme/another"
			case "missing credential":
				cfg.ReleaseAssets[0].CredentialSecret = ""
			}
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(body)
			if failure == "plaintext" {
				raw = strings.Replace(raw, `"credentialSecret":`, `"token":"private-do-not-disclose","credentialSecret":`, 1)
			}
			i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{systemVariable: func(context.Context, string) (string, error) { return raw, nil }, systemSecret: func(context.Context, string) (string, error) {
				t.Fatal("configuration exposed credentials")
				return "", nil
			}})
			if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { t.Fatal("configuration touched SQL"); return nil }, Library: &candidateLibraryFixture{t: t}}); err != nil {
				t.Fatal(err)
			}
			rows, err := i.handleCandidateConfiguration(ownerCtx(), nil, 0)
			if failure == "valid" {
				if err != nil || len(rows) != 1 || !strings.Contains(string(rows[0].Payload), `"releaseId":73`) || !strings.Contains(string(rows[0].Payload), `"kind":"file"`) {
					t.Fatal("file-only target unavailable", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-do-not-disclose") {
				t.Fatal("unsafe configuration accepted or disclosed", err)
			}
		})
	}
}
