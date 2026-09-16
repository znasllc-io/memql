package customdomain

import "testing"

// ingress-shim names its Certificate after tls.secretName. Our explicit
// Certificate has a different name, so enabling the shim produces two renewal
// controllers for the same Secret. Cover both runtime binding paths.
func TestExplicitCertificateDoesNotEnableIngressShim(t *testing.T) {
	req := BindRequest{Hostname: "example.com", DomainID: "d1", Namespace: "memql", Issuer: "letsencrypt-prod", IngressClass: "nginx", Service: "edge", Port: 8085}
	door := DoorBindRequest{AccountID: "a1", ReservedName: "memql.example.com", Namespace: "memql", Issuer: "letsencrypt-prod"}
	cases := map[string][]map[string]any{
		"domain":  {IngressObject(req), CertificateObject(req)},
		"account": append(doorIngressObjects(door, []string{"/healthz"}), DoorCertificateObject(door)),
	}
	for name, objects := range cases {
		t.Run(name, func(t *testing.T) {
			certificates := map[string]int{}
			for _, obj := range objects {
				if obj["kind"] == "Certificate" {
					spec := obj["spec"].(map[string]any)
					certificates[spec["secretName"].(string)]++
					if spec["issuerRef"].(map[string]any)["name"] != "letsencrypt-prod" {
						t.Fatal("explicit certificate lost its issuer")
					}
				}
			}
			for _, obj := range objects {
				if obj["kind"] != "Ingress" {
					continue
				}
				annotations, _ := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
				for _, key := range []string{"cert-manager.io/cluster-issuer", "cert-manager.io/issuer", "kubernetes.io/tls-acme"} {
					if _, ok := annotations[key]; ok {
						t.Errorf("Ingress enables competing certificate controller through %s", key)
					}
				}
				for _, tls := range obj["spec"].(map[string]any)["tls"].([]any) {
					secret := tls.(map[string]any)["secretName"].(string)
					if certificates[secret] != 1 {
						t.Errorf("TLS Secret %s has %d explicit certificate owners", secret, certificates[secret])
					}
				}
			}
		})
	}
}
