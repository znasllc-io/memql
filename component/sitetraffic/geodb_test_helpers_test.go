package sitetraffic

import (
	"bytes"
	"net"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// writeCountryDB builds a tiny MMDB in memory, mapping each CIDR to an ISO
// country code. dbType is the database_type the file declares (the reader
// refuses a file that is not a country database). Shared by the loader tests
// and the monthly-download tests so neither needs a real database file.
func writeCountryDB(t *testing.T, dbType string, entries map[string]string) []byte {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: dbType, RecordSize: 24, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatalf("mmdbwriter.New: %v", err)
	}
	for cidr, code := range entries {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", cidr, err)
		}
		rec := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(code)}}
		if err := w.Insert(n, rec); err != nil {
			t.Fatalf("Insert(%q): %v", cidr, err)
		}
	}
	var buf bytes.Buffer
	if _, err := w.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return buf.Bytes()
}
