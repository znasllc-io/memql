package release

import (
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestNPMTargetFreezesEveryPublishingChoice(t *testing.T) {
	base := candidateNPMTarget{ID: "sdk", Component: "sdk", Artifact: "archive", Origin: "https://npm.example", PackageName: "@acme/sdk", Version: "1.2.3", DistTag: "latest", CredentialSecret: "NPM_TOKEN"}
	_, _, digest, err := base.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: base.ID, TargetDigest: digest, Component: base.Component, Artifact: base.Artifact, Operation: "publish"}
	r := candidateTargetReader{npm: map[string]candidateNPMTarget{base.ID: base}}
	if _, _, err := r.resolveNPM(ownerCtx(), d); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*candidateNPMTarget){
		func(t *candidateNPMTarget) { t.Origin = "https://another.example" },
		func(t *candidateNPMTarget) { t.PackageName = "@acme/other" },
		func(t *candidateNPMTarget) { t.Version = "1.2.4" },
		func(t *candidateNPMTarget) { t.DistTag = "preview" },
		func(t *candidateNPMTarget) { t.CredentialSecret = "OTHER_TOKEN" },
		func(t *candidateNPMTarget) { t.DownloadOrigins = []string{"https://cdn.example"} },
	} {
		v := base
		change(&v)
		r.npm[base.ID] = v
		if _, _, err := r.resolveNPM(ownerCtx(), d); err == nil {
			t.Fatal("changed destination reused approval", v)
		}
	}
	r.npm[base.ID] = base
	r.files = map[string]candidateFileTarget{base.ID: {ID: base.ID}}
	if _, _, err := r.resolveNPM(ownerCtx(), d); err == nil {
		t.Fatal("ambiguous destination accepted")
	}
	if err := (candidatePublicationTarget{NPM: &base, Registry: &candidateRegistryTarget{}}).validate(d); err == nil {
		t.Fatal("two frozen targets accepted")
	}
	a := base
	b := base
	a.Origin = "https://NPM.example:443/"
	b.Origin = "https://npm.example"
	if npmVersionKey(a) != npmVersionKey(b) {
		t.Fatal("origin alias evades durable version reservation")
	}
	bad := base
	bad.CredentialSecret = strings.Repeat("X", 65)
	if _, _, _, err := bad.snapshot(); err == nil {
		t.Fatal("unbounded credential reference accepted")
	}
}
