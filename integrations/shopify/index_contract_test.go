package shopify

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestThinIndexConceptOmitsMerchandising(t *testing.T) {
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(this), "..", "..", "dsl", "shopify", "concepts.memql"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, field := range []string{"image", "price", "inventory", "collection", "variant", "tag"} {
		if strings.Contains(body, "\n  "+field) || strings.Contains(body, "\t"+field) {
			t.Fatalf("concept must not declare merchandising field %q", field)
		}
	}
	for _, need := range []string{"handle", "availableForSale", "present"} {
		if !strings.Contains(body, need) {
			t.Fatalf("concept missing %q", need)
		}
	}
}
