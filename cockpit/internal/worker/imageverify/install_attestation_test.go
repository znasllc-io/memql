package imageverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestCandidateAttestationUsesFreshInstallationAndClosedValidatorWithoutWrites(t *testing.T) {
	for _, kind := range []string{"valid", "missing fixture", "failed fixture", "changed host", "changed installation", "duplicate field", "oversize", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			fs := serviceFilesFixture(t, c)
			host, q, now := qualificationFixture(c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "missing fixture":
				q.Fixtures = q.Fixtures[1:]
			case "failed fixture":
				q.Fixtures[0].Result = "failed"
			case "changed host":
				host.KernelRelease += "-changed"
			case "changed installation":
				fs.put(c.Helper.Path, []byte("changed executable"), 0755)
			case "cancelled":
				cancel()
			}
			raw, _ := json.Marshal(q)
			if kind == "duplicate field" {
				raw = append([]byte(`{"schemaVersion":1,`), raw[1:]...)
			} else if kind == "oversize" {
				raw = []byte(strings.Repeat("x", serviceConfigMaxBytes+2))
			}
			input := bytes.NewReader(raw)
			calls := 0
			got, err := validateServiceAttestationObservation(ctx, fs, c, input, now, func(context.Context, ServiceConfig) (ServiceHostFacts, error) {
				calls++
				return host, nil
			})
			if kind == "valid" {
				hash := sha256.Sum256(raw)
				if err != nil || !got.Valid || got.AttestationSHA256 != hex.EncodeToString(hash[:]) || got.InstallationSHA256 != q.InstallationSHA256 || got.SchemaVersion != 1 || calls != 1 {
					t.Fatalf("candidate validation %+v calls=%d: %v", got, calls, err)
				}
			} else if err == nil || got.Valid {
				t.Fatalf("invalid candidate accepted: %+v", got)
			}
			if (kind == "changed installation" || kind == "cancelled" || kind == "oversize") && calls != 0 {
				t.Fatal("host metadata was read before bounded immutable validation")
			}
			if kind == "oversize" && input.Len() != 1 {
				t.Fatal("candidate read exceeded the configured bound")
			}
			if _, ok := fs.info[ServiceQualificationPath]; ok {
				t.Fatal("read-only candidate validation created an attestation")
			}
		})
	}
}
