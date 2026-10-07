package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func registryTargetFixture() candidateRegistryTarget {
	return candidateRegistryTarget{ID: "registry", Component: "engine", Artifact: "image", Origin: "https://registry.example.com", Repository: "acme/engine",
		Username: "release", PasswordSecret: "REGISTRY_PASSWORD", TokenEndpoint: "https://registry.example.com/token", TokenService: "registry.example.com",
		BlobDownloadOrigins: []string{"https://objects-b.example.com", "https://objects-a.example.com"}}
}

func TestCandidateRegistryIdentityBindsEveryAuthoritySetting(t *testing.T) {
	base := registryTargetFixture()
	_, _, digest, err := base.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*candidateRegistryTarget){
		"identity":             func(t *candidateRegistryTarget) { t.ID = "other" },
		"component":            func(t *candidateRegistryTarget) { t.Component = "cockpit" },
		"artifact":             func(t *candidateRegistryTarget) { t.Artifact = "another" },
		"origin":               func(t *candidateRegistryTarget) { t.Origin = "https://other.example.com" },
		"repository":           func(t *candidateRegistryTarget) { t.Repository = "acme/other" },
		"username":             func(t *candidateRegistryTarget) { t.Username = "other" },
		"credential reference": func(t *candidateRegistryTarget) { t.PasswordSecret = "OTHER_PASSWORD" },
		"token endpoint":       func(t *candidateRegistryTarget) { t.TokenEndpoint = "https://token.example.com/exchange" },
		"token service":        func(t *candidateRegistryTarget) { t.TokenService = "other" },
		"download origins":     func(t *candidateRegistryTarget) { t.BlobDownloadOrigins = []string{"https://different.example.com"} },
		"plaintext permission": func(t *candidateRegistryTarget) { t.AllowLoopbackHTTP = true },
	} {
		t.Run(name, func(t *testing.T) {
			other := registryTargetFixture()
			change(&other)
			_, _, got, err := other.snapshot()
			if err != nil || got == digest {
				t.Fatal("changed authority retained approval identity", err)
			}
		})
	}
	same := registryTargetFixture()
	same.BlobDownloadOrigins[0], same.BlobDownloadOrigins[1] = same.BlobDownloadOrigins[1], same.BlobDownloadOrigins[0]
	owned, target, sameDigest, err := same.snapshot()
	if err != nil || sameDigest != digest {
		t.Fatal("set order changed destination identity", err)
	}
	same.BlobDownloadOrigins[0] = "https://late-change.example.com"
	if owned.BlobDownloadOrigins[0] == same.BlobDownloadOrigins[0] || target.BlobDownloadOrigins[0] == same.BlobDownloadOrigins[0] {
		t.Fatal("target configuration retained mutable input")
	}
}

func TestCandidateRegistryRefusesDriftBeforeCredentials(t *testing.T) {
	config := registryTargetFixture()
	_, _, digest, err := config.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: config.ID, TargetDigest: digest, Component: config.Component, Artifact: config.Artifact, Operation: "publish"}
	r := candidateTargetReader{targets: map[string]candidateRegistryTarget{config.ID: config}, secret: func(context.Context, string) (string, error) {
		t.Fatal("invalid target reached credentials")
		return "", nil
	}}
	if err := r.check(ownerCtx(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.publisher(context.Background(), d); err == nil {
		t.Fatal("unresolved owner constructed publisher")
	}
	for _, change := range []func(*pl.ReleaseDestination){
		func(d *pl.ReleaseDestination) { d.TargetDigest = "sha256:" + strings.Repeat("f", 64) },
		func(d *pl.ReleaseDestination) { d.Component = "other" },
		func(d *pl.ReleaseDestination) { d.Artifact = "other" },
		func(d *pl.ReleaseDestination) { d.Operation = "install" },
	} {
		bad := d
		change(&bad)
		if _, err := r.publisher(ownerCtx(), bad); err == nil {
			t.Fatal("changed destination accepted")
		}
	}
	config.Repository = "acme/replaced"
	r.targets[config.ID] = config
	if _, err := r.publisher(ownerCtx(), d); err == nil {
		t.Fatal("operator configuration drift accepted")
	}
}

func TestCandidateRegistryCredentialRotationPreservesReferenceIdentity(t *testing.T) {
	config := registryTargetFixture()
	_, _, digest, err := config.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: config.ID, TargetDigest: digest, Component: config.Component, Artifact: config.Artifact, Operation: "publish"}
	value := "first-password"
	r := candidateTargetReader{targets: map[string]candidateRegistryTarget{config.ID: config}, secret: func(_ context.Context, name string) (string, error) {
		if name != config.PasswordSecret {
			t.Fatal("wrong credential reference")
		}
		return value, nil
	}}
	for _, rotated := range []string{"first-password", "second-password"} {
		value = rotated
		if _, err := r.publisher(ownerCtx(), d); err != nil {
			t.Fatal("rotation changed approved destination", err)
		}
	}
	r.secret = func(context.Context, string) (string, error) { return "", errors.New("private-token-value") }
	if _, err := r.publisher(ownerCtx(), d); err == nil || strings.Contains(err.Error(), "private-token-value") {
		t.Fatal("credential backend error disclosed", err)
	}
}

func TestCandidateRegistryRefusesUnsafeConfiguration(t *testing.T) {
	for _, change := range []func(*candidateRegistryTarget){
		func(t *candidateRegistryTarget) { t.Origin = "http://remote.example.com"; t.AllowLoopbackHTTP = true },
		func(t *candidateRegistryTarget) { t.Origin = "https://user:password@registry.example.com" },
		func(t *candidateRegistryTarget) { t.PasswordSecret = "" },
		func(t *candidateRegistryTarget) { t.BearerSecret = "BEARER" },
		func(t *candidateRegistryTarget) { t.PasswordSecret = "invalid.reference" },
		func(t *candidateRegistryTarget) { t.BlobDownloadOrigins = []string{"https://*.example.com"} },
		func(t *candidateRegistryTarget) { t.RootCAPEM = "not a certificate" },
	} {
		config := registryTargetFixture()
		change(&config)
		if _, _, _, err := config.snapshot(); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
}
