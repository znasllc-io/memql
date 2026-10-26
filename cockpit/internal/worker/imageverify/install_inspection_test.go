package imageverify

import (
	"context"
	"errors"
	"testing"
)

func TestInstallationObservationValidatesBeforeReadingAndNeverQualifies(t *testing.T) {
	for _, kind := range []string{"valid without attestation", "changed executable", "changed library", "changed unit", "cancelled", "host read failure"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			fs := serviceFilesFixture(t, c)
			host, _, _ := qualificationFixture(c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			switch kind {
			case "changed executable":
				fs.put(c.Helper.Path, []byte("changed"), 0755)
			case "changed library":
				fs.put("/usr/lib/libfixture.so.1", []byte("changed"), 0644)
			case "changed unit":
				fs.put(ServiceUnitPath, []byte("changed"), 0644)
			case "cancelled":
				cancel()
			}
			got, err := inspectServiceInstallationObservation(ctx, fs, c, func(context.Context, ServiceConfig) (ServiceHostFacts, error) {
				calls++
				if kind == "host read failure" {
					return ServiceHostFacts{}, errors.New("host unavailable")
				}
				return host, nil
			})
			if kind == "valid without attestation" {
				if err != nil || calls != 1 || got.Qualified || got.Host != host || !serviceHash.MatchString(got.InstallationSHA256) {
					t.Fatalf("invalid observation %+v calls=%d err=%v", got, calls, err)
				}
			} else {
				if err == nil {
					t.Fatal("missing evidence accepted")
				}
				if kind != "host read failure" && calls != 0 {
					t.Fatal("metadata command reached before immutable verification")
				}
			}
			if _, ok := fs.info[ServiceQualificationPath]; ok {
				t.Fatal("observation created an attestation")
			}
		})
	}
}
