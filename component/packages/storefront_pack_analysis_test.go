package packages

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

// storefront_pack_analysis_test.go -- a product's DSL may USE a storefront
// pack's concepts, and the analyzer has to know that.
//
// THE ANALYZER MODELS AN ENGINE. Every published image links the storefront
// packs with no build tag (app/anchor_storefront_packs.go), so a product
// domain that imports one loads perfectly well on a real node. The offline
// analysis linked none of them, so the same tree came back
// "dsl_refuses_boot" with a message telling the author to add an import
// they had already written -- and that is the gate memql-fylo's CI runs.
//
// It is the whole point of promoting the packs into the default build
// (epic memql#5532): a product consumes them. An analyzer that cannot see
// them can only ever refuse the packages the epic exists to make possible.

// packUsingPackage is a product tree whose concept relates to the wholesale
// pack's application, which is exactly what the design record prescribes for
// a client-specific field.
func packUsingPackage() fstest.MapFS {
	p := validPackage()
	p["dsl/acme/concepts.memql"] = file(`use wholesale.concepts.{ application }

@version("1.0.0")
@description("A client's own field on a wholesale application.")
@rowAuthz(owner="ownerUserId", clusterOwner)
concept acmeDetail {
  ownerUserId    string!  @description("The merchant.")
  applicationId  string!  @description("The application this is about.")
  taxId          string   @description("The client's own field.")

  @relationship(type="references", field="applicationId", target=application, direction="outgoing")
}
`)
	return p
}

func TestAProductMayRelateToAStorefrontPacksConcept(t *testing.T) {
	rep, err := Analyze(packUsingPackage(), Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	for _, p := range rep.Problems {
		if p.Code == CodeDslRefusesBoot {
			t.Fatalf("a product domain importing a storefront pack's concept was refused:\n  %s\n\n"+
				"Every published image links the storefront packs with no build tag, so this tree "+
				"loads on a real node. An analyzer that cannot see them refuses exactly the "+
				"packages epic memql#5532 exists to make possible.", p.Message)
		}
	}
	if !rep.OK {
		var codes []string
		for _, p := range rep.Problems {
			codes = append(codes, p.Code)
		}
		t.Fatalf("the package did not analyze clean: %s", strings.Join(codes, ", "))
	}
	_ = context.Background()
}
