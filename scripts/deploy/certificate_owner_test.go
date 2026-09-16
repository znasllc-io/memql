package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBindScriptsKeepOneCertificateOwnerPerSecret(t *testing.T) {
	for _, name := range []string{"domain", "account"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			rendered := filepath.Join(dir, "objects.yaml")
			if name == "domain" {
				// Capture what the real script applies, without reaching a cluster.
				stub := "#!/bin/bash\nfunction main() {\n  if [[ \"$1\" == apply ]]; then cat > \"$CERT_OWNER_TEST_CAPTURE\"; echo configured; fi\n}\nmain \"$@\"\n"
				if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(stub), 0755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("CERT_OWNER_TEST_CAPTURE", rendered)
				env, code := envelopeFrom(t, bindScript, "--hostname=example.com", "--domainId=d1", "--issuer=letsencrypt-prod")
				if code != 0 || !env.OK {
					t.Fatalf("bind failed: %d %+v", code, env.Error)
				}
			} else {
				env, code := envelopeFrom(t, bindDoorScript, "--accountId=a1", "--reservedName=memql.example.com", "--issuer=letsencrypt-prod", "--dryRun=true", "--renderTo="+rendered)
				if code != 0 || !env.OK {
					t.Fatalf("bind failed: %d %+v", code, env.Error)
				}
			}
			raw, err := os.ReadFile(rendered)
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
			owners := map[string]int{}
			var references []string
			for {
				var doc struct {
					Kind     string `yaml:"kind"`
					Metadata struct {
						Annotations map[string]string `yaml:"annotations"`
					} `yaml:"metadata"`
					Spec struct {
						SecretName string `yaml:"secretName"`
						IssuerRef  struct {
							Name string `yaml:"name"`
						} `yaml:"issuerRef"`
						TLS []struct {
							SecretName string `yaml:"secretName"`
						} `yaml:"tls"`
					} `yaml:"spec"`
				}
				err := decoder.Decode(&doc)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if doc.Kind == "Certificate" {
					owners[doc.Spec.SecretName]++
					if doc.Spec.IssuerRef.Name != "letsencrypt-prod" {
						t.Fatal("explicit certificate lost issuer")
					}
				}
				if doc.Kind == "Ingress" {
					for _, key := range []string{"cert-manager.io/cluster-issuer", "cert-manager.io/issuer", "kubernetes.io/tls-acme"} {
						if _, ok := doc.Metadata.Annotations[key]; ok {
							t.Errorf("Ingress enables competing certificate controller through %s", key)
						}
					}
					for _, tls := range doc.Spec.TLS {
						references = append(references, tls.SecretName)
					}
				}
			}
			if len(references) == 0 {
				t.Fatal("no TLS ingress rendered")
			}
			for _, secret := range references {
				if owners[secret] != 1 {
					t.Errorf("TLS Secret %s has %d explicit certificate owners", secret, owners[secret])
				}
			}
		})
	}
}
