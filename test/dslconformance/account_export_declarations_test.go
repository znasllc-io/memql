package dslconformance

// account_export_declarations_test.go -- pins the three declarations an
// account export reads off the shipped tree (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section C):
// which kinds stay in the cluster, which fields name bytes, and which fields
// name a secret held elsewhere.
//
// The sets are compared EXACTLY against what the loaded registry reports, so a
// concept that gains one of these declarations later fails here until the list
// is updated on purpose: an export that silently carried (or silently dropped)
// a new kind is the failure this closes.

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/integrations/accounts"
)

// stayingKinds is every kind that declares `stays`, with its reason.
var stayingKinds = map[string]string{
	"v1:platform:siteAccessSession": "signin",
	"v1:platform:sitePreviewGrant":  "signin",
	"v1:identity:invitation":        "signin",
	"v1:library:uploadSession":      "transit",
	"v1:platform:outboxEntry":       "transit",
	"v1:email:connectionState":      "transit",
	"v1:email:sendOperation":        "transit",
	"v1:workbench:workspace":        "machine",
	"v1:workbench:executionBinding": "machine",
	"v1:library:watchedFolder":      "machine",
}

func TestTheKindsThatStay(t *testing.T) {
	got := map[string]string{}
	for _, c := range loadedConcepts(t) {
		if c == nil {
			continue
		}
		if r := c.Stays(); r != "" {
			got[c.Name] = r
		}
	}
	if !reflect.DeepEqual(got, stayingKinds) {
		t.Errorf("kinds that declare stays differ from the expected set.\n got: %v\nwant: %v", got, stayingKinds)
	}
}

// A kind that travels is written into a file anyone can open, so a field the
// engine seals as a secret on it would leave with the rows. The export removes
// such values at every depth, and this gate keeps the engine's own tree from
// relying on it. It asks the export's own walk (accounts.SecretPaths), which
// reads a secret in an object block, an array's objects, a map's values or a
// union branch as the engine's redaction does, not only a top-level field.
func TestNoKindThatTravelsDeclaresASecretField(t *testing.T) {
	for _, c := range loadedConcepts(t) {
		if c == nil || !c.Travels() {
			continue
		}
		if s := accounts.SecretPaths(c); len(s) > 0 {
			t.Errorf("%s travels in an account export and declares @secret values at %v: mark the kind `stays` or name the secret with @secretName", c.Name, s)
		}
	}
}

func TestBytesAndSecretNameFields(t *testing.T) {
	wantBytes := []string{
		"v1:library:file.blobUrl tree=false counted=false under library/{ownerUserId}",
		"v1:library:fileVersion.blobUrl tree=false counted=false under library/{ownerUserId}",
		"v1:common:attachment.blobUrl tree=false counted=true under spaces",
		"v1:platform:site.bundleRef tree=true counted=false under sites/{id}",
		"v1:platform:site.candidateRef tree=true counted=false under sites/{id}",
		"v1:platform:packageDeployment.snapshotArtifactId tree=false counted=true under packages/snapshots/{packageId}",
	}
	wantSecretNames := []string{
		"v1:shopify:store.adminTokenRef",
		"v1:shopify:store.storefrontTokenRef",
		"v1:shopify:store.webhookSecretRef",
		"v1:pipelines:pipeline.secretNames",
		"v1:pipelines:channel.secretRef",
	}
	var gotBytes, gotNames []string
	for _, c := range loadedConcepts(t) {
		if c == nil {
			continue
		}
		for _, f := range c.BytesFields() {
			// Both flags, so a field that is a tree AND counted never reads
			// like one that is only a tree.
			gotBytes = append(gotBytes, fmt.Sprintf("%s.%s tree=%t counted=%t under %s", c.Name, f.Name, f.Tree, f.Counted, f.Under))
		}
		for _, f := range c.SecretNameFields() {
			gotNames = append(gotNames, c.Name+"."+f)
		}
	}
	for _, s := range [][]string{wantBytes, wantSecretNames, gotBytes, gotNames} {
		sort.Strings(s)
	}
	if !reflect.DeepEqual(gotBytes, wantBytes) {
		t.Errorf("@bytes fields differ.\n got: %v\nwant: %v", gotBytes, wantBytes)
	}
	if !reflect.DeepEqual(gotNames, wantSecretNames) {
		t.Errorf("@secretName fields differ.\n got: %v\nwant: %v", gotNames, wantSecretNames)
	}
}
