package azureblob

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/znasllc-io/memql/core/librarykey"
)

// librarykey_agreement_test.go -- core/librarykey judges a Library row's
// blobUrl by the (container, object) it parses out of it; this client OPENS
// whatever splitStoredURL parses out. If the two ever disagreed, the check
// would vouch for one object while the read opened another. So this holds the
// two parsers together against the real client, offline.

// TestLibraryKeyNeverAdmitsAnObjectThisClientWouldNotOpen is the safety
// property: whenever librarykey.Check ADMITS a stored URL that this client can
// open, it opens exactly the object the check judged. Bases include an https endpoint with
// the account in its path -- a shape MemQL does not run -- where the two
// parsers differ; there the check must refuse rather than vouch.
func TestLibraryKeyNeverAdmitsAnObjectThisClientWouldNotOpen(t *testing.T) {
	bases := []string{
		"https://acct.blob.core.windows.net",
		"http://azurite:10000/devstoreaccount1",
		"http://127.0.0.1:10000/devstoreaccount1",
		"https://emulator.example/devstoreaccount1",
	}
	objects := []string{
		"library/u-1/f-1/notes.md",
		"library/u-1/f-1/k-9/space name%2F#?.tar",
		"library/u-1/f-1/literal%252F.txt",
		"library/u-1/../u-2/f-1/payroll.csv",
		"library/u-2/f-1/payroll.csv",
		"memql-library/library/u-1/f-1/x",
	}
	for _, base := range bases {
		client, err := azblob.NewClientWithNoCredential(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := &AzureBlobUploader{client: client}
		for _, object := range objects {
			bc, err := u.blockClient("memql-library", object)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := storedBlobURL(bc.URL())
			if err != nil {
				t.Fatal(err)
			}
			refs := []string{
				bc.URL(), canonical,
				// The same container and object on a FOREIGN host, which the
				// client still opens against its own account.
				"https://foreign.example/memql-library/" + object,
				"http://foreign.example:10000/devstoreaccount1/memql-library/" + object,
			}
			for _, ref := range refs {
				if librarykey.Check(ref, "u-1", "memql-library") != nil {
					continue
				}
				// A URL the client cannot open at all reads nothing, so an
				// admission there costs nothing. One it CAN open must be
				// the object the check judged.
				container, opened, ok := u.splitStoredURL(ref)
				judgedContainer, judged, _ := librarykey.Split(ref)
				if ok && (container != judgedContainer || opened != judged) {
					t.Errorf("base %s: librarykey admitted %q as %s/%s, but this client opens %s/%s",
						base, ref, judgedContainer, judged, container, opened)
				}
			}
		}
	}
}

// TestLibraryKeyAdmitsTheOwnersOwnKeysOnTheEndpointsMemQLRuns is the other
// half: the check must not 404 a person's own file on the cloud endpoint or on
// local Azurite, whatever the name escapes to.
func TestLibraryKeyAdmitsTheOwnersOwnKeysOnTheEndpointsMemQLRuns(t *testing.T) {
	for _, base := range []string{"https://acct.blob.core.windows.net", "http://azurite:10000/devstoreaccount1"} {
		client, err := azblob.NewClientWithNoCredential(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := &AzureBlobUploader{client: client}
		for _, object := range []string{"library/u-1/f-1/notes.md", "library/u-1/f-1/k-9/space name%2F#?.tar"} {
			bc, err := u.blockClient("memql-library", object)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := storedBlobURL(bc.URL())
			if err != nil {
				t.Fatal(err)
			}
			if err := librarykey.Check(stored, "v1:identity:user:u-1", "memql-library"); err != nil {
				t.Errorf("base %s: the owner's own key %q was refused: %v", base, stored, err)
			}
		}
	}
}
