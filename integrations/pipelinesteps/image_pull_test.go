package pipelinesteps

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func registryConfig() string {
	return `{"auths":{"registry.example.com":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("rehearsal:"+plantedDeploy)) + `"}}}`
}

func TestPrivateImageCredentialOnlyReachesKubeletAndOwnedSecret(t *testing.T) {
	run := testRun()
	run.ImagePullSecret = "REGISTRY_AUTH"
	run.Secrets[run.ImagePullSecret] = registryConfig()
	job := mustBuild(t, testConfig(), run)
	if refs := job.Spec.Template.Spec.ImagePullSecrets; len(refs) != 1 || refs[0].Name != testSecretName {
		t.Fatalf("pull references: %v", refs)
	}
	secret := BuildSecret(testConfig(), run, testJobName, plantedClone)
	if secret.Type != "kubernetes.io/dockerconfigjson" || string(secret.Data[".dockerconfigjson"]) != registryConfig() || string(secret.Data[gitTokenKey]) != plantedClone {
		t.Fatal("missing scoped image/clone credentials")
	}
	if _, exists := secret.Data[run.ImagePullSecret]; exists {
		t.Fatal("pull credential also exposed as env key")
	}
	b, _ := json.Marshal(job)
	if strings.Contains(string(b), "REGISTRY_AUTH") || strings.Contains(string(b), ".dockerconfigjson") || strings.Contains(string(b), plantedDeploy) {
		t.Fatal("credential material reached Job JSON")
	}
	if strings.Contains(fmt.Sprintf("%+v", run), registryConfig()) {
		t.Fatal("credential logged by formatting")
	}
	masked := pl.MaskSecrets(plantedDeploy+" "+registryConfig(), secretValues(run))
	if strings.Contains(masked, plantedDeploy) || strings.Contains(masked, registryConfig()) {
		t.Fatal("decoded credential not redacted")
	}
	// The other replica rebuilds the same per-attempt Secret and Job; no
	// existing cluster Secret name or service-account-wide access is accepted.
	peer := stepRunFor(pl.StepRequest{Step: pl.Step{Image: run.Image, ImagePullSecret: run.ImagePullSecret}, Secrets: run.Secrets}, 600, pl.CodeStepTimeout)
	if peer.ImagePullSecret != run.ImagePullSecret || peer.Secrets[run.ImagePullSecret] != registryConfig() || peer.Env[run.ImagePullSecret] != "" {
		t.Fatal("forward lost credential separation")
	}
}

func TestPrivateImageRefusesInvalidCredentialsBeforeCreatingResources(t *testing.T) {
	for _, value := range []string{
		"", "not JSON", `{}`, `{"auths":{}}`, `{"auths":{"registry.example.com":{"auth":"invalid"}}}`,
		`{"auths":{"registry.example.com":{"username":"user","password":""}}}`,
		`{"auths":{"other.example.com":{"username":"user","password":"hidden"}}}`,
		`{"auths":{"*.example.com":{"username":"user","password":"hidden"}}}`,
		`{"auths":{"registry.example.com/path":{"username":"user","password":"hidden"}}}`,
		`{"auths":{"registry.example.com":{"username":"user","password":"hidden"}},"credsStore":"external"}`,
		registryConfig() + ` {}`, strings.Repeat("x", (64<<10)+1),
	} {
		run := testRun()
		run.ImagePullSecret = "REGISTRY_AUTH"
		run.Secrets[run.ImagePullSecret] = value
		if job, err := BuildJob(testConfig(), run, testJobName); err == nil || job.Kind != "" || strings.Contains(err.Error(), "hidden") {
			t.Fatalf("unsafe credential accepted or exposed: %v", err)
		}
	}
	run := testRun()
	run.ImagePullSecret = "REGISTRY_AUTH"
	run.Secrets[run.ImagePullSecret] = registryConfig()
	run.Env[run.ImagePullSecret] = registryConfig()
	if _, err := BuildJob(testConfig(), run, testJobName); err == nil {
		t.Fatal("credential in command environment accepted")
	}
	req := exRequest()
	req.Step.Placement = pl.PlacementFleet
	req.Step.ImagePullSecret = "REGISTRY_AUTH"
	if _, refused := refuseStep(req); !refused {
		t.Fatal("fleet path accepted unsupported private image credentials")
	}
}
